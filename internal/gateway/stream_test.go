package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// rangedUpstream 是个正经的文件服务器：支持 Range / If-Range / If-None-Match，
// 也能按需「忽略 Range」或「发一半就断」。
type rangedUpstream struct {
	mu           sync.Mutex
	body         []byte
	etag         string
	cc           string
	noRange      bool
	truncateOnce bool
	hits         int32

	lastRange   string
	lastIfRange string
}

func (u *rangedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&u.hits, 1)
	u.mu.Lock()
	body, etag, cc, noRange, trunc := u.body, u.etag, u.cc, u.noRange, u.truncateOnce
	if trunc {
		u.truncateOnce = false
	}
	u.lastRange = r.Header.Get("Range")
	u.lastIfRange = r.Header.Get("If-Range")
	u.mu.Unlock()

	if r.URL.Path == "/missing" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("ETag", etag)
	if cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	if trunc {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(body[:len(body)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	if noRange {
		r.Header.Del("Range")
		r.Header.Del("If-Range")
	}
	http.ServeContent(w, r, "f.bin", time.Unix(1700000000, 0), bytes.NewReader(body))
}

func (u *rangedUpstream) hitsCount() int { return int(atomic.LoadInt32(&u.hits)) }

func (u *rangedUpstream) set(body []byte, etag string) {
	u.mu.Lock()
	u.body, u.etag = body, etag
	u.mu.Unlock()
}

type httpFixture struct {
	gw    *Gateway
	st    *store.Store
	cs    *cache.Store
	up    *rangedUpstream
	srv   *httptest.Server
	dir   string
	token string
}

func newHTTPFixture(t *testing.T, maxBytes int64) *httpFixture {
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
	up := &rangedUpstream{
		body: []byte(strings.Repeat("0123456789abcdef", 64)), // 1 KiB
		etag: `"v1"`,
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	gw := New(st, Options{
		Cache:    cs,
		RawBase:  srv.URL,
		MaxBytes: maxBytes,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	plain, _, err := st.CreateToken("test")
	if err != nil {
		t.Fatalf("建 Token: %v", err)
	}
	return &httpFixture{gw: gw, st: st, cs: cs, up: up, srv: srv, dir: dir, token: plain}
}

func (f *httpFixture) req(t *testing.T, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	enc := url.PathEscape(target)
	r := httptest.NewRequest(http.MethodGet, "http://gw/http/"+enc, nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.gw.ServeHTTPProxy(w, r)
	return w
}

func (f *httpFixture) partials(t *testing.T) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(f.dir, "cache", "partial", "*.json"))
	return m
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestHTTPStreamMissThenHit(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"

	w1 := f.req(t, target, nil)
	if w1.Code != 200 {
		t.Fatalf("首次状态 %d: %s", w1.Code, w1.Body.String())
	}
	if got := w1.Header().Get("X-ML-Cache"); got != "MISS" {
		t.Fatalf("首次应 MISS，实际 %q", got)
	}
	if !bytes.Equal(w1.Body.Bytes(), f.up.body) {
		t.Fatalf("内容不一致，得到 %d 字节", w1.Body.Len())
	}
	if cl := w1.Header().Get("Content-Length"); cl != strconv.Itoa(len(f.up.body)) {
		t.Fatalf("Content-Length %q 不对", cl)
	}

	w2 := f.req(t, target, nil)
	if got := w2.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("二次应 HIT，实际 %q", got)
	}
	if !bytes.Equal(w2.Body.Bytes(), f.up.body) {
		t.Fatal("命中内容不一致")
	}
	if n := f.up.hitsCount(); n != 1 {
		t.Fatalf("应只回源 1 次，实际 %d", n)
	}
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("缓存里没有这份内容")
	}
}

// 普通流式下载的提交也必须走 blob 锁：持锁期间不能出现「文件已落盘、
// 索引还没写」的中间态，否则 janitor 会把它当孤儿删掉。
func TestStreamCommitHoldsBlobLock(t *testing.T) {
	f := newHTTPFixture(t, 0)
	sum := sha(f.up.body)

	unlock := f.cs.LockBlob(sum)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodGet, "http://gw/http/"+url.PathEscape(f.srv.URL+"/f.bin"), nil)
		r.Header.Set("Authorization", "Bearer "+f.token)
		f.gw.ServeHTTPProxy(httptest.NewRecorder(), r)
	}()

	select {
	case <-done:
		t.Fatal("持锁期间就完成了提交：Commit/CachePut 没走 blob 锁")
	case <-time.After(300 * time.Millisecond):
	}
	if f.cs.Has(sum) {
		t.Fatal("持锁期间不该把 blob 发布到缓存目录")
	}
	unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("放锁后下载没有继续")
	}
	if !f.cs.Has(sum) {
		t.Fatal("放锁后应该完成提交")
	}
}

func TestHTTPRangePassThrough(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"

	w := f.req(t, target, map[string]string{"Range": "bytes=0-9"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("应 206，实际 %d", w.Code)
	}
	if got := w.Header().Get("Content-Range"); !strings.HasPrefix(got, "bytes 0-9/") {
		t.Fatalf("Content-Range 不对: %q", got)
	}
	if w.Body.Len() != 10 {
		t.Fatalf("应 10 字节，实际 %d", w.Body.Len())
	}
	if got := w.Header().Get("X-ML-Cache"); got != "PASS" {
		t.Fatalf("Range 请求应直通，实际 %q", got)
	}
	if len(f.partials(t)) != 0 {
		t.Fatal("Range 直通不该留下续传文件")
	}
	if e, _ := f.st.CacheGet("http:" + target); e != nil {
		t.Fatal("Range 直通不该写缓存")
	}
}

func TestHTTPRangeFromCache(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"
	f.req(t, target, nil) // 先整份填进缓存

	w := f.req(t, target, map[string]string{"Range": "bytes=10-19"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("命中缓存后 Range 应 206，实际 %d", w.Code)
	}
	if got := w.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("应从缓存切片，实际 %q", got)
	}
	if w.Body.Len() != 10 {
		t.Fatalf("应 10 字节，实际 %d", w.Body.Len())
	}
	if n := f.up.hitsCount(); n != 1 {
		t.Fatalf("不该回源，回源次数 %d", n)
	}
}

func TestHTTPResumeAfterDisconnect(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"
	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.mu.Unlock()

	w1 := f.req(t, target, nil)
	if w1.Body.Len() >= len(f.up.body) {
		t.Fatalf("第一次应该是半截，实际 %d 字节", w1.Body.Len())
	}
	half := w1.Body.Len()
	if len(f.partials(t)) != 1 {
		t.Fatal("断流后应留下续传点")
	}

	w2 := f.req(t, target, nil)
	if got := w2.Header().Get("X-ML-Cache"); got != "RESUME" {
		t.Fatalf("应续传，实际 %q", got)
	}
	if !bytes.Equal(w2.Body.Bytes(), f.up.body) {
		t.Fatalf("续传后内容不一致：%d 字节", w2.Body.Len())
	}
	f.up.mu.Lock()
	lr, ifr := f.up.lastRange, f.up.lastIfRange
	f.up.mu.Unlock()
	if lr != "bytes="+strconv.Itoa(half)+"-" {
		t.Fatalf("上游收到的 Range 是 %q，期望从 %d 开始", lr, half)
	}
	if ifr != `"v1"` {
		t.Fatalf("If-Range 应为 ETag，实际 %q", ifr)
	}
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("续传完成后没落盘")
	}
	if len(f.partials(t)) != 0 {
		t.Fatal("完成后应清掉续传点")
	}
}

func TestHTTPUpstreamIgnoresRange(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"
	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.noRange = true
	f.up.mu.Unlock()

	f.req(t, target, nil)
	w2 := f.req(t, target, nil)
	if !bytes.Equal(w2.Body.Bytes(), f.up.body) {
		t.Fatalf("上游忽略 Range 时应从头重下，实际 %d 字节", w2.Body.Len())
	}
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("重下后应落盘")
	}
}

func TestHTTPNoStoreNotCached(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"
	f.up.mu.Lock()
	f.up.cc = "no-store"
	f.up.mu.Unlock()

	f.req(t, target, nil)
	f.req(t, target, nil)
	if n := f.up.hitsCount(); n != 2 {
		t.Fatalf("no-store 不该缓存，回源次数 %d", n)
	}
	if e, _ := f.st.CacheGet("http:" + target); e != nil {
		t.Fatal("no-store 写了缓存索引")
	}
}

func TestHTTPTooBigPassThrough(t *testing.T) {
	f := newHTTPFixture(t, 100) // 上限 100 字节，文件 1 KiB
	target := f.srv.URL + "/f.bin"

	w := f.req(t, target, nil)
	if w.Body.Len() != len(f.up.body) {
		t.Fatalf("超限也要完整透传，实际 %d 字节", w.Body.Len())
	}
	if got := w.Header().Get("X-ML-Cache"); got != "BYPASS" {
		t.Fatalf("超限应标 BYPASS，实际 %q", got)
	}
	if e, _ := f.st.CacheGet("http:" + target); e != nil {
		t.Fatal("超限不该写缓存")
	}
}

func TestHTTPRevalidate304(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"
	f.up.mu.Lock()
	f.up.cc = "max-age=0"
	f.up.mu.Unlock()

	f.req(t, target, nil)
	w2 := f.req(t, target, nil)
	if got := w2.Header().Get("X-ML-Cache"); got != "HIT-REVAL" {
		t.Fatalf("应 HIT-REVAL，实际 %q", got)
	}
	if !bytes.Equal(w2.Body.Bytes(), f.up.body) {
		t.Fatal("304 后内容不对")
	}
	if n := f.up.hitsCount(); n != 2 {
		t.Fatalf("应回源 2 次（一次 200 一次 304），实际 %d", n)
	}
}

func TestHTTPUpstreamChanged(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"
	f.up.mu.Lock()
	f.up.cc = "max-age=0"
	f.up.mu.Unlock()
	f.req(t, target, nil)

	nb := []byte(strings.Repeat("XYZ", 300))
	f.up.set(nb, `"v2"`)
	w := f.req(t, target, nil)
	if !bytes.Equal(w.Body.Bytes(), nb) {
		t.Fatalf("上游更新后应返回新内容，实际 %d 字节", w.Body.Len())
	}
	if got := w.Header().Get("ETag"); got != `"v2"` {
		t.Fatalf("ETag 应更新，实际 %q", got)
	}
}

func TestHTTPSSRFBlocked(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, _ := cache.New(filepath.Join(dir, "cache"))
	gw := New(st, Options{
		Cache: cs,
		Guard: safehttp.New(safehttp.Options{AllowLoopback: false}),
	})
	plain, _, _ := st.CreateToken("t")
	r := httptest.NewRequest(http.MethodGet, "http://gw/http/"+url.PathEscape("http://127.0.0.1:9/x"), nil)
	r.Header.Set("Authorization", "Bearer "+plain)
	w := httptest.NewRecorder()
	gw.ServeHTTPProxy(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("内网地址应 403，实际 %d", w.Code)
	}
}

func TestHTTPBadTarget(t *testing.T) {
	f := newHTTPFixture(t, 0)
	r := httptest.NewRequest(http.MethodGet, "http://gw/http/"+url.PathEscape("file:///etc/passwd"), nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.gw.ServeHTTPProxy(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非 http(s) 应 400，实际 %d", w.Code)
	}
}

func TestHTTPConcurrentSingleFill(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"
	var wg sync.WaitGroup
	bodies := make([][]byte, 6)
	codes := make([]int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "http://gw/http/"+url.PathEscape(target), nil)
			r.Header.Set("Authorization", "Bearer "+f.token)
			w := httptest.NewRecorder()
			f.gw.ServeHTTPProxy(w, r)
			bodies[i], codes[i] = w.Body.Bytes(), w.Code
		}(i)
	}
	wg.Wait()
	for i := range bodies {
		if codes[i] != 200 || !bytes.Equal(bodies[i], f.up.body) {
			t.Fatalf("第 %d 个并发请求坏了：%d / %d 字节", i, codes[i], len(bodies[i]))
		}
	}
	if n := f.up.hitsCount(); n != 1 {
		t.Fatalf("并发应只回源 1 次，实际 %d", n)
	}
}

func TestHTTP404(t *testing.T) {
	f := newHTTPFixture(t, 0)
	w := f.req(t, f.srv.URL+"/missing", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("应 404，实际 %d", w.Code)
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in    string
		start int64
		total int64
		ok    bool
	}{
		{"bytes 0-99/1000", 0, 1000, true},
		{"bytes 100-999/1000", 100, 1000, true},
		{"bytes 5-9/*", 5, -1, true},
		{"bytes */1000", 0, 0, false},
		{"nonsense", 0, 0, false},
	}
	for _, c := range cases {
		s, tt, err := parseContentRange(c.in)
		if c.ok != (err == nil) {
			t.Fatalf("%q: err=%v", c.in, err)
		}
		if err == nil && (s != c.start || tt != c.total) {
			t.Fatalf("%q: 得到 %d/%d", c.in, s, tt)
		}
	}
}
