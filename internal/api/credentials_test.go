package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/secret"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func newCredAPIFixture(t *testing.T) (*store.Store, *secret.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := auth.EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}
	key, err := secret.LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := secret.NewCredStore(filepath.Join(dir, "registry_creds.enc"), key, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.ReconcileAll(); err != nil {
		t.Fatal(err)
	}
	cc, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(st, gateway.Options{Cache: cc})
	h := New(st, runner.New(st), gw, nil, nil, WithCredentials(cs))
	return st, cs, h
}

func credReq(h http.Handler, method, path, body string, ck *http.Cookie, csrf, https bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-CSRF-Token", testCSRF)
	}
	if https {
		req.Header.Set("X-Forwarded-Proto", "https")
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 凭据 API 的认证、CSRF、HTTPS 与 no-store 边界。
func TestCredentialsAPIRequiresAdminAndHTTPS(t *testing.T) {
	_, cs, h := newCredAPIFixture(t)
	ck := setupPanel(t, h)

	// 匿名：401。
	if rec := credReq(h, http.MethodGet, "/api/credentials", "", nil, false, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名应 401，实际 %d", rec.Code)
	}
	// 无 CSRF：403。
	rec := credReq(h, http.MethodPut, "/api/credentials/quay.io",
		`{"mode":"basic","username":"u","password":"p"}`, ck, false, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403，实际 %d", rec.Code)
	}
	// 纯 HTTP（无 X-Forwarded-Proto）：403 https_required。
	rec = credReq(h, http.MethodPut, "/api/credentials/quay.io",
		`{"mode":"basic","username":"u","password":"p"}`, ck, true, false)
	if rec.Code != http.StatusForbidden || code(t, rec) != "https_required" {
		t.Fatalf("纯 HTTP 写入应 403 https_required，实际 %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := cs.Get("quay.io"); ok {
		t.Fatal("被拒绝的写入不得落盘")
	}
}

// 新增 / 幂等更新 / expected_gen / 省略密码 / 删除。
func TestCredentialsAPILifecycle(t *testing.T) {
	st, cs, h := newCredAPIFixture(t)
	ck := setupPanel(t, h)

	rec := credReq(h, http.MethodPut, "/api/credentials/quay.io",
		`{"mode":"bearer_token_basic","username":"robot","password":"s3cret"}`, ck, true, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("新增应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("凭据 API 必须 no-store，实际 %q", got)
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatal("响应体不得包含密码")
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	gen := int64(out["gen"].(float64))
	if gen != 1 {
		t.Fatalf("首次 gen 应为 1，实际 %v", gen)
	}

	// 更新不带 expected_gen → 409。
	rec = credReq(h, http.MethodPut, "/api/credentials/quay.io",
		`{"mode":"bearer_token_basic","username":"robot","password":"new"}`, ck, true, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("更新缺 expected_gen 应 409，实际 %d", rec.Code)
	}
	// 旧 gen → 409。
	rec = credReq(h, http.MethodPut, "/api/credentials/quay.io",
		`{"mode":"bearer_token_basic","username":"robot","password":"new","expected_gen":0}`, ck, true, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("旧 expected_gen 应 409，实际 %d", rec.Code)
	}
	// 省略密码 + 正确 gen → 保留原密码。
	rec = credReq(h, http.MethodPut, "/api/credentials/quay.io",
		`{"mode":"bearer_token_basic","username":"robot2","expected_gen":1}`, ck, true, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("省略密码更新应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	c, ok, err := cs.Get("quay.io")
	if err != nil || !ok {
		t.Fatalf("更新后应可读: ok=%v err=%v", ok, err)
	}
	if c.Password != "s3cret" || c.Username != "robot2" {
		t.Fatalf("省略密码应保留旧密码: %+v", c)
	}

	// 列表：有状态、无密码字段。
	rec = credReq(h, http.MethodGet, "/api/credentials", "", ck, false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "s3cret") || strings.Contains(body, "\"password\"") {
		t.Fatalf("列表不得返回密码: %s", body)
	}
	if !strings.Contains(body, "quay.io") || !strings.Contains(body, "Ready") {
		t.Fatalf("列表缺少状态: %s", body)
	}

	// 删除 → 204；重复删除 → 204（幂等）。
	rec = credReq(h, http.MethodDelete, "/api/credentials/quay.io", "", ck, true, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("删除应 204，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = credReq(h, http.MethodDelete, "/api/credentials/quay.io", "", ck, true, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("重复删除应 204，实际 %d", rec.Code)
	}

	// 审计：有 put/delete，且不含密码。
	rows, err := st.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	var put, del int
	for _, a := range rows {
		if strings.Contains(a.Detail, "s3cret") {
			t.Fatalf("审计泄露密码: %+v", a)
		}
		switch a.Action {
		case "credential_put":
			put++
			if a.User == "" || a.IP == "" {
				t.Fatalf("凭据写入审计缺少操作者/IP: %+v", a)
			}
		case "credential_delete":
			del++
			if a.User == "" || a.IP == "" {
				t.Fatalf("凭据删除审计缺少操作者/IP: %+v", a)
			}
		}
	}
	if put < 2 || del < 1 {
		t.Fatalf("审计缺失: put=%d delete=%d", put, del)
	}
}

// allowlist 之外的 Host 不得通过 API 写入。
func TestCredentialsAPIAllowlist(t *testing.T) {
	_, _, h := newCredAPIFixture(t)
	ck := setupPanel(t, h)
	for _, host := range []string{"lscr.io", "registry.gitlab.com", "asia-east1-docker.pkg.dev", "docker.io"} {
		rec := credReq(h, http.MethodPut, "/api/credentials/"+host,
			`{"mode":"basic","username":"u","password":"p"}`, ck, true, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d %s", host, rec.Code, rec.Body.String())
		}
	}
}
