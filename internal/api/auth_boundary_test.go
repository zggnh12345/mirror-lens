package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// newAuthServer 起一个「代理必须带 Token」的面板+网关，专测认证边界。
func newAuthServer(t *testing.T, requireToken bool) (*store.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := auth.EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(st, gateway.Options{
		Cache:        cs,
		RequireToken: requireToken,
		RawBase:      "http://127.0.0.1:1",
	})
	return st, New(st, runner.New(st), gw, nil, nil, WithMetrics(true, true))
}

func get(h http.Handler, path string, hdr map[string]string, ck *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

// 吊销的 Token 立刻失效，另一枚不受影响。
func TestRevokedTokenRejected(t *testing.T) {
	metrics.Enable()
	t.Cleanup(metrics.Disable)
	st, h := newAuthServer(t, true)

	tokA, recA, err := st.CreateToken("ci-a")
	if err != nil {
		t.Fatal(err)
	}
	tokB, _, err := st.CreateToken("ci-b")
	if err != nil {
		t.Fatal(err)
	}
	if r := get(h, "/metrics", bearer(tokA), nil); r.Code != 200 {
		t.Fatalf("未吊销前应可用，得到 %d", r.Code)
	}
	if err := st.RevokeToken(recA.ID); err != nil {
		t.Fatal(err)
	}
	if r := get(h, "/metrics", bearer(tokA), nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("吊销后应 401，得到 %d", r.Code)
	}
	if r := get(h, "/raw/o/r/main/f.txt", bearer(tokA), nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("吊销后代理路径也应 401，得到 %d", r.Code)
	}
	if r := get(h, "/metrics", bearer(tokB), nil); r.Code != 200 {
		t.Fatalf("另一枚 Token 不该被牵连，得到 %d", r.Code)
	}
}

// 空、缺位、乱码、错误 scheme：一律 401，不能因为解析失败就放行。
func TestMalformedTokenRejected(t *testing.T) {
	metrics.Enable()
	t.Cleanup(metrics.Disable)
	_, h := newAuthServer(t, true)

	cases := []map[string]string{
		{"Authorization": "Bearer "},
		{"Authorization": "Bearer"},
		{"Authorization": "Bearer not-a-real-token"},
		{"Authorization": "Bearer " + base64.StdEncoding.EncodeToString([]byte("admin:admin"))},
		{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(":"))},
		{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"))},
		{"Authorization": "Basic !!!not-base64!!!"},
		{"Authorization": "Token abc"},
	}
	for _, hdr := range cases {
		if r := get(h, "/metrics", hdr, nil); r.Code != http.StatusUnauthorized {
			t.Errorf("%v 应 401，得到 %d", hdr, r.Code)
		}
		if r := get(h, "/raw/o/r/main/f.txt", hdr, nil); r.Code != http.StatusUnauthorized {
			t.Errorf("%v 在代理路径应 401，得到 %d", hdr, r.Code)
		}
	}
}

// 代理 Token 不能当面板凭据用，面板会话也不能当代理凭据用。
func TestTokenAndSessionDoNotCross(t *testing.T) {
	st, h := newAuthServer(t, true)
	tok, _, err := st.CreateToken("proxy")
	if err != nil {
		t.Fatal(err)
	}

	// 代理 Token 打管理接口：守卫只看会话。
	if r := get(h, "/api/sources", bearer(tok), nil); r.Code == 200 {
		t.Fatal("代理 Token 不该能调管理接口")
	}
	if r := get(h, "/api/tokens", bearer(tok), nil); r.Code == 200 {
		t.Fatal("代理 Token 不该能列 Token")
	}

	// 面板会话打代理路径：网关只认 Bearer/Basic Token。
	if err := st.CreateSession("sess-proxy-boundary", 3600e9); err != nil {
		t.Fatal(err)
	}
	ck := &http.Cookie{Name: auth.CookieName, Value: "sess-proxy-boundary"}
	if r := get(h, "/raw/o/r/main/f.txt", nil, ck); r.Code != http.StatusUnauthorized {
		t.Fatalf("会话 cookie 不该当代理凭据，得到 %d", r.Code)
	}
}

// metrics.require_token 与 auth.require_token 各自独立：
// 代理匿名可访问时，指标仍要凭据；反过来也一样。
func TestMetricsTokenIndependentFromProxyAuth(t *testing.T) {
	metrics.Enable()
	t.Cleanup(metrics.Disable)
	st, h := newTestServerOpts(t, WithMetrics(true, true))

	// 代理默认匿名：不带凭据不是 401（这里没配上游，会是别的错误码）。
	if r := get(h, "/raw/o/r/main/f.txt", nil, nil); r.Code == http.StatusUnauthorized {
		t.Fatalf("代理未开 require_token 时不该 401，得到 %d", r.Code)
	}
	// 指标开了 require_token：必须 401。
	if r := get(h, "/metrics", nil, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("指标应独立要求凭据，得到 %d", r.Code)
	}

	tok, _, err := st.CreateToken("prom")
	if err != nil {
		t.Fatal(err)
	}
	if r := get(h, "/metrics", bearer(tok), nil); r.Code != 200 {
		t.Fatalf("带 Token 应 200，得到 %d", r.Code)
	}
}
