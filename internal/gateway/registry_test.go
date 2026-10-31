package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

const (
	manifestsAccept = "application/vnd.docker.distribution.manifest.v2+json"
	indexAccept     = "application/vnd.oci.image.index.v1+json"
)

var blobBytes = []byte("layer-bytes-for-test-0123456789")

// regUpstream 模拟一个 registry v2 上游：可选 Bearer 挑战、按 Accept 给不同清单、
// 可以故意把 blob 的字节改坏来测校验。
type regUpstream struct {
	mu           sync.Mutex
	manifestHits int32
	blobHits     int32
	headHits     int32
	tokenHits    int32
	lastAccept   string
	lastRange    string

	challenge  bool
	tamperBlob bool
	// noDigestHeader 模拟真实上游：307 到 CDN 之后，落地的响应里没有摘要头。
	noDigestHeader bool
}

func (u *regUpstream) blobDigest() string {
	sum := sha256.Sum256(blobBytes)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (u *regUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		atomic.AddInt32(&u.tokenHits, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"token":"tok123","expires_in":300}`)
		return
	}
	if r.URL.Path == "/v2/" {
		io.WriteString(w, "{}")
		return
	}
	if u.challenge && r.Header.Get("Authorization") != "Bearer tok123" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(
			`Bearer realm="http://%s/token",service="test",scope="repository:library/alpine:pull"`, r.Host))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch {
	case strings.HasSuffix(r.URL.Path, "/manifests/latest") || strings.Contains(r.URL.Path, "/manifests/sha256:"):
		atomic.AddInt32(&u.manifestHits, 1)
		accept := r.Header.Get("Accept")
		u.mu.Lock()
		u.lastAccept = accept
		u.mu.Unlock()
		if strings.Contains(accept, "index") {
			w.Header().Set("Content-Type", indexAccept)
			io.WriteString(w, `{"mediaType":"index"}`)
			return
		}
		w.Header().Set("Content-Type", manifestsAccept)
		io.WriteString(w, `{"mediaType":"manifest"}`)

	case strings.Contains(r.URL.Path, "/blobs/"):
		if r.Method == http.MethodHead {
			atomic.AddInt32(&u.headHits, 1)
			w.Header().Set("Content-Length", fmt.Sprint(len(blobBytes)))
			w.Header().Set("Docker-Content-Digest", u.blobDigest())
			w.WriteHeader(http.StatusOK)
			return
		}
		atomic.AddInt32(&u.blobHits, 1)
		u.mu.Lock()
		u.lastRange = r.Header.Get("Range")
		u.mu.Unlock()
		body := blobBytes
		if u.tamperBlob {
			body = []byte("corrupted-corrupted-corrupted-xx") // 同长度，换内容
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if !u.noDigestHeader {
			w.Header().Set("Docker-Content-Digest", u.blobDigest())
		}
		w.Write(body)

	default:
		http.NotFound(w, r)
	}
}

type regFixture struct {
	gw    *Gateway
	st    *store.Store
	up    *regUpstream
	srv   *httptest.Server
	token string
	ad    *registryadapter.Adapter
}

func newRegFixture(t *testing.T, challenge bool) *regFixture {
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
	up := &regUpstream{challenge: challenge}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	ad := registryadapter.New(host, 4<<30, 5*time.Minute)
	ad.Scheme = "http"

	gw := New(st, Options{
		Cache:    cs,
		MaxBytes: 256 << 20,
		Registry: ad,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
		// 这组用例测的是 401 挑战与 Token 校验，必须显式要求认证
		RequireToken: true,
	})
	plain, _, err := st.CreateToken("docker")
	if err != nil {
		t.Fatalf("建 Token: %v", err)
	}
	return &regFixture{gw: gw, st: st, up: up, srv: srv, token: plain, ad: ad}
}

func (f *regFixture) do(t *testing.T, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://gw"+target, nil)
	r.SetBasicAuth("x", f.token) // docker login：用户名随意，密码位填 Token
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.gw.ServeRegistry(w, r)
	return w
}

func (f *regFixture) blobPath() string {
	return "/v2/library/alpine/blobs/" + f.up.blobDigest()
}

// 客户端带 Range（断点续传）：必须原样转给上游，且不能把半份内容落盘。
func TestRegistryBlobRangePassthrough(t *testing.T) {
	f := newRegFixture(t, false)

	req := httptest.NewRequest(http.MethodGet, "http://gw"+f.blobPath(), nil)
	plan, err := f.ad.Plan(req)
	if err != nil {
		t.Fatalf("构造 plan: %v", err)
	}

	w := f.do(t, http.MethodGet, f.blobPath(), map[string]string{"Range": "bytes=0-9"})
	if w.Code != http.StatusOK {
		t.Fatalf("上游忽略 Range 时应原样回 200，实际 %d %s", w.Code, w.Body.String())
	}
	f.up.mu.Lock()
	got := f.up.lastRange
	f.up.mu.Unlock()
	if got != "bytes=0-9" {
		t.Fatalf("Range 没转给上游，实际 %q", got)
	}
	// containerd 靠这个头核对内容，透传时不能丢。
	if got := w.Header().Get("Docker-Content-Digest"); got != f.up.blobDigest() {
		t.Fatalf("透传丢了 Docker-Content-Digest：%q", got)
	}
	if e, _ := f.st.CacheGet(plan.CacheKey); e != nil {
		t.Fatal("Range 请求不该落盘（只拿到一段，不是完整内容）")
	}
}

// 上游是 307 到 CDN 的话，响应里根本没有摘要头；网关要按请求路径补上，
// 否则 containerd 拿不到 Docker-Content-Digest。
func TestRegistryBlobRangeSynthesizesDigest(t *testing.T) {
	f := newRegFixture(t, false)
	f.up.noDigestHeader = true

	w := f.do(t, http.MethodGet, f.blobPath(), map[string]string{"Range": "bytes=0-9"})
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	if got, want := w.Header().Get("Docker-Content-Digest"), f.up.blobDigest(); got != want {
		t.Fatalf("应补上摘要头 %q，实际 %q", want, got)
	}
}

func TestRegistryPingNeedsToken(t *testing.T) {
	f := newRegFixture(t, false)

	r := httptest.NewRequest(http.MethodGet, "http://gw/v2/", nil)
	w := httptest.NewRecorder()
	f.gw.ServeRegistry(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，实际 %d", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("docker 只认 Basic 挑战，实际 %q", w.Header().Get("WWW-Authenticate"))
	}

	w2 := f.do(t, http.MethodGet, "/v2/", nil)
	if w2.Code != 200 {
		t.Fatalf("带 Token 应 200，实际 %d: %s", w2.Code, w2.Body.String())
	}
	if got := w2.Header().Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Fatalf("缺版本头: %q", got)
	}
}

func TestRegistryManifestMissThenHit(t *testing.T) {
	f := newRegFixture(t, false)
	target := "/v2/library/alpine/manifests/latest"
	hdr := map[string]string{"Accept": manifestsAccept}

	w1 := f.do(t, http.MethodGet, target, hdr)
	if w1.Code != 200 {
		t.Fatalf("首次 %d: %s", w1.Code, w1.Body.String())
	}
	if got := w1.Header().Get("X-ML-Cache"); got != "MISS" {
		t.Fatalf("首次回源应标 MISS，实际 %q", got)
	}
	if got := w1.Header().Get("Docker-Content-Digest"); !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("缺 digest 头: %q", got)
	}
	if got := w1.Header().Get("Content-Type"); got != manifestsAccept {
		t.Fatalf("清单媒体类型被改了: %q", got)
	}

	w2 := f.do(t, http.MethodGet, target, hdr)
	if w2.Body.String() != w1.Body.String() {
		t.Fatal("两次内容不一致")
	}
	if got := w2.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("第二次应标 HIT，实际 %q", got)
	}
	if n := atomic.LoadInt32(&f.up.manifestHits); n != 1 {
		t.Fatalf("第二次不该回源，上游被打了 %d 次", n)
	}
}

// tag 没有可信摘要 → unverified；按 digest 拉 → 摘要来自客户端，verified。
func TestRegistryVerifyState(t *testing.T) {
	f := newRegFixture(t, false)
	hdr := map[string]string{"Accept": manifestsAccept}

	if w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", hdr); w.Code != 200 {
		t.Fatalf("按 tag 拉: %d", w.Code)
	}
	if w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", hdr); w.Code != 200 {
		t.Fatalf("二次: %d", w.Code)
	}
	sum := sha256.Sum256([]byte(`{"mediaType":"manifest"}`))
	dig := "sha256:" + hex.EncodeToString(sum[:])
	if w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/"+dig, hdr); w.Code != 200 {
		t.Fatalf("按 digest 拉: %d %s", w.Code, w.Body.String())
	}

	es, err := f.st.CacheList(20)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range es {
		got[e.Key] = e.Verify
	}
	nTag, nDig := 0, 0
	for k, v := range got {
		switch {
		case strings.Contains(k, "sha256:"):
			if v != "verified" {
				t.Fatalf("按 digest 拉的清单应 verified，实际 %q (%s)", v, k)
			}
			nDig++
		default:
			if v != "unverified" {
				t.Fatalf("按 tag 拉的清单应 unverified，实际 %q (%s)", v, k)
			}
			nTag++
		}
	}
	if nTag == 0 || nDig == 0 {
		t.Fatalf("两类记录都该存在：tag=%d digest=%d", nTag, nDig)
	}
}

// 同一个 tag，Accept 不同拿到的是不同字节：索引 vs 单平台清单。
// 缓存键必须把这层差异带上，否则多架构镜像会拿到错的那份。
func TestRegistryManifestAcceptSeparated(t *testing.T) {
	f := newRegFixture(t, false)
	target := "/v2/library/alpine/manifests/latest"

	a := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept})
	b := f.do(t, http.MethodGet, target, map[string]string{"Accept": indexAccept})

	if a.Body.String() == b.Body.String() {
		t.Fatal("两种 Accept 拿到了同样的内容，说明上游桩没生效")
	}
	if n := atomic.LoadInt32(&f.up.manifestHits); n != 2 {
		t.Fatalf("应各回源一次，实际 %d", n)
	}
	// 再要一次单平台清单，应该命中 A 那条
	c := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept})
	if c.Body.String() != a.Body.String() {
		t.Fatal("回到原来的 Accept 没命中原来的缓存")
	}
	if n := atomic.LoadInt32(&f.up.manifestHits); n != 2 {
		t.Fatalf("命中不该回源，实际 %d", n)
	}
}

func TestRegistryBlobStreamAndHit(t *testing.T) {
	f := newRegFixture(t, false)

	w1 := f.do(t, http.MethodGet, f.blobPath(), nil)
	if w1.Code != 200 {
		t.Fatalf("首次 %d: %s", w1.Code, w1.Body.String())
	}
	if w1.Body.String() != string(blobBytes) {
		t.Fatalf("字节不一致: %q", w1.Body.String())
	}
	e, err := f.st.CacheGet("docker:blob:" + f.up.blobDigest())
	if err != nil || e == nil {
		t.Fatalf("应已入缓存: %v", err)
	}
	if e.SHA256 != strings.TrimPrefix(f.up.blobDigest(), "sha256:") {
		t.Fatalf("缓存的内容哈希和 digest 不一致: %s", e.SHA256)
	}

	w2 := f.do(t, http.MethodGet, f.blobPath(), nil)
	if w2.Body.String() != string(blobBytes) {
		t.Fatal("二次内容不一致")
	}
	if got := w2.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("二次应 HIT，实际 %q", got)
	}
	if n := atomic.LoadInt32(&f.up.blobHits); n != 1 {
		t.Fatalf("二次不该回源，上游被打了 %d 次", n)
	}
}

// 上游给的字节和 digest 对不上：客户端自己会校验，我们唯一要保证的是别写进缓存。
func TestRegistryBlobDigestMismatchNotCached(t *testing.T) {
	f := newRegFixture(t, false)
	f.up.tamperBlob = true

	w := f.do(t, http.MethodGet, f.blobPath(), nil)
	if w.Code != 200 {
		t.Fatalf("字节已经发出去了，状态应是 200，实际 %d", w.Code)
	}
	if e, _ := f.st.CacheGet("docker:blob:" + f.up.blobDigest()); e != nil {
		t.Fatal("校验失败的字节不该入缓存")
	}
	// 再来一次，还得回源（因为上次没缓存）
	f.do(t, http.MethodGet, f.blobPath(), nil)
	if n := atomic.LoadInt32(&f.up.blobHits); n != 2 {
		t.Fatalf("没缓存就该重新回源，实际 %d 次", n)
	}
}

func TestRegistryHeadBlobDoesNotFillCache(t *testing.T) {
	f := newRegFixture(t, false)

	w := f.do(t, http.MethodHead, f.blobPath(), nil)
	if w.Code != 200 {
		t.Fatalf("HEAD %d", w.Code)
	}
	if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(blobBytes)) {
		t.Fatalf("长度不对: %q", got)
	}
	if e, _ := f.st.CacheGet("docker:blob:" + f.up.blobDigest()); e != nil {
		t.Fatal("HEAD 不该落盘")
	}
	if n := atomic.LoadInt32(&f.up.headHits); n != 1 {
		t.Fatalf("应回源一次 HEAD，实际 %d", n)
	}
}

func TestRegistryPushRejected(t *testing.T) {
	f := newRegFixture(t, false)

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		w := f.do(t, m, "/v2/library/alpine/blobs/uploads/", nil)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s 应 405，实际 %d", m, w.Code)
		}
	}
}

func TestRegistryBearerChallenge(t *testing.T) {
	f := newRegFixture(t, true)

	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest",
		map[string]string{"Accept": manifestsAccept})
	if w.Code != 200 {
		t.Fatalf("换 token 后应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if n := atomic.LoadInt32(&f.up.tokenHits); n != 1 {
		t.Fatalf("token 应只申请一次，实际 %d", n)
	}
	// 第二个请求复用缓存里的 token，不再打 token 端点
	f.do(t, http.MethodGet, f.blobPath(), nil)
	if n := atomic.LoadInt32(&f.up.tokenHits); n != 1 {
		t.Fatalf("token 应复用，实际申请了 %d 次", n)
	}
}

func TestRegistryRejectsBadPaths(t *testing.T) {
	f := newRegFixture(t, false)
	for _, p := range []string{
		"/v2/library/alpine/blobs/sha256:short",
		"/v2/../etc/manifests/latest",
		"/v2/library/alpine/blobs/",
	} {
		w := f.do(t, http.MethodGet, p, nil)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Fatalf("%s 应被拒，实际 %d", p, w.Code)
		}
	}
}
