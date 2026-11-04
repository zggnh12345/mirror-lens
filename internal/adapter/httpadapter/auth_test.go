package httpadapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GitHub 的私有内容需要调用方的 Authorization；普通 URL 不能转发凭据，
// 否则换源时会把 token 送给第三方镜像。
func TestPlanForwardsAuthOnlyForGitHub(t *testing.T) {
	a := New(0)

	gh := httptest.NewRequest(http.MethodGet, "http://gw/http/https://raw.githubusercontent.com/acme/private/main/x.json", nil)
	gh.Header.Set("Authorization", "token ghp_example")
	p1, err := a.Plan(gh)
	if err != nil {
		t.Fatal(err)
	}
	if got := p1.Header.Get("Authorization"); got != "token ghp_example" {
		t.Errorf("GitHub 目标未透传 Authorization: %q", got)
	}
	if p1.Cacheable {
		t.Error("带凭据的响应不应落盘")
	}
	if !strings.Contains(p1.CacheKey, "|auth=") {
		t.Errorf("缓存键未隔离: %s", p1.CacheKey)
	}

	plain := httptest.NewRequest(http.MethodGet, "http://gw/http/https://example.com/a.iso", nil)
	plain.Header.Set("Authorization", "token ghp_example")
	p2, err := a.Plan(plain)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Header.Get("Authorization") != "" {
		t.Error("普通域名不应转发凭据")
	}
	if !p2.Cacheable {
		t.Error("普通请求仍应可缓存")
	}

	anon := httptest.NewRequest(http.MethodGet, "http://gw/http/https://raw.githubusercontent.com/acme/private/main/x.json", nil)
	p3, _ := a.Plan(anon)
	if p3.CacheKey == p1.CacheKey {
		t.Error("匿名请求与带凭据请求的缓存键不能相同")
	}
}
