package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

// 3.5.25 F1/F2：归属不再必选、不再推断；旧字段一律 400；endpoint.scheme+address 生效。
func TestAddSourceEndpointContract(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	for _, body := range []string{
		`{"url":"https://example.com","name":"t"}`,
		`{"name":"t","endpoint":{"scheme":"https","address":"example.com","host":"x"}}`,
	} {
		if rec := postJSON(h, "/api/sources", body, ck); rec.Code != http.StatusBadRequest {
			t.Fatalf("旧/未知字段应 400，body=%s 得 %d %s", body, rec.Code, rec.Body.String())
		}
	}

	if rec := postJSON(h, "/api/sources", `{"name":"hub","endpoint":{"scheme":"https","address":"registry-1.docker.io"}}`, ck); rec.Code != http.StatusOK {
		t.Fatalf("新契约新增失败 %d %s", rec.Code, rec.Body.String())
	}
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	for _, x := range list {
		if strings.HasPrefix(x.BaseURL, "https://registry-1.docker.io") {
			got = true
			if x.RepoHost != "" {
				t.Fatalf("新源不应强制归属，得到 (%q,%q)", x.RepoHost, x.RegistryIdentity)
			}
		}
	}
	if !got {
		t.Fatal("新增源没有落库")
	}

	if rec := postJSON(h, "/api/sources", `{"name":"x","endpoint":{"scheme":"https","address":"https://example.com"}}`, ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("地址带 scheme 应 400，得 %d", rec.Code)
	}
	if rec := postJSON(h, "/api/sources", `{"name":"x","endpoint":{"scheme":"http","address":"example.com:5000"}}`, ck); rec.Code != http.StatusOK {
		t.Fatalf("显式端口新增失败 %d %s", rec.Code, rec.Body.String())
	}
}

func TestPatchSourceEndpointContract(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	rec := postJSON(h, "/api/sources", `{"name":"p","endpoint":{"scheme":"https","address":"mirror.example.org"}}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("新增失败 %d %s", rec.Code, rec.Body.String())
	}
	var created struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	p := "/api/sources/" + strconv.FormatInt(created.ID, 10)

	for _, body := range []string{
		`{"base_url":"https://x"}`,
		`{"endpoint":{"scheme":"https"}}`,
		`{"endpoint":{"scheme":"https","address":"x","junk":1}}`,
	} {
		if rec := sendJSON(h, http.MethodPatch, p, body, ck); rec.Code != http.StatusBadRequest {
			t.Fatalf("PATCH 非法输入应 400，body=%s 得 %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := sendJSON(h, http.MethodPatch, p, `{"name":"renamed"}`, ck); rec.Code != http.StatusOK {
		t.Fatalf("PATCH 元数据失败 %d %s", rec.Code, rec.Body.String())
	}
	if rec := sendJSON(h, http.MethodPatch, p, `{"endpoint":{"scheme":"http","address":"new.example.org:5000"}}`, ck); rec.Code != http.StatusOK {
		t.Fatalf("PATCH 完整 endpoint 失败 %d %s", rec.Code, rec.Body.String())
	}
}

func TestSourceHostsStrictFields(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	rec := postJSON(h, "/api/sources", `{"name":"h","endpoint":{"scheme":"https","address":"quay.io"}}`, ck)
	var created struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	p := "/api/sources/" + strconv.FormatInt(created.ID, 10) + "/hosts"

	for _, body := range []string{
		`{"host":"quay.io"}`,
		`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":null}`,
		`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":"x"}`,
		`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":["https://a.example.com","https://b.example.com","https://c.example.com","https://d.example.com"]}`,
		`{"host":"quay.io","primary_realm":"https://quay.io","junk":1}`,
	} {
		if rec := postJSON(h, p, body, ck); rec.Code != http.StatusBadRequest {
			t.Fatalf("Hosts 非法输入应 400，body=%s 得 %d %s", body, rec.Code, rec.Body.String())
		}
	}
	for _, body := range []string{
		`{"host":"quay.io","primary_realm":"https://quay.io"}`,
		`{"host":"quay.io","primary_realm":"https://quay.io","extra_realms":["https://a.example.com","https://b.example.com","https://c.example.com"]}`,
	} {
		if rec := postJSON(h, p, body, ck); rec.Code != http.StatusOK {
			t.Fatalf("Hosts 合法输入失败 body=%s 得 %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestUIFormNoLegacyControls(t *testing.T) {
	b, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "addPort") || strings.Contains(string(b), "addRepoHost") {
		t.Fatal("新增表单仍含旧端口/归属控件")
	}
}

// F1 RED：IPv6 字面量带/不带端口都必须正确解析；name 必填。
func TestParseEndpointAddressIPv6(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"[fd00::1]", "https://[fd00::1]:443"},
		{"[fd00::1]:5000", "https://[fd00::1]:5000"},
		{"mirror.example.org", "https://mirror.example.org:443"},
		{"mirror.example.org:5000", "https://mirror.example.org:5000"},
	} {
		ep, err := parseEndpointAddress("https", tc.in)
		if err != nil {
			t.Fatalf("parseEndpointAddress(%q) 失败: %v", tc.in, err)
		}
		if ep.Origin() != tc.want {
			t.Fatalf("parseEndpointAddress(%q)=%q，期望 %q", tc.in, ep.Origin(), tc.want)
		}
	}
}

func TestAddSourceNameRequired(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	if rec := postJSON(h, "/api/sources", `{"name":"","endpoint":{"scheme":"https","address":"example.com"}}`, ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 name 应 400，得 %d %s", rec.Code, rec.Body.String())
	}
}
