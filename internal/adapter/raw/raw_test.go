package raw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 私有仓库：GitHub 对未认证的 raw 请求回 404，必须把 Authorization 带给上游；
// 带凭据的响应也不能和匿名请求共用缓存键或落盘。
func TestPlanForwardsAuthorization(t *testing.T) {
	a := New("https://raw.githubusercontent.com")

	r := httptest.NewRequest(http.MethodGet, "http://gw/raw/acme/private/main/conf/app.json", nil)
	r.Header.Set("Authorization", "token ghp_example")
	p, err := a.Plan(r)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Header.Get("Authorization"); got != "token ghp_example" {
		t.Errorf("Authorization 未透传: %q", got)
	}
	if p.Cacheable {
		t.Error("带凭据的响应不应落盘")
	}
	if !strings.Contains(p.CacheKey, "|auth=") {
		t.Errorf("缓存键未隔离: %s", p.CacheKey)
	}

	anon := httptest.NewRequest(http.MethodGet, "http://gw/raw/acme/private/main/conf/app.json", nil)
	p2, err := a.Plan(anon)
	if err != nil {
		t.Fatal(err)
	}
	if p2.CacheKey == p.CacheKey {
		t.Error("匿名请求与带凭据请求的缓存键不能相同")
	}
	if p2.Header.Get("Authorization") != "" {
		t.Error("匿名请求不应凭空带上 Authorization")
	}
	if !p2.Cacheable {
		t.Error("匿名公开请求仍应可缓存")
	}
}
