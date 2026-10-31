package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 头部到了、身子断了：对客户端就是一次失败下载，不能给这台源记成功。
// 记成功会刷新 sticky（10 分钟）并清掉熔断，一台「握手快、传输慢」的源
// 就能一直霸占流量。
func TestTruncatedUpstreamCountedAsFailure(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}

	origin := srv(t, 0, "origin-body")
	// 声明 1000 字节，只发 100 字节就断：客户端读到 unexpected EOF。
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 100))
	}))
	defer truncated.Close()

	id := addSchedSource(t, st, "fast-hands", truncated.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	tok := token(t, st)
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	// 小文件走整份缓冲：断在上游、还没写客户端，所以能就地换源。
	if got := rec.Body.String(); got != "origin-body" {
		t.Fatalf("应回落到官方源，实际 %q", got)
	}

	okN, failN, _, err := st.SourceEvents(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if okN != 0 || failN != 1 {
		t.Fatalf("截断的源应记 1 次失败、0 次成功，实际 ok=%d fail=%d", okN, failN)
	}
}

// errReader 模拟上游在声明的长度之前关掉连接。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// 流式路径：头已经发给客户端，上游中途断掉就没法换源。
// 这种「看起来成功、实际断了」的源必须记失败，否则它会一直被粘着。
func TestStreamBodyReportsFailureOnTruncation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}

	id, err := st.AddSource(store.Source{
		Name: "half-pipe", BaseURL: "http://127.0.0.1:1", Status: "usable", Enabled: true, DockerCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	cand := scheduler.Candidate{ID: id, Name: "half-pipe"}
	up, _ := url.Parse("http://127.0.0.1:1/v2/library/alpine/blobs/sha256:abc")

	g := New(st, Options{Cache: cs})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))

	plan := &adapter.Plan{
		Kind:      "docker",
		Upstream:  up,
		CacheKey:  "docker:blob:sha256:abc",
		Cacheable: true,
		TTL:       time.Minute,
	}
	body := io.NopCloser(io.MultiReader(bytes.NewReader(make([]byte, 300<<10)), errReader{}))
	resp := &http.Response{StatusCode: http.StatusOK, ContentLength: 5 << 20, Body: body, Header: http.Header{}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://gw/blob", nil)
	g.streamBody(rec, req, plan, resp, nil, &fetchTarget{u: up, c: &cand}, 5*time.Millisecond)

	if rec.Body.Len() != 300<<10 {
		t.Fatalf("已收的 300KB 应先发给客户端，实际 %d", rec.Body.Len())
	}
	okN, failN, _, err := st.SourceEvents(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if okN != 0 || failN != 1 {
		t.Fatalf("流式中断应记 1 次失败，实际 ok=%d fail=%d", okN, failN)
	}
}

// 正常传完才记成功（对照组，防止上面的修复把所有源都记成失败）。
func TestCompletedUpstreamCountedAsSuccess(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}

	origin := srv(t, 0, "origin-body")
	mirror := srv(t, 0, "mirror-body")
	id := addSchedSource(t, st, "mirror", mirror.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	tok := token(t, st)
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "mirror-body" {
		t.Fatalf("应 200 mirror-body，实际 %d %q", rec.Code, rec.Body.String())
	}
	if okN, failN, _, _ := st.SourceEvents(id, 10); okN != 1 || failN != 0 {
		t.Fatalf("完整传完应记 1 次成功，实际 ok=%d fail=%d", okN, failN)
	}
}

// 客户端自己断开不算源的错：既不该记成功，也不该记失败。
func TestClientGoneNotCounted(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}

	origin := srv(t, 0, "origin-body")
	// 慢慢发，给客户端留出「读到一半就走」的机会。
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 100; i++ {
			if _, err := w.Write(make([]byte, 1000)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer slow.Close()
	id := addSchedSource(t, st, "slow", slow.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	// 带 cancel 的请求：读到一小段就断开。
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://gw/raw/o/r/main/big.bin", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token(t, st))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		g.ServeRaw(rec, req)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后处理函数没退出")
	}

	if okN, failN, _, _ := st.SourceEvents(id, 10); okN != 0 || failN != 0 {
		t.Fatalf("客户端断开不该记成败，实际 ok=%d fail=%d", okN, failN)
	}
}
