package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// metricScalar 读一个无标签指标的当前值（计数/浮点均可）。
func metricScalar(t *testing.T, name string) float64 {
	t.Helper()
	rec := httptestRecorder()
	line := "#"
	prefix := "mirrorlens_" + name + " "
	for _, l := range strings.Split(rec, "\n") {
		if strings.HasPrefix(l, prefix) {
			line = l
			break
		}
	}
	if line == "#" {
		t.Fatalf("指标 %s 不存在", name)
	}
	f := strings.Fields(line)
	v, err := strconv.ParseFloat(f[len(f)-1], 64)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", line, err)
	}
	return v
}

// 生产代码里 finish 可能被多条出口重复调用：底层 Close 只能执行一次，
// Attempt 计数也只能结算一次。
func TestChunkLeaseFinishIsIdempotent(t *testing.T) {
	f := newParLeaseFixture(t, 41)
	defer f.close()

	beforeActive := metricScalar(t, "attempts_active")
	beforeStarted := metricScalar(t, "attempts_started_total")

	tgt := fetchTarget{u: mustURL(t, "http://mirror.invalid/blob"), c: f.cand}
	cl, err := f.g.openChunkLease(context.Background(), tgt)
	if err != nil || cl == nil {
		t.Fatalf("openChunkLease: %v", err)
	}
	actx, acancel := context.WithCancel(context.Background())
	if !cl.bindAttempt(actx, acancel) {
		t.Fatal("bindAttempt 返回 false")
	}
	body := &countCloseBody{r: strings.NewReader("hello")}
	resp := &http.Response{StatusCode: http.StatusPartialContent, Body: body, Header: http.Header{}}
	if err := cl.attach(resp); err != nil {
		t.Fatalf("attach: %v", err)
	}
	cl.finish(resp.Body)
	cl.finish(resp.Body)

	if n := body.closes.Load(); n != 1 {
		t.Fatalf("底层 Body Close 执行 %d 次，期望 1", n)
	}
	if got := metricScalar(t, "attempts_active"); got != beforeActive {
		t.Fatalf("attempts_active=%v，期望回到基线 %v", got, beforeActive)
	}
	if got := metricScalar(t, "attempts_started_total"); got < beforeStarted+1 {
		t.Fatalf("attempts_started_total=%v，期望至少 %v", got, beforeStarted+1)
	}
	assertNoLeak(t, f.id)
}

// 两个 Attempt 各自持有名额：回收其中一个不能影响另一个。
func TestReclaimCancelsOnlyItsAttempt(t *testing.T) {
	f := newParLeaseFixture(t, 42)
	defer f.close()

	beforeCancelled := metricScalar(t, "attempts_cancelled_total")

	tgt := fetchTarget{u: mustURL(t, "http://mirror.invalid/blob"), c: f.cand}
	cl1, err := f.g.openChunkLease(context.Background(), tgt)
	if err != nil || cl1 == nil {
		t.Fatalf("openChunkLease #1: %v", err)
	}
	cl2, err := f.g.openChunkLease(context.Background(), tgt)
	if err != nil || cl2 == nil {
		t.Fatalf("openChunkLease #2: %v", err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	if !cl1.bindAttempt(ctx1, cancel1) || !cl2.bindAttempt(ctx2, cancel2) {
		t.Fatal("bindAttempt 返回 false")
	}

	cl1.l.Reclaim("test")
	if ctx1.Err() == nil {
		t.Fatal("被回收的 Attempt 没有取消")
	}
	if ctx2.Err() != nil {
		t.Fatal("另一个 Attempt 被误取消")
	}

	cl1.finish(nil)
	cl2.finish(nil)
	if got := metricScalar(t, "attempts_cancelled_total"); got < beforeCancelled+1 {
		t.Fatalf("attempts_cancelled_total=%v，期望至少 %v", got, beforeCancelled+1)
	}
	assertNoLeak(t, f.id)
}

// Body.Close 返回错误时，Lease 仍必须归还。
func TestCloseErrorStillReleasesLease(t *testing.T) {
	f := newParLeaseFixture(t, 43)
	defer f.close()

	tgt := fetchTarget{u: mustURL(t, "http://mirror.invalid/blob"), c: f.cand}
	cl, err := f.g.openChunkLease(context.Background(), tgt)
	if err != nil || cl == nil {
		t.Fatalf("openChunkLease: %v", err)
	}
	resp := &http.Response{
		StatusCode: http.StatusPartialContent,
		Header:     http.Header{},
		Body:       errCloseBody{err: errors.New("boom")},
	}
	if err := cl.attach(resp); err != nil {
		t.Fatalf("attach: %v", err)
	}
	cl.finish(resp.Body)
	assertNoLeak(t, f.id)
}

type countCloseBody struct {
	r      io.Reader
	closes atomic.Int32
}

func (b *countCloseBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *countCloseBody) Close() error {
	b.closes.Add(1)
	return nil
}

type errCloseBody struct{ err error }

func (b errCloseBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b errCloseBody) Close() error             { return b.err }

// httptestRecorder 渲染一次 /metrics 文本。
func httptestRecorder() string {
	rec := &strings.Builder{}
	req, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	rw := &textWriter{b: rec}
	metrics.Get().Handler().ServeHTTP(rw, req)
	return rec.String()
}

type textWriter struct{ b *strings.Builder }

func (w *textWriter) Header() http.Header         { return http.Header{} }
func (w *textWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
func (w *textWriter) WriteHeader(int)             {}
