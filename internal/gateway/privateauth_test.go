package gateway

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// fakeCreds 是测试用的凭据来源：只按 Host 返回固定视图。
type fakeCreds struct {
	host string
	view CredentialView
	err  error
}

func (f *fakeCreds) setPassword(p string) { f.view.Password = p }

func (f *fakeCreds) CredentialView(host string) (CredentialView, error) {
	if f.err != nil {
		return CredentialView{}, f.err
	}
	if host != f.host {
		return CredentialView{}, nil
	}
	return f.view, nil
}

// privUpstream 是一个最小 registry：可选 Basic / Bearer 认证，记录每次请求。
type privUpstream struct {
	basicUser, basicPass string
	challenge            bool
	tokenUser, tokenPass string
	token                string
	// rejectTokens 让前 n 个 Bearer token 返回 401（测 401 失效刷新）。
	rejectTokens int32

	anon      int32
	basicHits int32
	bearerHit int32
	tokenHit  int32
	lastAuth  string
	tokenAuth string
	// publicCC 模拟私有 manifest 带一年 public 缓存头（真实 registry 行为）。
	publicCC bool
	// status 非 0 时直接返回该状态，用于 403/404/429 的“不重试/不认证”验收。
	status     int
	statusHits int32
}

func (u *privUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token":
		atomic.AddInt32(&u.tokenHit, 1)
		user, pass, ok := r.BasicAuth()
		u.tokenAuth = r.Header.Get("Authorization")
		if u.tokenUser != "" && (!ok || user != u.tokenUser || pass != u.tokenPass) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"token":"`+u.token+`","expires_in":300}`)
		return
	case r.URL.Path == "/v2/":
		io.WriteString(w, "{}")
		return
	}
	if u.status != 0 {
		atomic.AddInt32(&u.statusHits, 1)
		w.WriteHeader(u.status)
		return
	}
	if u.publicCC {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Surrogate-Key", "private-content")
	}
	auth := r.Header.Get("Authorization")
	u.lastAuth = auth
	if auth == "" {
		atomic.AddInt32(&u.anon, 1)
	}
	if u.basicUser != "" && strings.HasPrefix(auth, "Basic ") {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(u.basicUser+":"+u.basicPass))
		if auth == want {
			atomic.AddInt32(&u.basicHits, 1)
			io.WriteString(w, `{"schemaVersion":2}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if u.challenge {
		if auth == "Bearer "+u.token {
			if atomic.AddInt32(&u.rejectTokens, -1) > 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			atomic.AddInt32(&u.bearerHit, 1)
			io.WriteString(w, `{"schemaVersion":2}`)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token",service="test",scope="repository:library/alpine:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if u.basicUser != "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	io.WriteString(w, `{"schemaVersion":2}`)
}

// privPolicy 是测试用官方策略：显式带 CredGen，模拟 registry_host_gen 的真实读取。
type privPolicy struct{ gen int64 }

func (p privPolicy) Resolve(_ context.Context, sourceID int64, host string) (v2auth.PolicySnapshot, error) {
	if sourceID != 0 {
		return testGatewayPolicy.Resolve(context.Background(), sourceID, host)
	}
	return v2auth.PolicySnapshot{
		SourceID: 0, LogicalHost: host, Official: true, AllowSameHost: true,
		Scheme: "https", CredGen: p.gen, CredGenKnown: true,
		Realms: v2auth.DefaultRealmsFor(host),
	}, nil
}

type privFixture struct {
	gw    *Gateway
	st    *store.Store
	up    *privUpstream
	srv   *httptest.Server
	ad    *registryadapter.Adapter
	host  string
	token string
	cs    *cache.Store
	creds *fakeCreds
}

func newPrivFixture(t testing.TB, up *privUpstream, view CredentialView, enabled, requireToken bool) *privFixture {
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
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	ad := registryadapter.New(host, 4<<30, 5*time.Minute)
	ad.Scheme = "http"

	opts := PrivateAuthOptions{Enabled: enabled, Hosts: map[string]bool{host: true}, RequireToken: requireToken}
	var fc *fakeCreds
	if view.Host != "" {
		// 快照 Host 必须与策略 LogicalHost 一致，否则门禁按 Host 不一致拒绝。
		view.Host = host
		fc = &fakeCreds{host: host, view: view}
		opts.Creds = fc
	}
	gw := New(st, Options{
		Policies:     privPolicy{gen: 1},
		Cache:        cs,
		MaxBytes:     256 << 20,
		Registry:     ad,
		Guard:        safehttp.New(safehttp.Options{AllowLoopback: true}),
		RequireToken: requireToken,
		PrivateAuth:  opts,
	})
	plain, _, err := st.CreateToken("docker")
	if err != nil {
		t.Fatal(err)
	}
	return &privFixture{gw: gw, st: st, up: up, srv: srv, ad: ad, host: host, token: plain, cs: cs, creds: fc}
}

func (f *privFixture) do(t testing.TB, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://gw"+target, nil)
	r.SetBasicAuth("x", f.token)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.gw.ServeRegistry(w, r)
	return w
}

// F02：Registry 直接 Basic。匿名先发一次，401 后用 Basic 重试；不写共享缓存。
func TestPrivateAuthDirectBasicBypassesCache(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "s3cret"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "s3cret", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)

	target := "/v2/library/alpine/manifests/latest"
	w := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ML-Cache"); got != "BYPASS" {
		t.Fatalf("私有 Host 必须标 BYPASS，实际 %q", got)
	}
	if atomic.LoadInt32(&up.anon) == 0 {
		t.Fatal("必须先发一次匿名请求（匿名优先）")
	}
	if atomic.LoadInt32(&up.basicHits) != 1 {
		t.Fatalf("应恰好一次 Basic 认证，实际 %d", up.basicHits)
	}
	// 第二次请求仍必须绕过缓存：上游命中数增加，且缓存里没有该键。
	w2 := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept})
	if w2.Code != http.StatusOK {
		t.Fatalf("第二次期望 200，实际 %d", w2.Code)
	}
	if got := w2.Header().Get("X-ML-Cache"); got != "BYPASS" {
		t.Fatalf("第二次仍必须 BYPASS，实际 %q", got)
	}
	if atomic.LoadInt32(&up.basicHits) != 2 {
		t.Fatalf("第二次应再次回源，实际 Basic 命中 %d", up.basicHits)
	}
	if n, _ := f.st.CacheEpochStats(0); len(n) != 0 {
		t.Fatalf("私有内容不得进入共享缓存: %+v", n)
	}
}

// F03：Bearer + Token 端点 Basic。Basic 只发往 Token 端点。
func TestPrivateAuthBearerTokenBasic(t *testing.T) {
	up := &privUpstream{challenge: true, tokenUser: "robot", tokenPass: "s3cret", token: "tok-1"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "s3cret", Mode: "bearer_token_basic", TokenAuth: "basic"}
	f := newPrivFixture(t, up, view, true, true)

	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&up.tokenHit) != 1 {
		t.Fatalf("应换一次 Token，实际 %d", up.tokenHit)
	}
	if !strings.HasPrefix(up.tokenAuth, "Basic ") {
		t.Fatalf("Token 端点应带 Basic，实际 %q", up.tokenAuth)
	}
	if up.lastAuth != "Bearer tok-1" {
		t.Fatalf("资源请求应带 Bearer，实际 %q", up.lastAuth)
	}
}

// F17：带 Token 仍 401 → 删 key、重取一次、重试一次，不循环。
func TestPrivateAuthTokenRefreshOnce(t *testing.T) {
	up := &privUpstream{challenge: true, tokenUser: "robot", tokenPass: "s3cret", token: "tok-1", rejectTokens: 2}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "s3cret", Mode: "bearer_token_basic", TokenAuth: "basic"}
	f := newPrivFixture(t, up, view, true, true)

	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusOK {
		t.Fatalf("401 后应刷新并成功，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&up.tokenHit); got != 2 {
		t.Fatalf("应恰好换两次 Token，实际 %d", got)
	}
}

// F05：错误密码返回 401 auth_failed，不回退匿名缓存；负缓存生效。
func TestPrivateAuthWrongPasswordNegativeCache(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "right"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "wrong", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)

	target := "/v2/library/alpine/manifests/latest"
	w := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", w.Code)
	}
	if got := w.Header().Get("X-ML-Auth-Error"); got != "auth_failed" {
		t.Fatalf("错误码应为 auth_failed，实际 %q", got)
	}
	basic := atomic.LoadInt32(&up.basicHits)
	// 30s 内再次请求：负缓存拦截，不再尝试凭据（匿名探测仍会发一次，
	// 这是「匿名优先」的固有成本，但不会把账号打到锁定）。
	w2 := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept})
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("负缓存期间应仍 401，实际 %d", w2.Code)
	}
	if atomic.LoadInt32(&up.basicHits) != basic {
		t.Fatal("负缓存期间不应再次发送凭据")
	}
	if got := w2.Header().Get("X-ML-Auth-Error"); got != "auth_backoff" {
		t.Fatalf("负缓存错误码应为 auth_backoff，实际 %q", got)
	}
}

// S23/D-15-15：私有认证开启但 Access Token 被关闭 → 请求路径 fail closed。
func TestPrivateAuthFailsClosedWithoutToken(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "p"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true, Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, false)

	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("Access Token 关闭时私有认证必须 fail closed，实际 %d", w.Code)
	}
	if got := w.Header().Get("X-ML-Auth-Error"); got != "private_auth_requires_token" {
		t.Fatalf("错误码不对: %q", got)
	}
	if atomic.LoadInt32(&up.basicHits) != 0 {
		t.Fatal("不得发送任何凭据")
	}
}

// D-15-15：verify 不能绕过 private_auth_enabled / Access Token 开关。
func TestVerifyCredentialRequiresAccessToken(t *testing.T) {
	cases := []struct {
		name         string
		enabled      bool
		requireToken bool
		wantCode     string
	}{
		{name: "private auth disabled", enabled: false, requireToken: true, wantCode: "verify_private_auth_disabled"},
		{name: "access token disabled", enabled: true, requireToken: false, wantCode: "verify_requires_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &privUpstream{basicUser: "robot", basicPass: "p"}
			view := CredentialView{Host: "x", Generation: 1, HasCredential: true, Username: "robot", Password: "p", Mode: "basic"}
			f := newPrivFixture(t, up, view, tc.enabled, tc.requireToken)

			res := f.gw.VerifyCredential(context.Background(), f.host)
			if res.Status != http.StatusServiceUnavailable || res.Code != tc.wantCode {
				t.Fatalf("verify 必须 fail closed，得到 status=%d code=%q", res.Status, res.Code)
			}
			if atomic.LoadInt32(&up.basicHits) != 0 {
				t.Fatal("verify 不得在开关关闭时发送凭据")
			}
		})
	}
}

// P2-3：凭据状态错误必须保留具体分类，不能统一成 auth_stale。
func TestPrivateAuthCredentialStateErrorCode(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "p"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true, Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)
	f.creds.err = ErrCredPending

	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Pending 凭据应 503，实际 %d", w.Code)
	}
	if got := w.Header().Get("X-ML-Auth-Error"); got != "auth_cred_pending" {
		t.Fatalf("错误码应为 auth_cred_pending，实际 %q", got)
	}
	if atomic.LoadInt32(&up.basicHits) != 0 {
		t.Fatal("Pending 凭据不得向上游发送 Basic")
	}
}

// S02/S04：候选身份拿不到官方凭据（transport 层门禁）。
func TestPrivateAuthCandidateNeverUsesCredentials(t *testing.T) {
	var hits int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" {
			atomic.AddInt32(&hits, 1)
		}
		return authResponse(http.StatusUnauthorized, "", map[string]string{
			"WWW-Authenticate": `Bearer realm="https://quay.io/token",service="x"`,
		}), nil
	})
	tr := &registryAuthTransport{
		base: base, tokens: newTokenCache(), neg: newNegCache(),
		priv: PrivateAuthOptions{
			Enabled: true, RequireToken: true, Hosts: map[string]bool{"quay.io": true},
			Creds: &fakeCreds{host: "quay.io", view: CredentialView{
				Host: "quay.io", Generation: 1, HasCredential: true,
				Username: "u", Password: "p", Mode: "basic",
			}},
		},
	}
	cid, err := v2auth.NewCandidateIdentity(9, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	ctx := v2auth.WithPolicy(v2auth.WithIdentity(context.Background(), cid), v2auth.PolicySnapshot{
		SourceID: 9, LogicalHost: "quay.io", Official: false, CredGen: 1, CredGenKnown: true,
	})
	req := httptest.NewRequest(http.MethodGet, "https://quay.io/v2/x/manifests/1", nil)
	req = req.WithContext(v2auth.WithRegistry(ctx))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("候选身份不得发送任何凭据")
	}
}

// 凭据 gen 与策略不一致 → 503 auth_stale（fail closed，不回退匿名）。
func TestPrivateAuthStaleGenerationFailsClosed(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "p"}
	view := CredentialView{Host: "x", Generation: 5, HasCredential: true, Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)
	// 策略里的 gen 来自 DB（0），快照是 5：门禁必须拒绝。
	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("gen 不一致应 503，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ML-Auth-Error"); got != "auth_stale" {
		t.Fatalf("错误码应为 auth_stale，实际 %q", got)
	}
	if atomic.LoadInt32(&up.basicHits) != 0 {
		t.Fatal("不得发送凭据")
	}
}

// F01：未启用私有认证的 Host 行为不变（匿名 + 缓存）。
func TestPrivateAuthDisabledKeepsAnonymousCaching(t *testing.T) {
	up := &privUpstream{}
	f := newPrivFixture(t, up, CredentialView{}, false, true)
	target := "/v2/library/alpine/manifests/latest"
	w := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusOK {
		t.Fatalf("匿名应 200，实际 %d", w.Code)
	}
	if got := w.Header().Get("X-ML-Cache"); got == "BYPASS" {
		t.Fatal("未启用私有认证时不应绕过缓存")
	}
	w2 := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept})
	if got := w2.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("第二次应命中缓存，实际 %q", got)
	}
}

// epoch 键：适配器按注入的代次生成键，旧 epoch 不可命中。
func TestRegistryCacheEpochKeys(t *testing.T) {
	var epoch int64
	ad := registryadapter.New("registry.example", 1<<30, time.Minute)
	ad.Epoch = func() int64 { return atomic.LoadInt64(&epoch) }
	r := httptest.NewRequest(http.MethodGet, "http://gw/v2/quay.io/library/alpine/manifests/latest", nil)
	p1, err := ad.Plan(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p1.CacheKey, ":m:") {
		t.Fatalf("epoch=0 应保持旧键格式: %s", p1.CacheKey)
	}
	atomic.StoreInt64(&epoch, 2)
	p2, err := ad.Plan(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p2.CacheKey, "docker:m:2:") {
		t.Fatalf("epoch 键格式不对: %s", p2.CacheKey)
	}
	if store.RegistryCacheKeyEpoch(p2.CacheKey) != 2 || store.RegistryCacheKind(p2.CacheKey) != "manifest" {
		t.Fatalf("epoch 键解析失败: %s", p2.CacheKey)
	}
}

var _ = url.URL{}
var _ = adapter.Plan{}

// F19/S09：失败响应不得回显密码或 Authorization。
func TestPrivateAuthDoesNotEchoSecrets(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "right"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "wrong-secret-value", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)
	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", map[string]string{"Accept": manifestsAccept})
	all := w.Body.String()
	for k, vs := range w.Header() {
		all += k + ":" + strings.Join(vs, ",") + "\n"
	}
	for _, secret := range []string{"wrong-secret-value", "Basic ", "Bearer "} {
		if strings.Contains(all, secret) {
			t.Fatalf("响应泄露敏感内容 %q: %s", secret, all)
		}
	}
}

// 性能 A/B：匿名缓存命中路径 vs 私有 Host 强制回源路径。
func BenchmarkRegistryAnonymousCacheHit(b *testing.B) {
	f := newPrivFixture(b, &privUpstream{}, CredentialView{}, false, true)
	target := "/v2/library/alpine/manifests/latest"
	if w := f.do(b, http.MethodGet, target, map[string]string{"Accept": manifestsAccept}); w.Code != http.StatusOK {
		b.Fatalf("warm status=%d", w.Code)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if w := f.do(b, http.MethodGet, target, map[string]string{"Accept": manifestsAccept}); w.Code != http.StatusOK {
			b.Fatalf("status=%d", w.Code)
		}
	}
}

func BenchmarkRegistryPrivateBypass(b *testing.B) {
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(b, &privUpstream{basicUser: "robot", basicPass: "p"}, view, true, true)
	target := "/v2/library/alpine/manifests/latest"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if w := f.do(b, http.MethodGet, target, map[string]string{"Accept": manifestsAccept}); w.Code != http.StatusOK {
			b.Fatalf("status=%d", w.Code)
		}
	}
}

// P0 回归：跨 Host 重定向后不得重新认证（凭据与目标 Host 绑定）。
func TestPrivateAuthNoReauthAfterCrossHostRedirect(t *testing.T) {
	var seen []string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req.URL.Host+"|"+req.Header.Get("Authorization"))
		return authResponse(http.StatusUnauthorized, "", map[string]string{
			"WWW-Authenticate": `Basic realm="registry"`,
		}), nil
	})
	tr := &registryAuthTransport{
		base: base, tokens: newTokenCache(), neg: newNegCache(),
		priv: PrivateAuthOptions{
			Enabled: true, RequireToken: true, Hosts: map[string]bool{"quay.io": true},
			Creds: &fakeCreds{host: "quay.io", view: CredentialView{
				Host: "quay.io", Generation: 1, HasCredential: true,
				Username: "robot", Password: "s3cret", Mode: "basic",
			}},
		},
	}
	id, err := v2auth.NewOfficialIdentity("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	ctx := v2auth.WithPolicy(v2auth.WithIdentity(context.Background(), id), v2auth.PolicySnapshot{
		SourceID: 0, LogicalHost: "quay.io", Official: true, Scheme: "https",
		CredGen: 1, CredGenKnown: true,
	})
	// 模拟 http.Client 跟随 302 到另一台主机后的请求。
	req := httptest.NewRequest(http.MethodGet, "https://evil.example/v2/x/manifests/1", nil)
	req = req.WithContext(v2auth.WithRegistry(ctx))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, s := range seen {
		if strings.Contains(s, "evil.example|Basic") {
			t.Fatalf("跨 Host 重定向后把官方 Basic 发给了别的 Host: %s", s)
		}
		if strings.HasPrefix(s, "evil.example|") && strings.TrimPrefix(s, "evil.example|") != "" {
			t.Fatalf("跨 Host 请求带了认证头: %s", s)
		}
	}
}

// P0 回归：私有响应必须 no-store，不能透传上游的 public 缓存头。
func TestPrivateAuthResponseIsNoStore(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "p", publicCC: true}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)
	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("私有响应必须 no-store，实际 %q", got)
	}
	if w.Header().Get("Surrogate-Key") != "" {
		t.Fatal("私有响应不得带共享缓存标记 Surrogate-Key")
	}
}

// P1 回归：凭据改对后，旧负缓存必须被清掉（否则 5 分钟内仍 auth_backoff）。
func TestPrivateAuthInvalidateOnCredentialChange(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "right"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "wrong", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)
	target := "/v2/library/alpine/manifests/latest"

	if w := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept}); w.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", w.Code)
	}
	// 只改密码，不改 gen：只能靠显式失效。
	f.creds.setPassword("right")
	if w := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept}); w.Code != http.StatusUnauthorized {
		t.Fatalf("未失效前应仍被负缓存拦，实际 %d", w.Code)
	}
	f.gw.InvalidateCredentialCaches(f.host)
	if w := f.do(t, http.MethodGet, target, map[string]string{"Accept": manifestsAccept}); w.Code != http.StatusOK {
		t.Fatalf("失效后应立即生效，实际 %d", w.Code)
	}
}

type seqPolicy struct {
	calls int32
	gens  []int64
}

func (p *seqPolicy) Resolve(_ context.Context, sourceID int64, host string) (v2auth.PolicySnapshot, error) {
	n := atomic.AddInt32(&p.calls, 1)
	gen := p.gens[len(p.gens)-1]
	if int(n) <= len(p.gens) {
		gen = p.gens[n-1]
	}
	return v2auth.PolicySnapshot{
		SourceID: sourceID, LogicalHost: host, Official: true, AllowSameHost: true,
		Scheme: "https", CredGen: gen, CredGenKnown: true,
		Realms: v2auth.DefaultRealmsFor(host),
	}, nil
}

// C.1：策略解析后凭据换代，必须重解析一次；第二次 gen 一致后继续认证。
func TestPrivateAuthStaleReResolvesOnce(t *testing.T) {
	var basicHits int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" {
			atomic.AddInt32(&basicHits, 1)
			return authResponse(http.StatusOK, "ok", nil), nil
		}
		return authResponse(http.StatusUnauthorized, "", map[string]string{
			"WWW-Authenticate": `Basic realm="registry"`,
		}), nil
	})
	pol := &seqPolicy{gens: []int64{1, 2}}
	tr := &registryAuthTransport{
		base: base, tokens: newTokenCache(), neg: newNegCache(), resolver: pol,
		priv: PrivateAuthOptions{
			Enabled: true, RequireToken: true, Hosts: map[string]bool{"quay.io": true},
			Creds: &fakeCreds{host: "quay.io", view: CredentialView{
				Host: "quay.io", Generation: 2, HasCredential: true,
				Username: "robot", Password: "p", Mode: "basic",
			}},
		},
	}
	id, err := v2auth.NewOfficialIdentity("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	p0, err := pol.Resolve(context.Background(), 0, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	ctx := v2auth.WithPolicy(v2auth.WithIdentity(context.Background(), id), p0)
	req := httptest.NewRequest(http.MethodGet, "https://quay.io/v2/x/manifests/1", nil)
	req = req.WithContext(v2auth.WithRegistry(ctx))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale 后重解析应成功，实际 %d code=%s calls=%d", resp.StatusCode, resp.Header.Get("X-ML-Auth-Error"), atomic.LoadInt32(&pol.calls))
	}
	if got := atomic.LoadInt32(&pol.calls); got != 2 {
		t.Fatalf("应只重解析一次，实际调用 %d 次", got)
	}
	if got := atomic.LoadInt32(&basicHits); got != 1 {
		t.Fatalf("应发送一次 Basic，实际 %d", got)
	}
}

// F06/F07/F08：403/404/429 不是 401 Challenge，不能触发凭据认证或重试。
func TestPrivateAuthNonChallengeStatusDoesNotAuthenticate(t *testing.T) {
	for _, st := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(http.StatusText(st), func(t *testing.T) {
			up := &privUpstream{status: st, basicUser: "robot", basicPass: "p"}
			view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
				Username: "robot", Password: "p", Mode: "basic"}
			f := newPrivFixture(t, up, view, true, true)

			w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest",
				map[string]string{"Accept": manifestsAccept})
			if w.Code != st {
				t.Fatalf("应原样返回 %d，实际 %d（%s）", st, w.Code, w.Body.String())
			}
			if got := atomic.LoadInt32(&up.statusHits); got != 1 {
				t.Fatalf("只应请求上游一次，实际 %d", got)
			}
			if got := atomic.LoadInt32(&up.basicHits); got != 0 {
				t.Fatalf("非 401 不应发送 Basic，实际 %d", got)
			}
			if got := atomic.LoadInt32(&up.tokenHit); got != 0 {
				t.Fatalf("非 401 不应取 Token，实际 %d", got)
			}
			if got := w.Header().Get("X-ML-Cache"); got != "BYPASS" {
				t.Fatalf("私有 Host 必须 BYPASS，实际 %q", got)
			}
		})
	}
}

// F04：私有 Blob 与 Manifest 走同一认证/绕过缓存路径。
func TestPrivateAuthBlobBypassesCache(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "p"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)

	digest := strings.Repeat("a", 64)
	w := f.do(t, http.MethodGet, "/v2/library/alpine/blobs/sha256:"+digest, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("私有 Blob 应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ML-Cache"); got != "BYPASS" {
		t.Fatalf("私有 Blob 必须 BYPASS，实际 %q", got)
	}
	if got := atomic.LoadInt32(&up.basicHits); got != 1 {
		t.Fatalf("应发送一次 Basic，实际 %d", got)
	}
	if n, err := f.st.CacheList(10); err != nil || len(n) != 0 {
		t.Fatalf("私有 Blob 不应落缓存，count=%d err=%v", len(n), err)
	}
}

// C08：私有 BYPASS 与公共 HIT 并发时不能串缓存。
func TestPrivateAndPublicConcurrentDoNotShareCache(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "p"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)

	pubUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		io.WriteString(w, `{"schemaVersion":2,"public":true}`)
	}))
	defer pubUp.Close()
	pubHost := strings.TrimPrefix(pubUp.URL, "http://")
	pubAd := registryadapter.New(pubHost, 256<<20, 5*time.Minute)
	pubAd.Scheme = "http"
	pubGW := New(f.st, Options{
		Policies: privPolicy{gen: 1}, Cache: f.cs, MaxBytes: 256 << 20,
		Registry: pubAd, Guard: safehttp.New(safehttp.Options{AllowLoopback: true}),
		RequireToken: false, PrivateAuth: PrivateAuthOptions{Enabled: false},
	})

	// 先预热公共缓存，确认后续公共请求确实走 HIT。
	pubReq := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://gw/v2/library/alpine/manifests/latest", nil)
		r.Header.Set("Accept", manifestsAccept)
		w := httptest.NewRecorder()
		pubGW.ServeRegistry(w, r)
		return w
	}
	if w := pubReq(); w.Code != http.StatusOK || w.Header().Get("X-ML-Cache") != "MISS" {
		t.Fatalf("公共预热应 MISS/200，实际 %d cache=%q body=%s", w.Code, w.Header().Get("X-ML-Cache"), w.Body.String())
	}
	if w := pubReq(); w.Code != http.StatusOK || w.Header().Get("X-ML-Cache") != "HIT" {
		t.Fatalf("公共第二次应 HIT/200，实际 %d cache=%q", w.Code, w.Header().Get("X-ML-Cache"))
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest",
				map[string]string{"Accept": manifestsAccept})
			if w.Code != http.StatusOK || w.Header().Get("X-ML-Cache") != "BYPASS" {
				t.Errorf("私有并发请求应 200/BYPASS，实际 %d cache=%q", w.Code, w.Header().Get("X-ML-Cache"))
			}
		}()
		go func() {
			defer wg.Done()
			if w := pubReq(); w.Code != http.StatusOK || w.Header().Get("X-ML-Cache") != "HIT" {
				t.Errorf("公共并发请求应 200/HIT，实际 %d cache=%q", w.Code, w.Header().Get("X-ML-Cache"))
			}
		}()
	}
	wg.Wait()

	entries, err := f.st.CacheList(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("共享缓存应只有公共条目，实际 %d: %+v", len(entries), entries)
	}
	if strings.Contains(entries[0].Key, f.host) {
		t.Fatalf("私有 Host 不应进入共享缓存: %s", entries[0].Key)
	}
}

// F15：私有 HEAD 走同一认证与缓存绕过规则。
func TestPrivateAuthHeadKeepsIdentityAndBypass(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "p"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)

	w := f.do(t, http.MethodHead, "/v2/library/alpine/manifests/latest",
		map[string]string{"Accept": manifestsAccept})
	if w.Code != http.StatusOK {
		t.Fatalf("私有 HEAD 应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ML-Cache"); got != "BYPASS" {
		t.Fatalf("私有 HEAD 必须 BYPASS，实际 %q", got)
	}
	if got := atomic.LoadInt32(&up.basicHits); got != 1 {
		t.Fatalf("HEAD 应发送一次 Basic，实际 %d", got)
	}
	if n, err := f.st.CacheList(10); err != nil || len(n) != 0 {
		t.Fatalf("私有 HEAD 不应落缓存，count=%d err=%v", len(n), err)
	}
}

// F16：私有 Range/resume 请求仍走同一认证，不进入共享缓存。
func TestPrivateAuthRangeKeepsIdentityAndBypass(t *testing.T) {
	up := &privUpstream{basicUser: "robot", basicPass: "p"}
	view := CredentialView{Host: "x", Generation: 1, HasCredential: true,
		Username: "robot", Password: "p", Mode: "basic"}
	f := newPrivFixture(t, up, view, true, true)

	digest := strings.Repeat("b", 64)
	w := f.do(t, http.MethodGet, "/v2/library/alpine/blobs/sha256:"+digest,
		map[string]string{"Range": "bytes=0-1"})
	if w.Code != http.StatusOK && w.Code != http.StatusPartialContent {
		t.Fatalf("私有 Range 应 200/206，实际 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ML-Cache"); got != "BYPASS" {
		t.Fatalf("私有 Range 必须 BYPASS，实际 %q", got)
	}
	if got := atomic.LoadInt32(&up.basicHits); got != 1 {
		t.Fatalf("Range 应带同一份 Basic，实际 %d", got)
	}
	if n, err := f.st.CacheList(10); err != nil || len(n) != 0 {
		t.Fatalf("私有 Range 不应落缓存，count=%d err=%v", len(n), err)
	}
}
