package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/ghapi"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 429 必须原样透出并带 Retry-After：折成 502 会让客户端立刻重试，把限流打成雪崩。
func TestUpstream429Passthrough(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "17")
		http.Error(w, "toomanyrequests", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	g := New(st, Options{
		Cache:   cs,
		RawBase: srv.URL,
		RawTTL:  time.Hour,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.requireToken = true
	tok := token(t, st)

	r := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if r.Code != http.StatusTooManyRequests {
		t.Fatalf("429 应透出，实际 %d", r.Code)
	}
	if got := r.Header().Get("Retry-After"); got != "17" {
		t.Fatalf("应带 Retry-After: 17，实际 %q", got)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("限流时不应自动重试，实际回源 %d 次", hits)
	}
}

func TestUpstream503Passthrough(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	g := New(st, Options{
		Cache:   cs,
		RawBase: srv.URL,
		RawTTL:  time.Hour,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.requireToken = true
	r := do(g, token(t, st), "http://gw/raw/o/r/main/a.txt", nil)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("503 应透出，实际 %d", r.Code)
	}
}

func ghPlan() *adapter.Plan {
	return &adapter.Plan{
		Kind:     "release",
		CacheKey: "gh:o/r/releases/download/v1/a.tgz",
	}
}

// 未配置 Token：一次 API 请求都不发。
func TestEnrichNoTokenNoCall(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"assets":[]}`))
	}))
	t.Cleanup(srv.Close)

	g := &Gateway{}
	g.ghMeta = ghapi.New("")
	g.ghMeta.Base = srv.URL
	p := ghPlan()
	g.enrichReleaseDigest(context.Background(), p)
	if p.ExpectSHA != "" || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("无 Token 不应查询: sha=%q calls=%d", p.ExpectSHA, calls)
	}
}

// 有摘要：回源前就写进 Plan，后续才能进入完整性校验与并行准入。
func TestEnrichSetsDigestBeforeServing(t *testing.T) {
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"assets":[{"id":2,"name":"a.tgz","digest":"sha256:` + sum + `","updated_at":"2026-01-01T00:00:00Z"}]}`))
	}))
	t.Cleanup(srv.Close)

	g := &Gateway{}
	g.ghMeta = ghapi.New("tok")
	g.ghMeta.Base = srv.URL
	p := ghPlan()
	g.enrichReleaseDigest(context.Background(), p)
	if p.ExpectSHA != sum {
		t.Fatalf("摘要未写入 Plan: %q", p.ExpectSHA)
	}
	if p.ExpectSource != adapter.DigestSourceGitHub {
		t.Fatalf("来源应为 github_asset_api，实际 %q", p.ExpectSource)
	}
}

// 非 release 路径不查 API（Docker/raw 有自己的摘要来源）。
func TestEnrichSkipsNonRelease(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)
	g := &Gateway{}
	g.ghMeta = ghapi.New("tok")
	g.ghMeta.Base = srv.URL
	p := &adapter.Plan{Kind: "raw", CacheKey: "raw:o/r/main/a.txt"}
	g.enrichReleaseDigest(context.Background(), p)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("非 release 不应查询 GitHub API")
	}
}

// 查询超时必须降级：返回空摘要，不报错、不阻塞。
func TestEnrichTimeoutDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(`{"assets":[]}`))
	}))
	t.Cleanup(srv.Close)
	g := &Gateway{}
	g.ghMeta = ghapi.New("tok")
	g.ghMeta.Base = srv.URL
	g.ghMeta.HTTP = &http.Client{Timeout: 30 * time.Millisecond}

	start := time.Now()
	p := ghPlan()
	g.enrichReleaseDigest(context.Background(), p)
	if p.ExpectSHA != "" {
		t.Fatal("超时不应设置摘要")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("超时降级耗时过长: %s", el)
	}
	_ = strconv.Itoa(0)
}
