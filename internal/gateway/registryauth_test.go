package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// 全都没过期时也必须封顶：key 带 scope，仓库一多就会无限长。
func TestTokenCacheHardLimit(t *testing.T) {
	c := newTokenCache()
	now := time.Now()
	for i := 0; i < c.max+80; i++ {
		c.mu.Lock()
		c.pruneLocked()
		c.m[tokenKey{Host: fmt.Sprintf("k%d", i)}] = tokenEntry{tok: "t", exp: now.Add(time.Duration(i+1) * time.Hour)}
		c.mu.Unlock()
	}
	if n := len(c.m); n > c.max {
		t.Fatalf("token 缓存应封顶 %d，实际 %d", c.max, n)
	}
	// 淘汰的是最早到期的那批，最新写入的必须还在。
	if _, ok := c.m[tokenKey{Host: "k0"}]; ok {
		t.Fatal("最早到期的条目应被淘汰")
	}
	if _, ok := c.m[tokenKey{Host: fmt.Sprintf("k%d", c.max+79)}]; !ok {
		t.Fatal("最新写入的条目不该被淘汰")
	}
}

func authResponse(status int, body string, hdr map[string]string) *http.Response {
	h := make(http.Header)
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// GitLab 的 Challenge Realm 是 gitlab.com，而请求 Host 是
// registry.gitlab.com。不能因为跨 Host 就把 401 直接透传给客户端。
func TestBearerTransportAllowsGitLabRealm(t *testing.T) {
	var tokenHits, manifestHits int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "registry.gitlab.com":
			manifestHits++
			if req.Header.Get("Authorization") == "" {
				return authResponse(http.StatusUnauthorized, "", map[string]string{
					"WWW-Authenticate": `Bearer realm="https://gitlab.com/jwt/auth",service="container_registry",scope="repository:group/project:pull"`,
				}), nil
			}
			return authResponse(http.StatusOK, `{"ok":true}`, nil), nil
		case "gitlab.com":
			tokenHits++
			if req.URL.Query().Get("service") != "container_registry" ||
				req.URL.Query().Get("scope") != "repository:group/project:pull" {
				t.Fatalf("token 请求参数不对: %s", req.URL.RawQuery)
			}
			return authResponse(http.StatusOK, `{"token":"tok"}`, nil), nil
		default:
			return authResponse(http.StatusBadGateway, "unexpected host "+req.URL.Host, nil), nil
		}
	})
	bt := &bearerTransport{base: base, tokens: newTokenCache()}
	req := httptest.NewRequest(http.MethodGet, "https://registry.gitlab.com/v2/group/project/manifests/latest", nil)
	req = req.WithContext(v2auth.WithPolicy(req.Context(), v2auth.PolicySnapshot{LogicalHost: "registry.gitlab.com", AllowSameHost: true, Scheme: "https", Realms: []string{"https://gitlab.com"}}))
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || tokenHits != 1 || manifestHits != 2 {
		t.Fatalf("status=%d tokenHits=%d manifestHits=%d", resp.StatusCode, tokenHits, manifestHits)
	}
}

// 非内置白名单的跨 Host Realm 仍然拒绝，不能把凭据交给任意第三方。
func TestBearerTransportRejectsUntrustedRealm(t *testing.T) {
	var tokenHits int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "evil.example" {
			tokenHits++
			return authResponse(http.StatusOK, `{"token":"bad"}`, nil), nil
		}
		return authResponse(http.StatusUnauthorized, "", map[string]string{
			"WWW-Authenticate": `Bearer realm="https://evil.example/token",service="x"`,
		}), nil
	})
	bt := &bearerTransport{base: base, tokens: newTokenCache()}
	req := httptest.NewRequest(http.MethodGet, "https://registry.gitlab.com/v2/group/project/manifests/latest", nil)
	req = req.WithContext(v2auth.WithPolicy(req.Context(), v2auth.PolicySnapshot{LogicalHost: "registry.gitlab.com", AllowSameHost: true, Scheme: "https", Realms: []string{"https://gitlab.com"}}))
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || tokenHits != 0 {
		t.Fatalf("status=%d tokenHits=%d，应原样返回 401 且不请求 token", resp.StatusCode, tokenHits)
	}
}

// 没有策略快照时必须 fail closed 成 503，不能伪装成 401 或退回按 Host 查表。
func TestBearerTransportFailClosedWithoutPolicy(t *testing.T) {
	bt := &bearerTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("没有策略时不应发上游请求")
		return nil, nil
	}), tokens: newTokenCache()}
	req := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/g/p/manifests/latest", nil)
	req = req.WithContext(v2auth.WithRegistry(req.Context()))
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d，应为 503", resp.StatusCode)
	}
	if got := resp.Header.Get("X-ML-Error"); got != "auth_policy_missing" {
		t.Fatalf("X-ML-Error=%q", got)
	}
}

// 同一个 realm/service/scope 但不同 Source 必须各自换 token，不能跨源复用。
func TestBearerTransportTokenCacheSeparatesSources(t *testing.T) {
	var tokenHits int
	var auths []string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "auth.example":
			tokenHits++
			return authResponse(http.StatusOK, fmt.Sprintf(`{"token":"t%d"}`, tokenHits), nil), nil
		case "registry.example":
			if req.Header.Get("Authorization") == "" {
				return authResponse(http.StatusUnauthorized, "", map[string]string{
					"WWW-Authenticate": `Bearer realm="https://auth.example/token",service="registry",scope="repository:g/p:pull"`,
				}), nil
			}
			auths = append(auths, req.Header.Get("Authorization"))
			return authResponse(http.StatusOK, `{"ok":true}`, nil), nil
		default:
			return authResponse(http.StatusBadGateway, "unexpected host "+req.URL.Host, nil), nil
		}
	})
	bt := &bearerTransport{base: base, tokens: newTokenCache()}

	do := func(sourceID int64) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/g/p/manifests/latest", nil)
		req = req.WithContext(v2auth.WithPolicy(req.Context(), v2auth.PolicySnapshot{
			SourceID: sourceID, LogicalHost: "registry.example", Scheme: "https",
			Realms: []string{"https://auth.example"},
		}))
		resp, err := bt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("source=%d status=%d", sourceID, resp.StatusCode)
		}
	}
	do(1)
	do(2)

	if tokenHits != 2 {
		t.Fatalf("不同 Source 应各换一次 token，tokenHits=%d", tokenHits)
	}
	if len(auths) != 2 || auths[0] == auths[1] {
		t.Fatalf("不同 Source 不应复用同一个 token: %v", auths)
	}
}

func TestScopeFromPathStripsHostPrefix(t *testing.T) {
	cases := []struct {
		path, host, want string
	}{
		{"/v2/library/alpine/manifests/latest", "registry.k8s.io", "repository:library/alpine:pull"},
		{"/v2/registry.k8s.io/pause/manifests/3.9", "registry.k8s.io", "repository:pause:pull"},
		{"/v2/registry.k8s.io/pause/blobs/sha256:abc", "registry.k8s.io", "repository:pause:pull"},
		{"/v2/", "registry.k8s.io", ""},
	}
	for _, c := range cases {
		if got := scopeFromPath(c.path, c.host); got != c.want {
			t.Errorf("scopeFromPath(%q, %q)=%q，应为 %q", c.path, c.host, got, c.want)
		}
	}
}

func TestWithRegistryPolicyRejectsRegistryPlanWithoutLogicalHost(t *testing.T) {
	_, err := (&Gateway{}).withRegistryPolicy(context.Background(), &adapter.Plan{Registry: true}, 7, "", "")
	if err == nil || !errors.Is(err, v2auth.ErrPolicyMissing) {
		t.Fatalf("Registry plan 缺少 LogicalHost 应返回 ErrPolicyMissing，实际 %v", err)
	}
}

func TestWithRegistryPolicyNilPlan(t *testing.T) {
	ctx := context.Background()
	got, err := (&Gateway{}).withRegistryPolicy(ctx, nil, 0, "", "")
	if err != nil || got != ctx {
		t.Fatalf("nil plan 应原样返回 context: got=%v err=%v", got, err)
	}
	if _, ok := v2auth.PolicyFromContext(got); ok {
		t.Fatal("nil plan 不应写入策略")
	}
}

type failingPolicy struct{}

func (failingPolicy) Resolve(context.Context, int64, string) (v2auth.PolicySnapshot, error) {
	return v2auth.PolicySnapshot{}, errors.New("policy backend down")
}

func TestWithRegistryPolicyResolverError(t *testing.T) {
	g := &Gateway{policies: failingPolicy{}}
	_, err := g.withRegistryPolicy(context.Background(), &adapter.Plan{Registry: true, LogicalHost: "registry.example"}, 3, "", "")
	if !errors.Is(err, v2auth.ErrPolicyMissing) {
		t.Fatalf("resolver 失败必须包装成 ErrPolicyMissing，实际 %v", err)
	}
}

func TestWithRegistryPolicyUsesTargetScheme(t *testing.T) {
	g := &Gateway{}
	plan := &adapter.Plan{LogicalHost: "docker.io", Upstream: &url.URL{Scheme: "http"}}
	ctx, err := g.withRegistryPolicy(context.Background(), plan, 9, "", "https")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := v2auth.PolicyFromContext(ctx)
	if !ok || p.Scheme != "https" {
		t.Fatalf("策略 scheme 应取实际 target 的 https，得到 %+v ok=%v", p, ok)
	}
}

func TestWithRegistryPolicyCarriesSourceAndLogicalHost(t *testing.T) {
	g := &Gateway{}
	plan := &adapter.Plan{LogicalHost: "docker.io"}
	ctx, err := g.withRegistryPolicy(context.Background(), plan, 9, "", "")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := v2auth.PolicyFromContext(ctx)
	if !ok || p.SourceID != 9 || p.LogicalHost != "docker.io" || p.Official {
		t.Fatalf("策略身份不对: %+v ok=%v", p, ok)
	}
	ctx, err = g.withRegistryPolicy(context.Background(), plan, 9, "ghcr.io", "")
	if err != nil {
		t.Fatal(err)
	}
	p, _ = v2auth.PolicyFromContext(ctx)
	if p.LogicalHost != "ghcr.io" {
		t.Fatalf("fallback 逻辑 Host 应为 ghcr.io，实际 %q", p.LogicalHost)
	}
}

// Git/Private/Raw 共用 cli，但没有 Registry 标记时必须原样透传，不能被 503 掉。
func TestBearerTransportNonRegistryPassthrough(t *testing.T) {
	bt := &bearerTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return authResponse(http.StatusOK, "git", nil), nil
	}), tokens: newTokenCache()}
	req := httptest.NewRequest(http.MethodGet, "https://git.example/repo/info/refs", nil)
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("非 Registry 请求被改写: %d", resp.StatusCode)
	}
}

func TestBearerTransportRejectsMissingRealm(t *testing.T) {
	var tokenHits int
	bt := &bearerTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "auth.example" {
			tokenHits++
		}
		return authResponse(http.StatusUnauthorized, "", nil), nil
	}), tokens: newTokenCache()}
	req := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/g/p/manifests/latest", nil)
	req = req.WithContext(v2auth.WithPolicy(req.Context(), v2auth.PolicySnapshot{
		LogicalHost: "registry.example", AllowSameHost: true, Scheme: "https", Realms: []string{"https://auth.example"},
	}))
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || tokenHits != 0 {
		t.Fatalf("status=%d tokenHits=%d，无 Realm 不应请求 token", resp.StatusCode, tokenHits)
	}
}

func TestBearerTransportRejectsMalformedRealm(t *testing.T) {
	var tokenHits int
	bt := &bearerTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "auth.example" {
			tokenHits++
		}
		return authResponse(http.StatusUnauthorized, "", map[string]string{
			"WWW-Authenticate": `Bearer realm="http://[::1",service="registry"`,
		}), nil
	}), tokens: newTokenCache()}
	req := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/g/p/manifests/latest", nil)
	req = req.WithContext(v2auth.WithPolicy(req.Context(), v2auth.PolicySnapshot{
		LogicalHost: "registry.example", AllowSameHost: true, Scheme: "https", Realms: []string{"https://auth.example"},
	}))
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || tokenHits != 0 {
		t.Fatalf("status=%d tokenHits=%d，坏 Realm 不应请求 token", resp.StatusCode, tokenHits)
	}
}

func TestBearerTransportTokenRefreshErrorKeeps401(t *testing.T) {
	var tokenHits int
	bt := &bearerTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "registry.example":
			return authResponse(http.StatusUnauthorized, "", map[string]string{
				"WWW-Authenticate": `Bearer realm="https://auth.example/token",service="registry",scope="repository:g/p:pull"`,
			}), nil
		case "auth.example":
			tokenHits++
			return authResponse(http.StatusInternalServerError, "down", nil), nil
		default:
			return authResponse(http.StatusBadGateway, "unexpected", nil), nil
		}
	}), tokens: newTokenCache()}
	req := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/g/p/manifests/latest", nil)
	req = req.WithContext(v2auth.WithPolicy(req.Context(), v2auth.PolicySnapshot{
		LogicalHost: "registry.example", AllowSameHost: true, Scheme: "https", Realms: []string{"https://auth.example"},
	}))
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || tokenHits != 1 {
		t.Fatalf("status=%d tokenHits=%d，刷新失败应原样返回 401", resp.StatusCode, tokenHits)
	}
}
