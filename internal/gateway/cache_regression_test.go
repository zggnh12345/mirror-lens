package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 批次 2：跨协议缓存回归基线里当前没有覆盖的三条路径。
// 这三个用例只断言「客户端看到什么 + 索引里留下什么」，不关心内部实现。

type cacheRegFixture struct {
	g   *Gateway
	st  *store.Store
	cs  *cache.Store
	up  *fakeUpstream
	srv *httptest.Server
	dir string
	tok string
}

func newCacheRegFixture(t *testing.T) *cacheRegFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}
	up := &fakeUpstream{body: "hello", etag: `"v1"`}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	g := New(st, Options{
		Cache:   cs,
		RawBase: srv.URL,
		RawTTL:  time.Hour,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.requireToken = true
	return &cacheRegFixture{g: g, st: st, cs: cs, up: up, srv: srv, dir: dir, tok: token(t, st)}
}

func regSha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// 提交失败（rename 落位不了）时：客户端仍拿到完整字节，索引里不能出现条目，
// 下次请求必须能重新回源成功——不能留下「有索引没文件」或「永久 502」。
func TestCacheCommitFailureServesWithoutIndex(t *testing.T) {
	f := newCacheRegFixture(t)
	// 把目标 blob 路径占成目录，让 os.Rename 必然失败。
	if err := os.MkdirAll(f.cs.BlobPath(regSha("hello")), 0o755); err != nil {
		t.Fatal(err)
	}

	r1 := do(f.g, f.tok, "http://gw/raw/o/r/main/a.txt", nil)
	if r1.Code != http.StatusOK || r1.Body.String() != "hello" {
		t.Fatalf("提交失败不应影响本次响应: %d %q", r1.Code, r1.Body.String())
	}
	if e, _ := f.st.CacheGet("raw:o/r/main/a.txt"); e != nil {
		t.Fatalf("提交失败却写了索引: %+v", e)
	}

	if err := os.RemoveAll(f.cs.BlobPath(regSha("hello"))); err != nil {
		t.Fatal(err)
	}
	r2 := do(f.g, f.tok, "http://gw/raw/o/r/main/a.txt", nil)
	if r2.Code != http.StatusOK || r2.Body.String() != "hello" {
		t.Fatalf("重试应成功: %d %q", r2.Code, r2.Body.String())
	}
	// 第一次请求：填充失败后转透传，会再回源一次；第二次请求重新填充。
	if n := int(atomic.LoadInt32(&f.up.hits)); n != 3 {
		t.Fatalf("应回源 3 次（填充失败转透传 + 重试），实际 %d 次", n)
	}
	if e, _ := f.st.CacheGet("raw:o/r/main/a.txt"); e == nil {
		t.Fatal("重试成功后索引应存在")
	}
}

// 服务重启（SQLite 重开、缓存目录重扫）后，命中不应再回源。
func TestCacheSurvivesRestart(t *testing.T) {
	f := newCacheRegFixture(t)
	do(f.g, f.tok, "http://gw/raw/o/r/main/a.txt", nil)
	if n := int(atomic.LoadInt32(&f.up.hits)); n != 1 {
		t.Fatalf("首次应回源 1 次，实际 %d", n)
	}

	// 模拟重启：关库，用同一目录重新开一套 Gateway。
	f.st.Close()
	st2, err := store.Open(filepath.Join(f.dir, "t.db"))
	if err != nil {
		t.Fatalf("重开库: %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	cs2, err := cache.New(filepath.Join(f.dir, "cache"))
	if err != nil {
		t.Fatalf("重开缓存: %v", err)
	}
	g2 := New(st2, Options{
		Cache:   cs2,
		RawBase: f.srv.URL,
		RawTTL:  time.Hour,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g2.requireToken = true

	r := do(g2, token(t, st2), "http://gw/raw/o/r/main/a.txt", nil)
	if r.Code != http.StatusOK || r.Body.String() != "hello" {
		t.Fatalf("重启后应命中: %d %q", r.Code, r.Body.String())
	}
	if got := r.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("重启后应 HIT，实际 %q", got)
	}
	if n := int(atomic.LoadInt32(&f.up.hits)); n != 1 {
		t.Fatalf("重启后不应回源，实际回源 %d 次", n)
	}
}

// 索引在、文件被删：当前实现返回一次 502 并删索引，客户端重试应重新填充。
// 这是「索引与文件不一致」在请求路径上的恢复行为，janitor 侧另有孤儿清理。
func TestCacheMissingBlobDropsIndexAndRefetches(t *testing.T) {
	f := newCacheRegFixture(t)
	do(f.g, f.tok, "http://gw/raw/o/r/main/a.txt", nil)

	e, err := f.st.CacheGet("raw:o/r/main/a.txt")
	if err != nil || e == nil {
		t.Fatalf("首次填充后应有索引: %v", err)
	}
	if err := os.Remove(f.cs.BlobPath(e.SHA256)); err != nil {
		t.Fatal(err)
	}

	r := do(f.g, f.tok, "http://gw/raw/o/r/main/a.txt", nil)
	if r.Code != http.StatusBadGateway {
		t.Fatalf("文件丢失应返回 502，实际 %d", r.Code)
	}
	if gone, _ := f.st.CacheGet("raw:o/r/main/a.txt"); gone != nil {
		t.Fatal("文件丢失后应删掉索引，避免下次继续 502")
	}

	r2 := do(f.g, f.tok, "http://gw/raw/o/r/main/a.txt", nil)
	if r2.Code != http.StatusOK || r2.Body.String() != "hello" {
		t.Fatalf("重试应重新填充: %d %q", r2.Code, r2.Body.String())
	}
	if n := int(atomic.LoadInt32(&f.up.hits)); n != 2 {
		t.Fatalf("应回源 2 次（首次 + 重填），实际 %d", n)
	}
}

// 上游声明 Content-Length 但提前断流：不得把半份内容写进缓存。
func TestTruncatedUpstreamNotCached(t *testing.T) {
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
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/plain")
		if n == 1 {
			// 第一次：声明 100 字节只发 10 字节，客户端会拿到 unexpected EOF。
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("0123456789"))
			return
		}
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("0123456789"))
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
	// 截断可能表现为：头部未发出直接 502，或已流式输出但短于声明长度。
	// 两种都允许，唯一硬要求是「不进缓存」。
	if r.Code != http.StatusOK && r.Code != http.StatusBadGateway {
		t.Fatalf("截断响应状态异常: %d", r.Code)
	}
	if got := r.Body.String(); len(got) >= 100 {
		t.Fatalf("上游只发了 10 字节，实际收到 %d", len(got))
	}
	if e, _ := st.CacheGet("raw:o/r/main/a.txt"); e != nil {
		t.Fatalf("截断内容不能进缓存: %+v", e)
	}
	if cs.Has(regSha("0123456789")) {
		t.Fatal("截断内容不能落成 blob")
	}

	r2 := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if r2.Code != http.StatusOK {
		t.Fatalf("重试应重新回源，实际 %d", r2.Code)
	}
	if n := int(atomic.LoadInt32(&hits)); n != 2 {
		t.Fatalf("应回源 2 次，实际 %d", n)
	}
	if !strings.Contains(r2.Body.String(), "0123456789") {
		t.Fatalf("重试内容异常: %q", r2.Body.String())
	}
}
