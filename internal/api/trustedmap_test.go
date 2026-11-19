package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 3.5.17 F08 管理 API：认证边界、校验错误码、事务结果。
func TestTrustedMappingAPI(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)
	src, err := st.AddSource(store.Source{BaseURL: "https://mirror.example.org", Name: "m", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}

	// 未登录必须拒绝（单 admin 模型，没有角色分级；这里验的是入口边界）。
	anon := httptest.NewRequest(http.MethodGet, "/api/trusted-mappings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, anon)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实得 %d", rec.Code)
	}

	// 已登录但缺 CSRF：写请求必须拒绝。
	body := `{"registry_host":"quay.io","source_id":` + itoa(src) + `,"priority":5,"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/trusted-mappings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403，实得 %d %s", rec.Code, rec.Body.String())
	}

	// 正常新建 → 200 且带 Generation。
	post := postJSON(h, "/api/trusted-mappings", body, ck)
	if post.Code != http.StatusOK {
		t.Fatalf("新建应 200，实得 %d %s", post.Code, post.Body.String())
	}
	var created struct {
		OK         bool  `json:"ok"`
		Generation int64 `json:"generation"`
	}
	_ = json.Unmarshal(post.Body.Bytes(), &created)
	if !created.OK || created.Generation < 1 {
		t.Fatalf("新建响应不对: %s", post.Body.String())
	}

	// 重复 POST 必须 409。
	if dup := postJSON(h, "/api/trusted-mappings", body, ck); dup.Code != http.StatusConflict {
		t.Fatalf("重复新建应 409，实得 %d", dup.Code)
	}

	// 非法输入（不存在的 source）→ 400。
	bad := postJSON(h, "/api/trusted-mappings", `{"registry_host":"quay.io","source_id":99999}`, ck)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("无效 Source 应 400，实得 %d", bad.Code)
	}

	// GET 返回 items 与 generations。
	get := httptest.NewRequest(http.MethodGet, "/api/trusted-mappings?host=quay.io", nil)
	get.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询应 200，实得 %d", rec.Code)
	}
	var list struct {
		Items       []map[string]any `json:"items"`
		Generations map[string]int64 `json:"generations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(list.Items) != 1 || list.Generations["quay.io"] < 1 {
		t.Fatalf("查询结果不对: %s", rec.Body.String())
	}

	// 审计必须有 create 行且带操作者。
	auditReq := httptest.NewRequest(http.MethodGet, "/api/trusted-mapping-audit?host=quay.io", nil)
	auditReq.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, auditReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("审计应 200，实得 %d", rec.Code)
	}
	var audit struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &audit)
	if len(audit.Items) == 0 || audit.Items[0]["operation"] != "create" || audit.Items[0]["actor"] == "" {
		t.Fatalf("审计内容不对: %s", rec.Body.String())
	}

	// DELETE 成功；再删返回 404。
	del := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/trusted-mappings?host=quay.io&source_id="+itoa(src), nil)
		req.Header.Set("X-CSRF-Token", testCSRF)
		req.AddCookie(ck)
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		return r
	}
	if d := del(); d.Code != http.StatusOK {
		t.Fatalf("删除应 200，实得 %d %s", d.Code, d.Body.String())
	}
	if d := del(); d.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实得 %d", d.Code)
	}
}
