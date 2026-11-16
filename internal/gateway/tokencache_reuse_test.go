package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// 同一 Source、同一 Realm/Service/Scope，但 LogicalHost 不同：token 不能共享。
func TestTokenCacheSeparatesHosts(t *testing.T) {
	var tokenHits int
	var auths []string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "auth.example":
			tokenHits++
			return authResponse(http.StatusOK, fmt.Sprintf(`{"token":"h%d"}`, tokenHits), nil), nil
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
	do := func(host string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/g/p/manifests/latest", nil)
		req = req.WithContext(v2auth.WithPolicy(req.Context(), v2auth.PolicySnapshot{
			SourceID: 1, LogicalHost: host, Scheme: "https",
			Realms: []string{"https://auth.example"},
		}))
		resp, err := bt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("host=%s status=%d", host, resp.StatusCode)
		}
	}
	do("registry.example")
	do("registry2.example")

	if tokenHits != 2 {
		t.Fatalf("同一 Source 的不同 Host 应各换一次 token，tokenHits=%d", tokenHits)
	}
	if len(auths) != 2 || auths[0] == auths[1] {
		t.Fatalf("不同 Host 不应复用同一个 token: %v", auths)
	}
}

// 同一 Source/Host/PolicyGen，但 CredGen 不同（凭据轮换）：旧 token 不能复用。
func TestTokenCacheSeparatesCredGenerations(t *testing.T) {
	var tokenHits int
	var auths []string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "auth.example":
			tokenHits++
			return authResponse(http.StatusOK, fmt.Sprintf(`{"token":"c%d"}`, tokenHits), nil), nil
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
	do := func(credGen int64) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/g/p/manifests/latest", nil)
		req = req.WithContext(v2auth.WithPolicy(req.Context(), v2auth.PolicySnapshot{
			SourceID: 1, LogicalHost: "registry.example", Scheme: "https",
			CredGen: credGen, PolicyGen: 1,
			Realms: []string{"https://auth.example"},
		}))
		resp, err := bt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("credGen=%d status=%d", credGen, resp.StatusCode)
		}
	}
	do(1)
	do(2)

	if tokenHits != 2 {
		t.Fatalf("CredGen 变化应各换一次 token，tokenHits=%d", tokenHits)
	}
	if len(auths) != 2 || auths[0] == auths[1] {
		t.Fatalf("CredGen 变化不应复用旧 token: %v", auths)
	}
}
