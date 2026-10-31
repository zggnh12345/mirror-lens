package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// gitUpstream 模拟一个 Smart HTTP 仓库：info/refs 按协议版本给不同广告，
// upload-pack 把请求体原样塞进响应，方便断言透传是否完整。
type gitUpstream struct {
	mu        sync.Mutex
	refsHits  int32
	postHits  int32
	lastBody  []byte
	lastProto string
	lastCT    string
	lastCE    string
}

const v1Advert = "001e# service=git-upload-pack\n0000"

func (u *gitUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/repo.git/info/refs":
		if r.URL.Query().Get("service") != "git-upload-pack" {
			http.Error(w, "bad service", http.StatusBadRequest)
			return
		}
		atomic.AddInt32(&u.refsHits, 1)
		u.mu.Lock()
		u.lastProto = r.Header.Get("Git-Protocol")
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		if r.Header.Get("Git-Protocol") == "version=2" {
			io.WriteString(w, "000eversion 2\n0000")
			return
		}
		io.WriteString(w, v1Advert)
	case "/repo.git/git-upload-pack":
		atomic.AddInt32(&u.postHits, 1)
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.lastBody = body
		u.lastCT = r.Header.Get("Content-Type")
		u.lastCE = r.Header.Get("Content-Encoding")
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Write([]byte("PACK"))
		w.Write(body)
	case "/redirect.git/info/refs":
		http.Redirect(w, r, "/repo.git/info/refs?service=git-upload-pack", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

type gitFixture struct {
	gw    *Gateway
	st    *store.Store
	up    *gitUpstream
	srv   *httptest.Server
	token string
}

func newGitFixture(t *testing.T, ttlSeconds int64) *gitFixture {
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
	up := &gitUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	gw := New(st, Options{
		Cache:  cs,
		GitTTL: time.Duration(ttlSeconds) * time.Second,
		Guard:  safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	plain, _, err := st.CreateToken("git")
	if err != nil {
		t.Fatalf("建 Token: %v", err)
	}
	return &gitFixture{gw: gw, st: st, up: up, srv: srv, token: plain}
}

func (f *gitFixture) do(t *testing.T, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://gw"+target, rd)
	r.SetBasicAuth(f.token, "") // git 客户端的习惯写法：token 放在用户名位
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.gw.ServeGit(w, r)
	return w
}

// repo 返回 /git/ 后面的路径。测试服务器是明文 HTTP，所以显式写 http。
func (f *gitFixture) repo() string {
	return "http/" + strings.TrimPrefix(f.srv.URL, "http://") + "/repo.git"
}

func TestGitInfoRefsMissThenHit(t *testing.T) {
	f := newGitFixture(t, 30)
	target := "/git/" + f.repo() + "/info/refs?service=git-upload-pack"

	w1 := f.do(t, http.MethodGet, target, nil, nil)
	if w1.Code != 200 {
		t.Fatalf("首次 %d: %s", w1.Code, w1.Body.String())
	}
	if got := w1.Header().Get("X-ML-Cache"); got != "MISS" {
		t.Fatalf("首次应 MISS，实际 %q", got)
	}
	if w1.Body.String() != v1Advert {
		t.Fatalf("内容不对: %q", w1.Body.String())
	}
	if got := w1.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("info/refs 不能被中间层缓存，实际 %q", got)
	}

	w2 := f.do(t, http.MethodGet, target, nil, nil)
	if got := w2.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("二次应 HIT，实际 %q", got)
	}
	if w2.Body.String() != v1Advert {
		t.Fatal("命中内容不对")
	}
	if n := atomic.LoadInt32(&f.up.refsHits); n != 1 {
		t.Fatalf("应只回源 1 次，实际 %d", n)
	}
}

func TestGitProtocolVersionSeparated(t *testing.T) {
	f := newGitFixture(t, 30)
	target := "/git/" + f.repo() + "/info/refs?service=git-upload-pack"

	f.do(t, http.MethodGet, target, nil, nil)
	w2 := f.do(t, http.MethodGet, target, nil, map[string]string{"Git-Protocol": "version=2"})
	if got := w2.Header().Get("X-ML-Cache"); got != "MISS" {
		t.Fatalf("v2 不能复用 v1 的缓存，实际 %q", got)
	}
	if !strings.Contains(w2.Body.String(), "version 2") {
		t.Fatalf("v2 广告不对: %q", w2.Body.String())
	}
	f.up.mu.Lock()
	proto := f.up.lastProto
	f.up.mu.Unlock()
	if proto != "version=2" {
		t.Fatalf("Git-Protocol 没传到上游: %q", proto)
	}
}

func TestGitUploadPackPassthrough(t *testing.T) {
	f := newGitFixture(t, 30)
	target := "/git/" + f.repo() + "/git-upload-pack"
	body := []byte("0032want 0123456789abcdef\n0000")

	w := f.do(t, http.MethodPost, target, body, map[string]string{
		"Content-Type": "application/x-git-upload-pack-request",
		"Git-Protocol": "version=2",
	})
	if w.Code != 200 {
		t.Fatalf("状态 %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ML-Cache"); got != "PASS" {
		t.Fatalf("upload-pack 应直通，实际 %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/x-git-upload-pack-result" {
		t.Fatalf("Content-Type 丢了: %q", got)
	}
	if !bytes.Equal(w.Body.Bytes(), append([]byte("PACK"), body...)) {
		t.Fatalf("响应体不对: %q", w.Body.String())
	}
	f.up.mu.Lock()
	gotBody, gotCT := f.up.lastBody, f.up.lastCT
	f.up.mu.Unlock()
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("上游收到的请求体不对: %q", gotBody)
	}
	if gotCT != "application/x-git-upload-pack-request" {
		t.Fatalf("请求 Content-Type 没透传: %q", gotCT)
	}
	if e, _ := f.st.CacheGet("git:" + f.srv.URL + "/repo.git|version=2"); e != nil {
		t.Fatal("upload-pack 不该写缓存")
	}
}

// git 会把 fetch 的请求体 gzip 后再发。只转发字节不转发 Content-Encoding，
// 上游会按明文解析并回 400——这是实测踩到的坑，别删。
func TestGitContentEncodingPassthrough(t *testing.T) {
	f := newGitFixture(t, 30)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("0032want deadbeef\n0000"))
	zw.Close()

	w := f.do(t, http.MethodPost, "/git/"+f.repo()+"/git-upload-pack", buf.Bytes(), map[string]string{
		"Content-Type":     "application/x-git-upload-pack-request",
		"Content-Encoding": "gzip",
		"Git-Protocol":     "version=2",
	})
	if w.Code != 200 {
		t.Fatalf("状态 %d: %s", w.Code, w.Body.String())
	}
	f.up.mu.Lock()
	ce, body := f.up.lastCE, f.up.lastBody
	f.up.mu.Unlock()
	if ce != "gzip" {
		t.Fatalf("Content-Encoding 没透传: %q", ce)
	}
	if !bytes.Equal(body, buf.Bytes()) {
		t.Fatal("gzip 请求体被改动了")
	}
}

func TestGitPostNotCached(t *testing.T) {
	f := newGitFixture(t, 30)
	target := "/git/" + f.repo() + "/git-upload-pack"
	f.do(t, http.MethodPost, target, []byte("a"), nil)
	f.do(t, http.MethodPost, target, []byte("a"), nil)
	if n := atomic.LoadInt32(&f.up.postHits); n != 2 {
		t.Fatalf("POST 每次都该回源，实际 %d", n)
	}
}

func TestGitReceivePackRejected(t *testing.T) {
	f := newGitFixture(t, 30)
	w := f.do(t, http.MethodGet, "/git/"+f.repo()+"/info/refs?service=git-receive-pack", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("push 应 403，实际 %d", w.Code)
	}
}

func TestGitDumbProtocolRejected(t *testing.T) {
	f := newGitFixture(t, 30)
	w := f.do(t, http.MethodGet, "/git/"+f.repo()+"/objects/ab/cdef", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("dumb 协议应 400，实际 %d", w.Code)
	}
}

func TestGitSSRFBlocked(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, _ := cache.New(filepath.Join(dir, "cache"))
	gw := New(st, Options{Cache: cs, Guard: safehttp.New(safehttp.Options{AllowLoopback: false})})
	plain, _, _ := st.CreateToken("t")
	r := httptest.NewRequest(http.MethodGet, "http://gw/git/127.0.0.1:9/repo.git/info/refs?service=git-upload-pack", nil)
	r.SetBasicAuth(plain, "")
	w := httptest.NewRecorder()
	gw.ServeGit(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("内网地址应 403，实际 %d", w.Code)
	}
}

func TestGitNoTokenRejected(t *testing.T) {
	f := newGitFixture(t, 30)
	f.gw.requireToken = true
	r := httptest.NewRequest(http.MethodGet, "http://gw/git/"+f.repo()+"/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	f.gw.ServeGit(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，实际 %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatal("应给 git 一个 Basic 挑战")
	}
}

func TestGitInfoRefsExpires(t *testing.T) {
	f := newGitFixture(t, 0) // TTL=0：每次都回源
	target := "/git/" + f.repo() + "/info/refs?service=git-upload-pack"
	f.do(t, http.MethodGet, target, nil, nil)
	w := f.do(t, http.MethodGet, target, nil, nil)
	if got := w.Header().Get("X-ML-Cache"); got == "HIT" {
		t.Fatal("TTL=0 不该命中")
	}
	if n := atomic.LoadInt32(&f.up.refsHits); n != 2 {
		t.Fatalf("应回源 2 次，实际 %d", n)
	}
}
