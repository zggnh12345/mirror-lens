package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 分片路径的所有出口都必须把名额和在途还干净：
// 成功、响应头校验失败、短读、写盘失败，都不能留下 held / bodies / inflight。
func TestParChunkLeaseLifecycle(t *testing.T) {
	const length = 1 << 20

	cases := []struct {
		name  string
		setup func(t *testing.T, f *parLeaseFixture)
	}{
		{name: "成功"},
		{name: "响应头校验失败", setup: func(t *testing.T, f *parLeaseFixture) { f.mirror.noRange = true }},
		{name: "短读", setup: func(t *testing.T, f *parLeaseFixture) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", length-1, f.total))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(f.body[:length/2])
			}))
			t.Cleanup(srv.Close)
			f.sourceURL = srv.URL
		}},
		{name: "写盘失败", setup: func(t *testing.T, f *parLeaseFixture) { f.closePart = true }},
		{name: "回源失败", setup: func(t *testing.T, f *parLeaseFixture) {
			dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			dead.Close()
			f.sourceURL = dead.URL
		}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newParLeaseFixture(t, int64(i)+1)
			defer f.close()
			if tc.setup != nil {
				tc.setup(t, f)
			}

			part, err := os.CreateTemp(t.TempDir(), "part")
			if err != nil {
				t.Fatal(err)
			}
			if f.closePart {
				part.Close() // 之后写入必然失败
			} else {
				defer part.Close()
			}

			src := parSource{t: fetchTarget{u: mustURL(t, f.sourceURL), c: f.cand}}
			stats := make([]parStat, 1)
			// 失败场景允许返回错误，这里只关心账目。
			_ = f.g.parChunk(context.Background(), f.plan, src, []parSource{src}, 0, 0, length, f.total, part, stats, nil)

			if got := leaseGauge(t, "leases_held", f.id); got != 0 {
				t.Fatalf("leases_held=%v，期望 0（有名额残留）", got)
			}
			if got := leaseGauge(t, "a_bodies_active", f.id); got != 0 {
				t.Fatalf("a_bodies_active=%v，期望 0（Body 未退出）", got)
			}
			if got := leaseGauge(t, "source_inflight", f.id); got != 0 {
				t.Fatalf("source_inflight=%v，期望 0（在途未归还）", got)
			}
			if got := leaseGauge(t, "lease_released_total", f.id); got != 1 {
				t.Fatalf("lease_released_total=%v，期望恰好 1 次", got)
			}
		})
	}
}

type parLeaseFixture struct {
	g         *Gateway
	st        *store.Store
	plan      *adapter.Plan
	cand      *scheduler.Candidate
	sourceURL string
	mirror    *rangeSrv
	body      []byte
	total     int64
	id        int64
	closePart bool
	closers   []func()
}

func newParLeaseFixture(t *testing.T, seq int64) *parLeaseFixture {
	t.Helper()
	g, st, _, body, _, mirror, osrv, msrv := parFixture(t, 4<<20)
	plan, cand := parPlan(t, osrv, body)

	id, err := st.AddSource(store.Source{
		Name: fmt.Sprintf("镜像%d", seq), BaseURL: "http://mirror",
		Status: "usable", Enabled: true, DockerCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxPerSource: 8, Admission: "blocking"}))
	metrics.Enable()
	t.Cleanup(metrics.Disable)

	return &parLeaseFixture{
		g: g, st: st, plan: plan, cand: cand, sourceURL: msrv.URL,
		mirror: mirror, body: body, total: int64(len(body)), id: id,
	}
}

func (f *parLeaseFixture) close() {
	for _, c := range f.closers {
		c()
	}
}

// 两个分片 Body 同时传输时，a_bodies_active 必须真实反映 2，退出后归零。
// 修复前 parChunk 只记账名额、不 Activate，这个值恒为 0。
func TestParChunkBodiesCounted(t *testing.T) {
	const length = 256 << 10
	f := newParLeaseFixture(t, 9)
	defer f.close()

	gate := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", f.total-1, f.total))
		w.WriteHeader(http.StatusPartialContent)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-gate // 两个 Body 都停在读取中
		_, _ = w.Write(f.body)
	}))
	t.Cleanup(slow.Close)

	src := parSource{t: fetchTarget{u: mustURL(t, slow.URL), c: f.cand}}
	part, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats := make([]parStat, 2)
			_ = f.g.parChunk(context.Background(), f.plan, src, []parSource{src}, 0, 0, length, f.total, part, stats, nil)
		}()
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && leaseGauge(t, "a_bodies_active", f.id) != 2 {
		time.Sleep(10 * time.Millisecond)
	}
	got := leaseGauge(t, "a_bodies_active", f.id)
	close(gate)
	wg.Wait()
	if got != 2 {
		t.Fatalf("两个分片同时活跃时 a_bodies_active=%v，期望 2", got)
	}
	if got := leaseGauge(t, "a_bodies_active", f.id); got != 0 {
		t.Fatalf("分片退出后 a_bodies_active=%v，期望 0", got)
	}
	if got := leaseGauge(t, "leases_held", f.id); got != 0 {
		t.Fatalf("分片退出后 leases_held=%v，期望 0", got)
	}
	if got := leaseGauge(t, "source_inflight", f.id); got != 0 {
		t.Fatalf("分片退出后 source_inflight=%v，期望 0", got)
	}
}

// 官方源 fallback（idx=-1）没有 Lease，也必须关闭原始 Body。
func TestParChunkClosesBodyWithoutLease(t *testing.T) {
	const length = 1 << 20
	f := newParLeaseFixture(t, 21)
	defer f.close()

	rc := &trackedBody{r: bytes.NewReader(f.body[:length])}
	old := f.g.cli
	f.g.cli = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Range": []string{fmt.Sprintf("bytes 0-%d/%d", f.total-1, f.total)}},
			Body:       rc,
		}, nil
	})}
	defer func() { f.g.cli = old }()

	src := parSource{t: fetchTarget{u: mustURL(t, "http://mirror.invalid/blob"), c: f.cand}}
	part, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()

	stats := make([]parStat, 1)
	// idx=-1：没有任何 Lease，历史实现此处靠 resp.Body.Close() 兜底。
	if err := f.g.parChunk(context.Background(), f.plan, src, []parSource{src}, -1, 0, length, f.total, part, stats, nil); err != nil {
		t.Fatalf("官方源 fallback 应成功: %v", err)
	}
	if rc.closes != 1 {
		t.Fatalf("无 Lease 路径底层 Body 关闭 %d 次，期望 1", rc.closes)
	}
	if got := leaseGauge(t, "source_inflight", f.id); got != 0 {
		t.Fatalf("source_inflight=%v，期望 0", got)
	}
}

type trackedBody struct {
	r      io.Reader
	closes int
}

func (b *trackedBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *trackedBody) Close() error {
	b.closes++
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Activate 失败时必须关掉上游 Body：不能留下半开连接。
func TestChunkLeaseAttachFailureClosesBody(t *testing.T) {
	f := newParLeaseFixture(t, 22)
	defer f.close()

	l, err := f.g.sched.AcquireLease(context.Background(), f.id, scheduler.KindNormal)
	if err != nil || l == nil {
		t.Fatalf("acquire: %v", err)
	}
	tok := l.Token()
	cl := &chunkLease{l: l, tok: tok, id: f.id, done: func() {}}
	l.Reclaim("测试：让 Activate 必然失败")

	rc := &countingCloser{}
	resp := &http.Response{Body: rc}
	if err := cl.attach(resp); err == nil {
		t.Fatal("Lease 已回收，Activate 应失败")
	}
	if rc.closes != 1 {
		t.Fatalf("Activate 失败后底层 Body 关闭 %d 次，期望 1", rc.closes)
	}
	cl.finish(nil)
	if got := leaseGauge(t, "leases_held", f.id); got != 0 {
		t.Fatalf("leases_held=%v，期望 0", got)
	}
}
