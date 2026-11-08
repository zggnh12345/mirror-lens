package gateway

import (
	"context"
	"fmt"
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
