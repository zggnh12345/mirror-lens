package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// assert400CodeField 断言 400 且错误类别与字段名精确匹配。
func assert400CodeField(t *testing.T, rec *httptest.ResponseRecorder, body, code, field string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body=%s 期望 400，得 %d %s", body, rec.Code, recBody(rec))
	}
	var obj map[string]string
	if err := json.Unmarshal(recBody(rec), &obj); err != nil {
		t.Fatalf("body=%s 错误响应非 JSON: %v / %s", body, err, recBody(rec))
	}
	if obj["error"] != code {
		t.Errorf("body=%s error=%q，期望 %q", body, obj["error"], code)
	}
	if field != "" && obj["field"] != field {
		t.Errorf("body=%s field=%q，期望 %q", body, obj["field"], field)
	}
}

func recBody(rec *httptest.ResponseRecorder) []byte {
	if rec.Body == nil {
		return nil
	}
	return rec.Body.Bytes()
}

// §15.2 POST Source：六个旧字段逐个断言 legacy_field_not_supported + field。
func TestG4PostLegacyFieldsEach(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	for _, f := range []string{"url", "host", "scheme", "port", "repo_host", "connect_host"} {
		body := `{"name":"t","endpoint":{"scheme":"https","address":"example.com"},"` + f + `":"x"}`
		assert400CodeField(t, postJSON(h, "/api/sources", body, ck), body, "legacy_field_not_supported", f)
	}
}

// §15.2 POST Source：顶层未知字段 / 嵌套 endpoint 未知字段都返回 unknown_field + 路径。
func TestG4PostUnknownFields(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	b1 := `{"name":"t","endpoint":{"scheme":"https","address":"example.com"},"junk":1}`
	assert400CodeField(t, postJSON(h, "/api/sources", b1, ck), b1, "unknown_field", "junk")
	b2 := `{"name":"t","endpoint":{"scheme":"https","address":"example.com","junk":1}}`
	assert400CodeField(t, postJSON(h, "/api/sources", b2, ck), b2, "unknown_field", "endpoint.junk")
}

// §15.2 POST Source：地址带 http:// 与 https:// 前缀都 400；name 省略 400。
func TestG4PostAddressAndName(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	for _, addr := range []string{"http://mirror.example.org:5000", "https://example.com"} {
		body := `{"name":"t","endpoint":{"scheme":"https","address":"` + addr + `"}}`
		if rec := postJSON(h, "/api/sources", body, ck); rec.Code != http.StatusBadRequest {
			t.Fatalf("带 scheme 前缀应 400，body=%s 得 %d", body, rec.Code)
		}
	}
	body := `{"endpoint":{"scheme":"https","address":"example.com"}}`
	if rec := postJSON(h, "/api/sources", body, ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("name 省略应 400，得 %d", rec.Code)
	}
}

// §15.2 PATCH Source：四个旧地址字段逐个断言；顶层/嵌套 unknown；只 address 拒绝。
func TestG4PatchFieldsEach(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	rec := postJSON(h, "/api/sources", `{"name":"p","endpoint":{"scheme":"https","address":"mirror.example.org"}}`, ck)
	var created struct{ ID int64 }
	_ = json.Unmarshal(recBody(rec), &created)
	p := "/api/sources/" + strconv.FormatInt(created.ID, 10)
	for _, f := range []string{"base_url", "repo_host", "scheme", "port"} {
		body := `{"` + f + `":"x"}`
		assert400CodeField(t, sendJSON(h, http.MethodPatch, p, body, ck), body, "legacy_field_not_supported", f)
	}
	b1 := `{"junk":1}`
	assert400CodeField(t, sendJSON(h, http.MethodPatch, p, b1, ck), b1, "unknown_field", "junk")
	b2 := `{"endpoint":{"scheme":"https","address":"x","junk":1}}`
	assert400CodeField(t, sendJSON(h, http.MethodPatch, p, b2, ck), b2, "unknown_field", "endpoint.junk")
	b3 := `{"endpoint":{"address":"only.example.com"}}`
	if rec := sendJSON(h, http.MethodPatch, p, b3, ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("只给 address 应 400，得 %d", rec.Code)
	}
}

// §15.2 Host POST：extra_realms 矩阵、空 host、空 primary_realm、unknown_field。
func TestG4HostMatrix(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	rec := postJSON(h, "/api/sources", `{"name":"h","endpoint":{"scheme":"https","address":"quay.io"}}`, ck)
	var created struct{ ID int64 }
	_ = json.Unmarshal(recBody(rec), &created)
	p := "/api/sources/" + strconv.FormatInt(created.ID, 10) + "/hosts"

	// 合法：省略、0 个、1 个、3 个
	for _, body := range []string{
		`{"host":"quay.io","primary_realm":"https://quay.io"}`,
		`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":[]}`,
		`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":["https://a.example.com"]}`,
		`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":["https://a.example.com","https://b.example.com","https://c.example.com"]}`,
	} {
		if rec := postJSON(h, p, body, ck); rec.Code != http.StatusOK {
			t.Fatalf("Hosts 合法输入失败 body=%s 得 %d %s", body, rec.Code, recBody(rec))
		}
	}
	// 非法：对象、非法元素、空 host、空 primary_realm、unknown
	for _, tc := range []struct{ body, code, field string }{
		{`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":{}}`, "extra_realms 必须是数组", "extra_realms"},
		{`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":[1]}`, "extra_realms 元素必须是字符串", "extra_realms"},
		{`{"host":"","primary_realm":"https://quay.io"}`, "缺少 host", "host"},
		{`{"host":"quay.io","primary_realm":""}`, "primary_realm is required", "primary_realm"},
		{`{"host":"quay.io","primary_realm":"https://quay.io","junk":1}`, "unknown_field", "junk"},
	} {
		assert400CodeField(t, postJSON(h, p, tc.body, ck), tc.body, tc.code, tc.field)
	}
}

// §15.2 UI：HTTP/HTTPS 下拉框保留、IPv6 示例存在。
func TestG4UISchemeDropdownKept(t *testing.T) {
	b, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, id := range []string{"addScheme", "editScheme"} {
		if !strings.Contains(s, id) {
			t.Fatalf("UI 缺少 HTTP/HTTPS 下拉框 %s", id)
		}
	}
	if !strings.Contains(s, "[fd00::1]:5000") {
		t.Fatal("UI 缺少 IPv6 示例")
	}
}
