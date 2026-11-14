package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 全都没过期时也必须封顶：key 带 scope，仓库一多就会无限长。
func TestTokenCacheHardLimit(t *testing.T) {
	c := newTokenCache()
	now := time.Now()
	for i := 0; i < tokenCacheMax+80; i++ {
		c.mu.Lock()
		c.pruneLocked()
		c.m[fmt.Sprintf("k%d", i)] = tokenEntry{tok: "t", exp: now.Add(time.Duration(i+1) * time.Hour)}
		c.mu.Unlock()
	}
	if n := len(c.m); n > tokenCacheMax {
		t.Fatalf("token 缓存应封顶 %d，实际 %d", tokenCacheMax, n)
	}
	// 淘汰的是最早到期的那批，最新写入的必须还在。
	if _, ok := c.m["k0"]; ok {
		t.Fatal("最早到期的条目应被淘汰")
	}
	if _, ok := c.m[fmt.Sprintf("k%d", tokenCacheMax+79)]; !ok {
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
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || tokenHits != 0 {
		t.Fatalf("status=%d tokenHits=%d，应原样返回 401 且不请求 token", resp.StatusCode, tokenHits)
	}
}
