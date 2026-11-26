package api

import (
	"net/http"
	"testing"
)

// TestAddSourceRejectsUnknownIdentityWithoutHost 锁定 3.5.24 §2.3：
// 未知域名/IP 连接地址不得静默默认到 docker.io；未显式选择归属 Registry
// （host/repo_host 字段）时必须 400 拒绝。修复前 addSource 默认 docker.io → RED。
func TestAddSourceRejectsUnknownIdentityWithoutHost(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	// 未知域名，无归属：必须拒绝。
	rec := postJSON(h, "/api/sources", `{"url":"https://mirror.example.net","name":"t"}`, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知身份无归属应 400，得到 %d %s", rec.Code, rec.Body.String())
	}
	// IP 连接地址，无归属：必须拒绝。
	rec = postJSON(h, "/api/sources", `{"url":"http://192.0.2.10:5000","name":"ip"}`, ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("IP 地址无归属应 400，得到 %d %s", rec.Code, rec.Body.String())
	}
	// 显式归属：允许。
	rec = postJSON(h, "/api/sources", `{"url":"https://mirror.example.net","name":"t","host":"ghcr.io"}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("显式归属应成功，得到 %d %s", rec.Code, rec.Body.String())
	}
	list, _ := st.ListSources()
	found := false
	for _, x := range list {
		if x.BaseURL == "https://mirror.example.net" {
			found = true
			if x.RepoHost != "ghcr.io" {
				t.Fatalf("RepoHost=%q，期望 ghcr.io", x.RepoHost)
			}
		}
	}
	if !found {
		t.Fatal("显式归属新增源未落库")
	}
}

// TestAddSourceExplicitHTTPAndPort 锁定 §2.3：协议/连接 Host/端口三字段
// 明确构造 SourceEndpoint，HTTP 为显式选择，归属走 repo_host。
func TestAddSourceExplicitHTTPAndPort(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	body := `{"connect_host":"mirror.example.org","scheme":"http","port":"5000","name":"plain","repo_host":"docker.io"}`
	rec := postJSON(h, "/api/sources", body, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("显式 HTTP+端口新增失败 %d %s", rec.Code, rec.Body.String())
	}
	list, _ := st.ListSources()
	found := false
	for _, x := range list {
		if x.BaseURL == "http://mirror.example.org:5000" {
			found = true
		}
	}
	if !found {
		t.Fatalf("显式 HTTP+端口未落成 http://mirror.example.org:5000，列表=%v", list)
	}
}

// TestPatchSourceEndpoint 锁定 §2.3 编辑链路：PATCH 可改 base_url 与归属，
// 且改协议/端口后 SetBaseURL 原子清旧 Realm 授权（独立测试覆盖 Realm 清理）。
func TestPatchSourceEndpoint(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)
	rec := postJSON(h, "/api/sources", `{"url":"https://mirror.example.org","name":"t","host":"docker.io"}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("新增失败 %d %s", rec.Code, rec.Body.String())
	}
	list, _ := st.ListSources()
	var id int64
	for _, x := range list {
		if x.BaseURL == "https://mirror.example.org" {
			id = x.ID
		}
	}
	if id == 0 {
		t.Fatal("新增源未落库")
	}

	rec = patchJSON(h, "/api/sources/"+itoa(id), `{"base_url":"http://mirror.example.org:5000","repo_host":"ghcr.io"}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH 编辑 Endpoint 失败 %d %s", rec.Code, rec.Body.String())
	}
	list, _ = st.ListSources()
	for _, x := range list {
		if x.ID == id {
			if x.BaseURL != "http://mirror.example.org:5000" {
				t.Fatalf("base_url=%q，期望 http://mirror.example.org:5000", x.BaseURL)
			}
			return
		}
	}
	t.Fatal("编辑后源丢失")
}
