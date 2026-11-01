package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"path/filepath"
)

// edgeUpstream 是一台「会犯各种错」的文件服务器，用来压续传的边界。
type edgeUpstream struct {
	mu   sync.Mutex
	body []byte
	etag string

	truncateOnce bool // 第一次只发一半就掐断
	ignoreRange  bool // 无视 Range，永远回 200 整份
	badRange     bool // 回 206 但起点比请求的少 1
	shortBody    bool // 声明长度后少发一半，然后正常结束
	lastRange    string
	hits         int
}

func (u *edgeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	body, etag := u.body, u.etag
	trunc, ignore, bad, short := u.truncateOnce, u.ignoreRange, u.badRange, u.shortBody
	if trunc {
		u.truncateOnce = false
	}
	u.lastRange = r.Header.Get("Range")
	u.hits++
	u.mu.Unlock()

	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/octet-stream")

	if trunc {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body[:len(body)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	if short {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body[:len(body)/2])
		return
	}
	if ignore {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return
	}
	if bad && r.Header.Get("Range") != "" {
		start := 0
		if s, _, ok := parseRangeStart(r.Header.Get("Range")); ok && s > 0 {
			start = s - 1
		}
		part := body[start:]
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(body)-1)+"/"+strconv.Itoa(len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(part)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(part)
		return
	}
	http.ServeContent(w, r, "f.bin", time.Unix(1700000000, 0), bytes.NewReader(body))
}

func parseRangeStart(v string) (int, int, bool) {
	s := strings.TrimPrefix(v, "bytes=")
	if i := strings.IndexByte(s, '-'); i > 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, 0, false
		}
		return n, 0, true
	}
	return 0, 0, false
}

type edgeFixture struct {
	gw    *Gateway
	st    *store.Store
	cs    *cache.Store
	up    *edgeUpstream
	srv   *httptest.Server
	dir   string
	token string
}

func newEdgeFixture(t *testing.T) *edgeFixture {
	t.Helper()
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
	up := &edgeUpstream{body: []byte(strings.Repeat("0123456789abcdef", 64)), etag: `"v1"`}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	gw := New(st, Options{
		Cache:   cs,
		RawBase: srv.URL,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	plain, _, err := st.CreateToken("test")
	if err != nil {
		t.Fatal(err)
	}
	return &edgeFixture{gw: gw, st: st, cs: cs, up: up, srv: srv, dir: dir, token: plain}
}

func (f *edgeFixture) req(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://gw/http/"+url.PathEscape(target), nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.gw.ServeHTTPProxy(w, r)
	return w
}

func (f *edgeFixture) parts(t *testing.T) []string { return partialFiles(t, f.dir) }

// 正常续传：第一轮断流留点，第二轮带 Range 接上，客户端拿到完整一份。
func TestResumeHappyPath(t *testing.T) {
	f := newEdgeFixture(t)
	target := f.srv.URL + "/f.bin"

	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.mu.Unlock()
	w1 := f.req(t, target)
	if w1.Body.Len() == 0 || w1.Body.Len() >= len(f.up.body) {
		t.Fatalf("第一次应是半截，实际 %d 字节", w1.Body.Len())
	}

	w2 := f.req(t, target)
	if got := w2.Header().Get("X-ML-Cache"); got != "RESUME" {
		t.Fatalf("第二次应续传，X-ML-Cache=%q", got)
	}
	if !bytes.Equal(w2.Body.Bytes(), f.up.body) {
		t.Fatalf("续传结果不完整：%d/%d", w2.Body.Len(), len(f.up.body))
	}
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("续传完成后应落盘")
	}
	if n := len(f.parts(t)); n != 0 {
		t.Fatalf("完成后不该留下 .part，实际 %d", n)
	}
	f.up.mu.Lock()
	lr := f.up.lastRange
	f.up.mu.Unlock()
	if !strings.HasPrefix(lr, "bytes=") {
		t.Fatalf("续传应带 Range，实际 %q", lr)
	}
}

// 源站不支持 Range（回 200 整份）：不能把整份接在半截后面，只能从头写。
func TestResumeWhenUpstreamIgnoresRange(t *testing.T) {
	f := newEdgeFixture(t)
	target := f.srv.URL + "/f.bin"

	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.mu.Unlock()
	f.req(t, target)

	f.up.mu.Lock()
	f.up.ignoreRange = true
	f.up.mu.Unlock()

	w := f.req(t, target)
	if !bytes.Equal(w.Body.Bytes(), f.up.body) {
		t.Fatalf("上游忽略 Range 时应整份重下，实际 %d/%d 字节", w.Body.Len(), len(f.up.body))
	}
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("重下完成后应落盘")
	}
}

// 上游回的 Content-Range 起点和续传点对不上：宁可从零重下，也不能拼错。
func TestResumeRejectsMismatchedContentRange(t *testing.T) {
	f := newEdgeFixture(t)
	target := f.srv.URL + "/f.bin"

	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.mu.Unlock()
	f.req(t, target)

	f.up.mu.Lock()
	f.up.badRange = true
	f.up.mu.Unlock()

	w := f.req(t, target)
	if !bytes.Equal(w.Body.Bytes(), f.up.body) {
		t.Fatalf("对不上的分段不该拼给客户端：%d/%d 字节", w.Body.Len(), len(f.up.body))
	}
	if e, _ := f.st.CacheGet("http:" + target); e == nil {
		t.Fatal("从头重下成功后应落盘")
	}
}

// 上游声明的长度比实际发的多：流没走完，不能提交缓存。
func TestShortBodyNotCommitted(t *testing.T) {
	f := newEdgeFixture(t)
	target := f.srv.URL + "/f.bin"

	f.up.mu.Lock()
	f.up.shortBody = true
	f.up.mu.Unlock()

	w := f.req(t, target)
	if w.Body.Len() >= len(f.up.body) {
		t.Fatalf("上游只发了半份，客户端却收到 %d 字节", w.Body.Len())
	}
	if f.cs.Has(sha(f.up.body)) {
		t.Fatal("没下完的内容不该入缓存")
	}
	// 半截可以留作续传点，但不能是「已完成」的条目。
	if e, _ := f.st.CacheGet("http:" + target); e != nil {
		t.Fatalf("不完整的内容不该有缓存索引: %+v", e)
	}
}

// 两个请求同时续传同一个文件：结果都得是完整一份，缓存里也只有一份。
func TestConcurrentResumeSameFile(t *testing.T) {
	f := newEdgeFixture(t)
	target := f.srv.URL + "/f.bin"

	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.mu.Unlock()
	f.req(t, target)

	var wg sync.WaitGroup
	bodies := make([][]byte, 2)
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := f.req(t, target)
			codes[i] = w.Code
			bodies[i] = append([]byte(nil), w.Body.Bytes()...)
		}(i)
	}
	wg.Wait()

	for i, b := range bodies {
		if codes[i] != http.StatusOK {
			t.Fatalf("第 %d 个请求状态 %d", i, codes[i])
		}
		if !bytes.Equal(b, f.up.body) {
			t.Fatalf("第 %d 个请求拿到 %d 字节，不是完整文件", i, len(b))
		}
	}
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("并发续传后应落盘")
	}
}
