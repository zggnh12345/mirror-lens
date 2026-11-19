package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 3.5.17 跨 Registry 副本映射 API：认证/CSRF、校验、冲突、审计。
func TestRegistryReplicaAPI(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)

	// 未登录拒绝。
	anon := httptest.NewRequest(http.MethodGet, "/api/registry-replicas", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, anon)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实得 %d", rec.Code)
	}

	body := `{"authority_registry":"registry.k8s.io","authority_repository":"kube-apiserver",` +
		`"replica_registry":"quay.io","replica_repository":"mirror/kube-apiserver","priority":10,"enabled":true}`

	// 缺 CSRF 的写请求必须拒绝。
	req := httptest.NewRequest(http.MethodPost, "/api/registry-replicas", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403，实得 %d %s", rec.Code, rec.Body.String())
	}

	// 正常新建 → 200 + generation。
	post := postJSON(h, "/api/registry-replicas", body, ck)
	if post.Code != http.StatusOK {
		t.Fatalf("新建应 200，实得 %d %s", post.Code, post.Body.String())
	}
	var created struct {
		OK         bool  `json:"ok"`
		Generation int64 `json:"generation"`
	}
	if err := json.Unmarshal(post.Body.Bytes(), &created); err != nil || !created.OK || created.Generation < 1 {
		t.Fatalf("新建响应不对: %s", post.Body.String())
	}

	// 重复 POST → 409。
	if dup := postJSON(h, "/api/registry-replicas", body, ck); dup.Code != http.StatusConflict {
		t.Fatalf("重复新建应 409，实得 %d %s", dup.Code, dup.Body.String())
	}

	// 通配符/非法仓库 → 400。
	bad := postJSON(h, "/api/registry-replicas",
		`{"authority_registry":"registry.k8s.io","authority_repository":"kube-*",`+
			`"replica_registry":"quay.io","replica_repository":"mirror/x","enabled":true}`, ck)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("通配符应 400，实得 %d %s", bad.Code, bad.Body.String())
	}

	// GET 返回 items 与代次。
	get := httptest.NewRequest(http.MethodGet, "/api/registry-replicas?authority=registry.k8s.io", nil)
	get.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询应 200，实得 %d", rec.Code)
	}
	var list struct {
		Items      []map[string]any `json:"items"`
		Generation int64            `json:"generation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(list.Items) != 1 || list.Generation < 1 {
		t.Fatalf("查询结果不对: %s", rec.Body.String())
	}

	// 删除 → 200；重复删除 → 404。
	del := httptest.NewRequest(http.MethodDelete,
		"/api/registry-replicas?authority=registry.k8s.io&repository=kube-apiserver&replica=quay.io", nil)
	del.AddCookie(ck)
	del.Header.Set("X-CSRF-Token", testCSRF)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, del)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应 200，实得 %d %s", rec.Code, rec.Body.String())
	}
	again := httptest.NewRequest(http.MethodDelete,
		"/api/registry-replicas?authority=registry.k8s.io&repository=kube-apiserver&replica=quay.io", nil)
	again.AddCookie(ck)
	again.Header.Set("X-CSRF-Token", testCSRF)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, again)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实得 %d", rec.Code)
	}

	// 审计：删除前后快照都要有。
	audit := httptest.NewRequest(http.MethodGet, "/api/registry-replica-audit?authority=registry.k8s.io", nil)
	audit.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, audit)
	if rec.Code != http.StatusOK {
		t.Fatalf("审计应 200，实得 %d", rec.Code)
	}
	var au struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &au)
	if len(au.Items) < 2 {
		t.Fatalf("审计至少 create+delete 两行，实得 %d", len(au.Items))
	}
}
