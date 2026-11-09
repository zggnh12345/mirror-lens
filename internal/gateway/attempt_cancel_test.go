package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 拨号期间被 Reclaim：attempt ctx 必须立刻取消，分片退出，账目归零。
func TestParChunkAttemptCancelledByReclaim(t *testing.T) {
	const length = 64 << 10
	f := newParLeaseFixture(t, 31)
	defer f.close()

	src := parSource{t: fetchTarget{u: mustURL(t, "http://mirror.invalid/blob"), c: f.cand}}
	f.plan.Upstream = src.t.u // 只留一个 attempt，避免回落到真实上游

	started := make(chan struct{})
	var once sync.Once
	f.g.cli = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}

	part, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()

	done := make(chan error, 1)
	go func() {
		stats := make([]parStat, 1)
		done <- f.g.parChunk(context.Background(), f.plan, src, []parSource{src}, 0, 0, length, f.total, part, stats, nil)
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("请求没有发出去")
	}
	if n := f.g.sched.ReclaimSource(f.id, "test"); n != 1 {
		t.Fatalf("ReclaimSource 回收 %d 个名额，期望 1", n)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("被回收的分片不应返回成功")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Reclaim 后拨号没有被取消")
	}
	assertNoLeak(t, f.id)
}

// Body 读取期间被 Reclaim：读操作必须被打断，随后账目归零。
func TestParChunkBodyReadCancelledByReclaim(t *testing.T) {
	const length = 64 << 10
	f := newParLeaseFixture(t, 32)
	defer f.close()

	src := parSource{t: fetchTarget{u: mustURL(t, "http://mirror.invalid/blob"), c: f.cand}}
	f.plan.Upstream = src.t.u

	started := make(chan struct{})
	var once sync.Once
	f.g.cli = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Range": []string{fmt.Sprintf("bytes 0-%d/%d", f.total-1, f.total)}},
			Body: &ctxBody{ctx: r.Context(), first: f.body[:1024], started: func() {
				once.Do(func() { close(started) })
			}},
		}, nil
	})}

	part, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()

	done := make(chan error, 1)
	go func() {
		stats := make([]parStat, 1)
		done <- f.g.parChunk(context.Background(), f.plan, src, []parSource{src}, 0, 0, length, f.total, part, stats, nil)
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("没有进入 Body 读取")
	}
	f.g.sched.ReclaimSource(f.id, "test")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("读取被取消的 Body 不应算成功")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Body 读取没有被取消")
	}
	assertNoLeak(t, f.id)
}

// 旧 Attempt 失败不能把新 Attempt 一起带走，也不能留下账目。
func TestParChunkRetryUsesFreshAttempt(t *testing.T) {
	const length = 64 << 10
	f := newParLeaseFixture(t, 33)
	defer f.close()

	src := parSource{t: fetchTarget{u: mustURL(t, "http://mirror.invalid/blob"), c: f.cand}}
	f.plan.Upstream = src.t.u
	// 第二个源提供重试机会（不带名额，走无 Lease 分支）。
	alt := parSource{t: fetchTarget{u: mustURL(t, "http://mirror2.invalid/blob")}}

	var mu sync.Mutex
	attempts := 0
	ctxs := []context.Context{}
	f.g.cli = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		n := attempts
		ctxs = append(ctxs, r.Context())
		mu.Unlock()
		if n == 1 {
			// 第一次读取中途报错（模拟断流）
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header:     http.Header{"Content-Range": []string{fmt.Sprintf("bytes 0-%d/%d", f.total-1, f.total)}},
				Body:       io.NopCloser(bytes.NewReader(f.body[:10])),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Range": []string{fmt.Sprintf("bytes 0-%d/%d", f.total-1, f.total)}},
			Body:       io.NopCloser(bytes.NewReader(f.body[:length])),
		}, nil
	})}

	part, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()

	stats := make([]parStat, 2)
	if err := f.g.parChunk(context.Background(), f.plan, src, []parSource{src, alt}, 0, 0, length, f.total, part, stats, nil); err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	mu.Lock()
	n := attempts
	var firstCtx context.Context
	if len(ctxs) > 1 {
		firstCtx = ctxs[0]
	}
	mu.Unlock()
	if n < 2 {
		t.Fatalf("只发了 %d 次请求，期望重试", n)
	}
	if firstCtx != nil && firstCtx.Err() == nil {
		t.Fatal("旧 Attempt 的 Context 在重试后仍存活")
	}
	assertNoLeak(t, f.id)
}

// finish 必须先让 Body 关闭，再取消 Attempt Context：
// 提前 cancel 会让 Transport 丢掉可复用的连接。
func TestParChunkCancelsAfterBodyClose(t *testing.T) {
	cases := []struct {
		name   string
		header func(total int64) string
		status int
	}{
		{name: "正常完成", status: http.StatusPartialContent, header: func(total int64) string {
			return fmt.Sprintf("bytes 0-%d/%d", total-1, total)
		}},
		{name: "响应头校验失败", status: http.StatusOK, header: func(int64) string { return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const length = 32 << 10
			f := newParLeaseFixture(t, 34)
			defer f.close()

			src := parSource{t: fetchTarget{u: mustURL(t, "http://mirror.invalid/blob"), c: f.cand}}
			f.plan.Upstream = src.t.u

			var closedAfterCancel atomic.Bool
			var attemptCtx context.Context
			f.g.cli = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				attemptCtx = r.Context()
				h := http.Header{}
				if v := tc.header(f.total); v != "" {
					h.Set("Content-Range", v)
				}
				return &http.Response{
					StatusCode: tc.status,
					Header:     h,
					Body:       &closeCheckBody{r: bytes.NewReader(f.body[:length]), ctx: r.Context(), seen: &closedAfterCancel},
				}, nil
			})}

			part, err := os.CreateTemp(t.TempDir(), "part")
			if err != nil {
				t.Fatal(err)
			}
			defer part.Close()

			stats := make([]parStat, 1)
			_ = f.g.parChunk(context.Background(), f.plan, src, []parSource{src}, 0, 0, length, f.total, part, stats, nil)
			if closedAfterCancel.Load() {
				t.Fatal("Body.Close 时 Attempt Context 已被取消，连接无法复用")
			}
			// 顺序之外还要确认取消确实发生：只测「Close 时还没取消」会让
			// 「永远不取消」的实现也通过。
			if attemptCtx == nil {
				t.Fatal("没有拿到 Attempt Context")
			}
			if attemptCtx.Err() == nil {
				t.Fatal("分片结束后 Attempt Context 仍未取消")
			}
		})
	}
}

type closeCheckBody struct {
	r    io.Reader
	ctx  context.Context
	seen *atomic.Bool
}

func (b *closeCheckBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *closeCheckBody) Close() error {
	b.seen.Store(b.ctx.Err() != nil)
	return nil
}

func assertNoLeak(t *testing.T, id int64) {
	t.Helper()
	if got := leaseGauge(t, "leases_held", id); got != 0 {
		t.Fatalf("leases_held=%v，期望 0", got)
	}
	if got := leaseGauge(t, "a_bodies_active", id); got != 0 {
		t.Fatalf("a_bodies_active=%v，期望 0", got)
	}
	if got := leaseGauge(t, "source_inflight", id); got != 0 {
		t.Fatalf("source_inflight=%v，期望 0", got)
	}
}

// ctxBody 先吐一小段，然后阻塞到 Attempt Context 结束。
type ctxBody struct {
	ctx     context.Context
	first   []byte
	started func()
	once    sync.Once
}

func (b *ctxBody) Read(p []byte) (int, error) {
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	b.once.Do(b.started)
	<-b.ctx.Done()
	return 0, errors.New("attempt cancelled")
}

func (b *ctxBody) Close() error { return nil }
