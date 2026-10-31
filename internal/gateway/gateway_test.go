package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// fakeUpstream 模拟 raw.githubusercontent.com
type fakeUpstream struct {
	mu     sync.Mutex
	body   string
	etag   string
	status int
	hits   int32
	delay  time.Duration
	digest string // Content-Digest 头，模拟上游自报摘要
}

func (u *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&u.hits, 1)
	if u.delay > 0 {
		time.Sleep(u.delay)
	}
	u.mu.Lock()
	body, etag, status, digest := u.body, u.etag, u.status, u.digest
	u.mu.Unlock()
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	if digest != "" {
		w.Header().Set("Content-Digest", digest)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	io.WriteString(w, body)
}

func (u *fakeUpstream) set(body, etag string) {
	u.mu.Lock()
	u.body, u.etag = body, etag
	u.mu.Unlock()
}

func newGW(t *testing.T, allowLoopback bool) (*Gateway, *store.Store, *cache.Store, *fakeUpstream, *httptest.Server) {
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
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: allowLoopback}),
	})
	return g, st, cs, up, srv
}

func token(t *testing.T, st *store.Store) string {
	t.Helper()
	plain, _, err := st.CreateToken("test")
	if err != nil {
		t.Fatalf("建 token: %v", err)
	}
	return plain
}

func do(g *Gateway, tok, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.ServeRaw(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	g, st, _, _, _ := newGW(t, true)
	g.requireToken = true // 这条用例专测认证，匿名放行是另一条路
	if rec := do(g, "", "http://gw/raw/o/r/main/a.txt", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带 token 应 401，得到 %d", rec.Code)
	}
	tok := token(t, st)
	if rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil); rec.Code != http.StatusOK {
		t.Fatalf("带 token 应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRevokedTokenRejected(t *testing.T) {
	g, st, _, _, _ := newGW(t, true)
	g.requireToken = true
	tok := token(t, st)
	list, _ := st.ListTokens()
	if err := st.RevokeToken(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("吊销后应 401，得到 %d", rec.Code)
	}
}

func TestMissThenHit(t *testing.T) {
	g, st, cs, up, _ := newGW(t, true)
	tok := token(t, st)

	r1 := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if r1.Code != 200 || r1.Body.String() != "hello" {
		t.Fatalf("首次: %d %q", r1.Code, r1.Body.String())
	}
	if got := r1.Header().Get("X-ML-Cache"); got != "MISS" {
		t.Fatalf("首次应为 MISS，得到 %q", got)
	}
	r2 := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if got := r2.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("二次应为 HIT，得到 %q", got)
	}
	if n := atomic.LoadInt32(&up.hits); n != 1 {
		t.Fatalf("上游应只被请求 1 次，实际 %d", n)
	}
	// 内容寻址：索引里记的 sha256 与磁盘文件名一致
	e, err := st.CacheGet("raw:o/r/main/a.txt")
	if err != nil || e == nil {
		t.Fatalf("索引缺失: %v", err)
	}
	if _, err := os.Stat(cs.BlobPath(e.SHA256)); err != nil {
		t.Fatalf("blob 不存在: %v", err)
	}
}

// 首次未命中只算回源填充，不能同时算一次命中——否则每个新文件都把命中率抬高。
func TestMissNotCountedAsHit(t *testing.T) {
	g, st, _, _, _ := newGW(t, true)
	tok := token(t, st)

	do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	hit, fill := trafficBytes(t, st)
	if hit != 0 {
		t.Fatalf("首次未命中不该记 hit，实际 %d", hit)
	}
	if fill != 5 {
		t.Fatalf("fill 应为 5，实际 %d", fill)
	}

	do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	hit, _ = trafficBytes(t, st)
	if hit != 5 {
		t.Fatalf("命中应记 5 字节，实际 %d", hit)
	}
}

func trafficBytes(t *testing.T, st *store.Store) (hit, fill int64) {
	t.Helper()
	rows, err := st.TrafficByDay(1)
	if err != nil {
		t.Fatalf("读流量: %v", err)
	}
	for _, r := range rows {
		switch r.Traffic {
		case store.TrafficHit:
			hit += r.Bytes
		case store.TrafficFill:
			fill += r.Bytes
		}
	}
	return
}

func TestRevalidate304(t *testing.T) {
	g, st, _, up, _ := newGW(t, true)
	tok := token(t, st)
	g.SetRawTTL(0) // 每次都重新验证

	do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	r := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if got := r.Header().Get("X-ML-Cache"); got != "HIT-REVAL" {
		t.Fatalf("应命中重新验证，得到 %q", got)
	}
	if r.Body.String() != "hello" {
		t.Fatalf("内容不对: %q", r.Body.String())
	}
	if n := atomic.LoadInt32(&up.hits); n != 2 {
		t.Fatalf("上游应被请求 2 次（含 304），实际 %d", n)
	}
}

func TestUpstreamUpdateRefreshesCache(t *testing.T) {
	g, st, cs, up, _ := newGW(t, true)
	tok := token(t, st)
	g.SetRawTTL(0)

	do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	up.set("hello v2", `"v2"`)
	r := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if r.Body.String() != "hello v2" {
		t.Fatalf("应拿到新内容，得到 %q", r.Body.String())
	}
	if got := r.Header().Get("X-ML-Cache"); got != "MISS" {
		t.Fatalf("更新后应为 MISS，得到 %q", got)
	}
	e, _ := st.CacheGet("raw:o/r/main/a.txt")
	if e.Size != int64(len("hello v2")) {
		t.Fatalf("索引大小没更新: %d", e.Size)
	}
	// 旧 blob 还在（别的键可能引用），但新内容必须已落盘
	if !cs.Has(e.SHA256) {
		t.Fatal("新 blob 未落盘")
	}
}

func TestConcurrentSingleFlight(t *testing.T) {
	g, st, _, up, _ := newGW(t, true)
	tok := token(t, st)
	up.delay = 120 * time.Millisecond

	var wg sync.WaitGroup
	bodies := make([]string, 8)
	codes := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
			codes[i] = rec.Code
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	for i := range codes {
		if codes[i] != 200 || bodies[i] != "hello" {
			t.Fatalf("第 %d 个请求: %d %q", i, codes[i], bodies[i])
		}
	}
	if n := atomic.LoadInt32(&up.hits); n != 1 {
		t.Fatalf("8 个并发只应回源 1 次，实际 %d", n)
	}
}

func TestRangeFromCache(t *testing.T) {
	g, st, _, _, _ := newGW(t, true)
	tok := token(t, st)
	do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)

	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", map[string]string{"Range": "bytes=0-1"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("应 206，得到 %d", rec.Code)
	}
	if rec.Body.String() != "he" {
		t.Fatalf("分片内容不对: %q", rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 0-1/5" {
		t.Fatalf("Content-Range 不对: %q", cr)
	}
}

func TestSSRFBlocked(t *testing.T) {
	g, st, _, _, _ := newGW(t, false) // 不开回环例外
	tok := token(t, st)
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("指向 127.0.0.1 应 403，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "hello" {
		t.Fatal("泄露了上游内容")
	}
}

func TestUpstream404(t *testing.T) {
	g, st, _, up, _ := newGW(t, true)
	tok := token(t, st)
	up.mu.Lock()
	up.status = http.StatusNotFound
	up.mu.Unlock()
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应透传 404，得到 %d", rec.Code)
	}
}

func TestBadPath(t *testing.T) {
	g, st, _, _, _ := newGW(t, true)
	tok := token(t, st)
	for _, p := range []string{
		"http://gw/raw/o/r",
		"http://gw/raw/o/r/main/../../etc/passwd",
		"http://gw/raw/o//main/a.txt",
	} {
		if rec := do(g, tok, p, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400，得到 %d", p, rec.Code)
		}
	}
}
