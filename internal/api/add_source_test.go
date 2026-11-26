package api

import (
	"net/http"
	"testing"
)

// 3.5.24 §2.3 收紧：未知连接地址无 host 必须拒绝；已知 Hub 域名自动归属；
// 显式 host 原样交给 store。原「未知身份静默默认 docker.io」已被安全收紧取代。
func TestAddSourceDefaultsToDockerHubIdentity(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	// 未知域名无 host：拒绝（不再静默默认 docker.io）。
	if rec := postJSON(h, "/api/sources", `{"url":"https://example.com","name":"t"}`, ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知身份无 host 应 400，得到 %d %s", rec.Code, rec.Body.String())
	}
	// 已知 Hub 域名无 host：自动归属 docker.io。
	if rec := postJSON(h, "/api/sources", `{"url":"https://registry-1.docker.io","name":"hub"}`, ck); rec.Code != http.StatusOK {
		t.Fatalf("已知 Hub 域名新增失败 %d %s", rec.Code, rec.Body.String())
	}
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	for _, x := range list {
		if x.BaseURL == "https://registry-1.docker.io" {
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
