package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

type redirectResolver struct {
	policy v2auth.PolicySnapshot
	err    error
	calls  int
}

func (r *redirectResolver) Resolve(_ context.Context, sourceID int64, host string) (v2auth.PolicySnapshot, error) {
	r.calls++
	if r.err != nil {
		return v2auth.PolicySnapshot{}, r.err
	}
	p := r.policy
	p.SourceID = sourceID
	p.LogicalHost = host
	return p, nil
}

func redirectContext(t *testing.T) context.Context {
	t.Helper()
	id, err := v2auth.NewOfficialIdentity("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	return v2auth.WithPolicy(v2auth.WithIdentity(context.Background(), id), v2auth.PolicySnapshot{
		SourceID: 0, LogicalHost: "quay.io", Official: true,
		CredGen: 1, CredGenKnown: true,
	})
}

func redirectURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 1.6 C.3：同 Host 重定向也要重新校验策略；策略变化必须 fail closed。
func TestRecheckRedirectPolicySameHost(t *testing.T) {
	from := redirectURL(t, "https://quay.io/v2/library/alpine/manifests/1")
	to := redirectURL(t, "https://quay.io/v2/library/alpine/manifests/2")

	t.Run("unchanged policy passes", func(t *testing.T) {
		r := &redirectResolver{policy: v2auth.PolicySnapshot{Official: true, CredGen: 1, CredGenKnown: true}}
		if err := recheckRedirectPolicy(redirectContext(t), r, from, to); err != nil {
			t.Fatalf("策略未变化应放行: %v", err)
		}
		if r.calls != 1 {
			t.Fatalf("应重新解析一次策略，实际 %d", r.calls)
		}
	})

	t.Run("official flag changed rejects", func(t *testing.T) {
		r := &redirectResolver{policy: v2auth.PolicySnapshot{Official: false, CredGen: 1, CredGenKnown: true}}
		if err := recheckRedirectPolicy(redirectContext(t), r, from, to); !errors.Is(err, v2auth.ErrPolicyMissing) {
			t.Fatalf("Official 变化应拒绝，得到 %v", err)
		}
	})

	t.Run("credential generation changed rejects", func(t *testing.T) {
		r := &redirectResolver{policy: v2auth.PolicySnapshot{Official: true, CredGen: 2, CredGenKnown: true}}
		if err := recheckRedirectPolicy(redirectContext(t), r, from, to); !errors.Is(err, v2auth.ErrPolicyMissing) {
			t.Fatalf("CredGen 变化应拒绝，得到 %v", err)
		}
	})

	t.Run("resolver error rejects", func(t *testing.T) {
		r := &redirectResolver{err: errors.New("policy unavailable")}
		if err := recheckRedirectPolicy(redirectContext(t), r, from, to); err == nil {
			t.Fatal("策略解析失败应拒绝")
		}
	})
}

// 跨 Host 不重新认证：这里不重解析策略，Authorization 由 RedirectPolicy 清除。
func TestRecheckRedirectPolicyCrossHostSkips(t *testing.T) {
	from := redirectURL(t, "https://quay.io/v2/library/alpine/manifests/1")
	to := redirectURL(t, "https://cdn.example/blob")
	r := &redirectResolver{policy: v2auth.PolicySnapshot{Official: false}}
	if err := recheckRedirectPolicy(redirectContext(t), r, from, to); err != nil {
		t.Fatalf("跨 Host 不应在这里拒绝: %v", err)
	}
	if r.calls != 0 {
		t.Fatalf("跨 Host 不应重解析策略，实际 %d", r.calls)
	}
}

func TestRecheckRedirectPolicyMissingContext(t *testing.T) {
	from := redirectURL(t, "https://quay.io/v2/library/alpine/manifests/1")
	to := redirectURL(t, "https://quay.io/v2/library/alpine/manifests/2")
	r := &redirectResolver{policy: v2auth.PolicySnapshot{Official: true}}
	if err := recheckRedirectPolicy(context.Background(), r, from, to); !errors.Is(err, v2auth.ErrPolicyMissing) {
		t.Fatalf("缺少策略上下文应拒绝，得到 %v", err)
	}
}

func TestApplyRedirectPolicyCDN(t *testing.T) {
	from := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/library/alpine/blobs/sha256:abc", nil)
	to := httptest.NewRequest(http.MethodGet, "https://cdn.example/blob?X-Amz-Signature=abc", nil)
	to.Header.Set("Authorization", "Bearer secret")
	to.Header.Set("Cookie", "session=1")

	if err := applyRedirectPolicy(to, []*http.Request{from}, true, nil); err != nil {
		t.Fatalf("CDN 重定向应放行: %v", err)
	}
	if got := to.Header.Get("Authorization"); got != "" {
		t.Fatalf("CDN 重定向不得带 Authorization，得到 %q", got)
	}
	if got := to.Header.Get("Cookie"); got != "" {
		t.Fatalf("CDN 重定向不得带 Cookie，得到 %q", got)
	}
	if got := to.URL.RawQuery; got != "X-Amz-Signature=abc" {
		t.Fatalf("签名 query 必须保留，得到 %q", got)
	}
}

func TestApplyRedirectPolicyCDNRejectsUnsafe(t *testing.T) {
	from := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/library/alpine/blobs/sha256:abc", nil)

	httpTo := httptest.NewRequest(http.MethodGet, "http://cdn.example/blob", nil)
	if err := applyRedirectPolicy(httpTo, []*http.Request{from}, true, nil); err == nil {
		t.Fatal("CDN HTTPS 降级必须拒绝")
	}

	post := httptest.NewRequest(http.MethodPost, "https://cdn.example/blob", nil)
	if err := applyRedirectPolicy(post, []*http.Request{from}, true, nil); err == nil {
		t.Fatal("CDN POST 必须拒绝")
	}
}

func TestApplyRedirectPolicyRegistryCrossHostStripsAuth(t *testing.T) {
	from := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/x/manifests/1", nil)
	to := httptest.NewRequest(http.MethodGet, "https://mirror.example/v2/x/manifests/1", nil)
	to.Header.Set("Authorization", "Basic secret")
	if err := applyRedirectPolicy(to, []*http.Request{from}, false, nil); err != nil {
		t.Fatalf("跨 Host Registry 重定向应放行: %v", err)
	}
	if got := to.Header.Get("Authorization"); got != "" {
		t.Fatalf("跨 Host 必须清 Authorization，得到 %q", got)
	}
}

func TestIsBlobPlan(t *testing.T) {
	cases := []struct {
		plan *adapter.Plan
		want bool
	}{
		{&adapter.Plan{Registry: true, ResourceKind: adapter.ResourceBlob, CacheKey: "docker:blob:sha256:abc"}, true},
		{&adapter.Plan{Registry: true, ResourceKind: adapter.ResourceManifest, CacheKey: "docker:manifest:x"}, false},
		{&adapter.Plan{Registry: false, ResourceKind: adapter.ResourceBlob, CacheKey: "docker:blob:sha256:abc"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isBlobPlan(tc.plan); got != tc.want {
			t.Fatalf("isBlobPlan(%+v)=%v want %v", tc.plan, got, tc.want)
		}
	}
}

// 本地 TLS 302 fixture：Registry blob 跳到 CDN，验证 CDN 收不到认证头/Cookie，
// 且签名 query 保留。测试 CA 只注入本测试的 RootCAs，不关闭 TLS 校验。
func TestRedirectCDN302Fixture(t *testing.T) {
	var registrySawAuth, cdnSawAuth, cdnSawCookie bool
	var cdnQuery string
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnSawAuth = r.Header.Get("Authorization") != ""
		cdnSawCookie = r.Header.Get("Cookie") != ""
		cdnQuery = r.URL.RawQuery
		io.WriteString(w, "hello")
	}))
	defer cdn.Close()

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registrySawAuth = r.Header.Get("Authorization") != ""
		http.Redirect(w, r, cdn.URL+"/blob?sig=abc", http.StatusFound)
	}))
	defer registry.Close()

	pool := x509.NewCertPool()
	pool.AddCert(registry.Certificate())
	pool.AddCert(cdn.Certificate())
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return applyRedirectPolicy(r, via, true, nil)
		},
	}
	req, err := http.NewRequest(http.MethodGet, registry.URL+"/v2/library/alpine/blobs/sha256:abc", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("跟随 CDN 302 失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("CDN 响应异常: status=%d body=%q", resp.StatusCode, body)
	}
	if !registrySawAuth {
		t.Fatal("首跳应带 Authorization，才能证明重定向后是被清掉的")
	}
	if cdnSawAuth || cdnSawCookie {
		t.Fatalf("CDN 不得收到认证头/Cookie: auth=%v cookie=%v", cdnSawAuth, cdnSawCookie)
	}
	if cdnQuery != "sig=abc" {
		t.Fatalf("签名 query 未保留: %q", cdnQuery)
	}
}

// openUpstream 端到端：blob plan 的 302 必须走 CDN 规则，而不是只测策略函数。
func TestOpenUpstreamCDN302(t *testing.T) {
	var registrySawAuth, cdnSawAuth, cdnSawCookie bool
	var cdnQuery string
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnSawAuth = r.Header.Get("Authorization") != ""
		cdnSawCookie = r.Header.Get("Cookie") != ""
		cdnQuery = r.URL.RawQuery
		io.WriteString(w, "hello")
	}))
	defer cdn.Close()

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registrySawAuth = r.Header.Get("Authorization") != ""
		http.Redirect(w, r, cdn.URL+"/blob?sig=abc", http.StatusFound)
	}))
	defer registry.Close()

	pool := x509.NewCertPool()
	pool.AddCert(registry.Certificate())
	pool.AddCert(cdn.Certificate())
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return applyRedirectPolicy(r, via, isCDNRedirect(r.Context()), nil)
		},
	}
	g := &Gateway{cli: client, policies: privPolicy{gen: 1}}
	plan := &adapter.Plan{
		Registry:     true,
		ResourceKind: adapter.ResourceBlob,
		LogicalHost:  "registry.example",
		CacheKey:     "docker:blob:sha256:abc",
		Header:       http.Header{"Authorization": []string{"Bearer secret"}, "Cookie": []string{"session=1"}},
	}
	u, err := url.Parse(registry.URL + "/v2/library/alpine/blobs/sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	up, err := g.openUpstream(context.Background(), plan, nil, u, 0, "registry.example")
	if err != nil {
		t.Fatalf("openUpstream 失败: %v", err)
	}
	defer up.cancel()
	defer up.resp.Body.Close()
	if up.resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", up.resp.StatusCode)
	}
	if !registrySawAuth {
		t.Fatal("首跳应带 Authorization")
	}
	if cdnSawAuth || cdnSawCookie {
		t.Fatalf("CDN 不得收到认证头/Cookie: auth=%v cookie=%v", cdnSawAuth, cdnSawCookie)
	}
	if cdnQuery != "sig=abc" {
		t.Fatalf("签名 query 未保留: %q", cdnQuery)
	}
}
