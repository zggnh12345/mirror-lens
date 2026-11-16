package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

func policyReq(h http.Handler, method, path, body string, ck *http.Cookie, csrf bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-CSRF-Token", testCSRF)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 12c：Source 级策略的读、改、审计、删除与 fail closed。
func TestPolicyAPISourceLevel(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	id, err := st.AddSource(store.Source{
		Name: "quay-mirror", BaseURL: "https://mirror.example", Status: "usable",
		Enabled: true, RepoHost: "quay.io",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := get(h, "/api/policies", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("Host 汇总应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "quay.io") {
		t.Fatalf("Host 汇总缺少 quay.io: %s", rec.Body.String())
	}

	rec = get(h, "/api/policies?host=quay.io", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("策略列表应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "quay-mirror") || !strings.Contains(rec.Body.String(), "primary_realm") {
		t.Fatalf("策略列表内容不对: %s", rec.Body.String())
	}

	body := `{"primary_realm":"https://quay.io","extra_realms":["https://mirror.example"]}`
	rec = policyReq(h, http.MethodPut, "/api/policies/"+itoa(id)+"?host=quay.io", body, ck, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新策略应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	p, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if p.PrimaryRealm != "https://quay.io" || len(p.ExtraRealms) != 1 || p.ExtraRealms[0] != "https://mirror.example" {
		t.Fatalf("策略未落库: %+v", p)
	}

	rec = policyReq(h, http.MethodPut, "/api/policies/"+itoa(id)+"?host=gcr.io", body, ck, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("host 与 Source 归属不一致应 400，得到 %d %s", rec.Code, rec.Body.String())
	}

	rec = get(h, "/api/policy-audit?source_id="+itoa(id), nil, ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "source_id") {
		t.Fatalf("策略审计应可读: %d %s", rec.Code, rec.Body.String())
	}

	rec = policyReq(h, http.MethodDelete, "/api/policies/"+itoa(id)+"?host=quay.io", "", ck, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除策略应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetRealmPolicy(id, "quay.io"); !errors.Is(err, v2auth.ErrPolicyMissing) {
		t.Fatalf("删除后应查不到策略，得到 %v", err)
	}
	// 运行时必须 fail closed，不能退回不受约束的 Realm。
	if _, err := store.NewStorePolicy(st).Resolve(context.Background(), id, "quay.io"); !errors.Is(err, v2auth.ErrPolicyMissing) {
		t.Fatalf("删除策略后运行时必须 fail closed，得到 %v", err)
	}
}

func TestPolicyAPIDeniesMissingCSRF(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)
	id, err := st.AddSource(store.Source{
		Name: "q", BaseURL: "https://mirror.example", Status: "usable",
		Enabled: true, RepoHost: "quay.io",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"primary_realm":"https://quay.io","extra_realms":[]}`
	rec := policyReq(h, http.MethodPut, "/api/policies/"+itoa(id)+"?host=quay.io", body, ck, false)
	if rec.Code == http.StatusOK {
		t.Fatalf("无 CSRF 的策略修改不应成功: %d %s", rec.Code, rec.Body.String())
	}
}
