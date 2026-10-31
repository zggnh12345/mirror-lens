package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func doGH(g *Gateway, tok, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	g.ServeGH(rec, req)
	return rec
}

// release 资产要能被镜像源接住：镜像按「base + 完整 URL」的前缀约定拼地址。
func TestGHReleaseThroughMirror(t *testing.T) {
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

	var hits int32
	var seen atomic.Value
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seen.Store(r.URL.Path)
		w.Header().Set("ETag", `"rel"`)
		w.Write([]byte("release-bytes"))
	}))
	defer mirror.Close()
	addSchedSource(t, st, "mirror", mirror.URL, 50, 5000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: "https://raw.githubusercontent.com",
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))
	tok := token(t, st)

	rec := doGH(g, tok, "http://gw/gh/octocat/Hello-World/releases/download/v1.0/asset.zip")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "release-bytes" {
		t.Fatalf("内容不对: %q", got)
	}
	if got := rec.Header().Get("X-ML-Source"); got != "mirror" {
		t.Fatalf("X-ML-Source 应为 mirror，实际 %q", got)
	}
	if got, _ := seen.Load().(string); got != "/https://github.com/octocat/Hello-World/releases/download/v1.0/asset.zip" {
		t.Fatalf("镜像收到的路径不对: %q", got)
	}

	// 同一个资产再取一次：命中缓存，不该再打镜像。
	if rec := doGH(g, tok, "http://gw/gh/octocat/Hello-World/releases/download/v1.0/asset.zip"); rec.Code != http.StatusOK {
		t.Fatalf("二次请求应 200，得到 %d", rec.Code)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("二次请求应命中缓存，镜像被打 %d 次", n)
	}
}

// 源码包走同一条路，但新鲜期短：分支会动。
func TestGHArchiveTTL(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, _ := cache.New(filepath.Join(dir, "cache"))
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("zip-bytes"))
	}))
	defer mirror.Close()
	addSchedSource(t, st, "mirror", mirror.URL, 50, 5000)

	g := New(st, Options{Cache: cs, RawBase: "https://raw.githubusercontent.com",
		Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))
	tok := token(t, st)

	if rec := doGH(g, tok, "http://gw/gh/octocat/Hello-World/archive/refs/heads/main.zip"); rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	// 分支快照 1 小时过期；release 资产 24 小时。这里只确认两者不同，
	// 免得以后有人把 TTL 统一了却没人发现。
	e, err := st.CacheGet("gh:octocat/hello-world/archive/refs/heads/main.zip")
	if err != nil || e == nil {
		t.Fatalf("缓存索引应有一条记录: %v", err)
	}
	if e.TTL > int64((2 * time.Hour).Seconds()) {
		t.Fatalf("分支快照的新鲜期应短于 2 小时，实际 %d 秒", e.TTL)
	}
}

// 路径不合法时给 400，而不是把请求转发到别处。
func TestGHBadPath(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, _ := cache.New(filepath.Join(dir, "cache"))
	g := New(st, Options{Cache: cs, Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})

	if rec := doGH(g, token(t, st), "http://gw/gh/o/r/releases/tag/v1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("网页路径应 400，得到 %d", rec.Code)
	}
}
