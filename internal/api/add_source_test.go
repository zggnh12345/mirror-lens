package api

import (
	"net/http"
	"testing"
)

// 手工新增入口必须把 Host 原样交给 store；不传时由 store 统一落 Hub 身份。
func TestAddSourceDefaultsToDockerHubIdentity(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	if rec := postJSON(h, "/api/sources", `{"url":"https://example.com","name":"t"}`, ck); rec.Code != http.StatusOK {
		t.Fatalf("新增失败 %d %s", rec.Code, rec.Body.String())
	}
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	for _, x := range list {
		if x.BaseURL == "https://example.com" {
			got = true
			if x.RepoHost != "docker.io" || x.RegistryIdentity != "identified" {
				t.Fatalf("默认身份=(%q,%q)，期望 (docker.io,identified)", x.RepoHost, x.RegistryIdentity)
			}
		}
	}
	if !got {
		t.Fatal("新增源没有落库")
	}

	if rec := postJSON(h, "/api/sources", `{"url":"https://example.org","name":"g","host":"ghcr.io"}`, ck); rec.Code != http.StatusOK {
		t.Fatalf("显式 Host 新增失败 %d %s", rec.Code, rec.Body.String())
	}
	list, _ = st.ListSources()
	for _, x := range list {
		if x.BaseURL == "https://example.org" {
			if x.RepoHost != "ghcr.io" || x.RegistryIdentity != "identified" {
				t.Fatalf("显式身份=(%q,%q)，期望 (ghcr.io,identified)", x.RepoHost, x.RegistryIdentity)
			}
		}
	}
}
