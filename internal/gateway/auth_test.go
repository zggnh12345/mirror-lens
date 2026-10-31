package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 匿名放行是默认：不带任何凭据也要能过，面板登录是另一条路。
func TestWithAuthAnonymousByDefault(t *testing.T) {
	g := &Gateway{} // requireToken 零值 false，也不碰 store
	called := false
	h := g.withAuth(func(w http.ResponseWriter, r *http.Request) { called = true })

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/raw/o/r/main/x", nil))

	if !called {
		t.Fatalf("匿名模式被拦了，状态码 %d", rec.Code)
	}
	if rec.Code != http.StatusOK && rec.Code != 0 {
		t.Fatalf("匿名模式返回了 %d", rec.Code)
	}
}

// require_token=true 时，无凭据必须 401，且带 Access Token 能过。
func TestWithAuthRequiresToken(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	tok, _, err := st.CreateToken("unit")
	if err != nil {
		t.Fatalf("建 token: %v", err)
	}

	g := &Gateway{st: st, requireToken: true}
	called := false
	h := g.withAuth(func(w http.ResponseWriter, r *http.Request) { called = true })

	// 无凭据 → 401，且带 Basic 挑战头（docker/git 靠它弹认证框）
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if called {
		t.Fatal("没有 token 却放行了")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，得到 %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 缺少 WWW-Authenticate")
	}

	// Bearer → 放行
	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h(rec, req)
	if !called {
		t.Fatalf("带 token 被拒了，状态码 %d", rec.Code)
	}

	// Basic 密码位放 token（docker login 的形式）→ 放行
	called = false
	req = httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.SetBasicAuth("token", tok)
	h(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("Basic 密码位带 token 被拒了")
	}
}
