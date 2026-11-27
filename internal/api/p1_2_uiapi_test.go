package api

import (
	"net/http"
	"strings"
	"testing"
)

// 3.5.25 F1/F2：不再有"自动归属"，旧契约字段一律拒绝。
func TestAddSourceRejectsLegacyFields(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	for _, body := range []string{
		`{"url":"https://mirror.example.net","name":"t"}`,
		`{"connect_host":"mirror.example.net","scheme":"https","name":"t"}`,
		`{"name":"t","endpoint":{"scheme":"https","address":"mirror.example.net","host":"ghcr.io"}}`,
	} {
		rec := postJSON(h, "/api/sources", body, ck)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("旧字段应 400，body=%s 得 %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestAddSourceExplicitHTTPAndPort(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)
	body := `{"name":"plain","endpoint":{"scheme":"http","address":"mirror.example.org:5000"}}`
	rec := postJSON(h, "/api/sources", body, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("显式 HTTP+端口新增失败 %d %s", rec.Code, rec.Body.String())
	}
	list, _ := st.ListSources()
	found := false
	for _, x := range list {
		if strings.HasPrefix(x.BaseURL, "http://mirror.example.org:5000") {
			found = true
			if x.RepoHost != "" {
				t.Fatalf("新源不应强制归属，得到 %q", x.RepoHost)
			}
		}
	}
	if !found {
		t.Fatalf("显式 HTTP+端口未落库，列表=%v", list)
	}
}

func TestPatchSourceEndpoint(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)
	rec := postJSON(h, "/api/sources", `{"name":"t","endpoint":{"scheme":"https","address":"mirror.example.org"}}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("新增失败 %d %s", rec.Code, rec.Body.String())
	}
	list, _ := st.ListSources()
	var id int64
	for _, x := range list {
		if strings.HasPrefix(x.BaseURL, "https://mirror.example.org") {
			id = x.ID
		}
	}
	if id == 0 {
		t.Fatal("新增源未落库")
	}
	rec = patchJSON(h, "/api/sources/"+itoa(id), `{"endpoint":{"scheme":"http","address":"mirror.example.org:5000"}}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH endpoint 失败 %d %s", rec.Code, rec.Body.String())
	}
	list, _ = st.ListSources()
	for _, x := range list {
		if x.ID == id {
			if !strings.HasPrefix(x.BaseURL, "http://mirror.example.org:5000") {
				t.Fatalf("base_url=%q 期望 http://mirror.example.org:5000", x.BaseURL)
			}
			return
		}
	}
	t.Fatal("编辑后源丢失")
}
