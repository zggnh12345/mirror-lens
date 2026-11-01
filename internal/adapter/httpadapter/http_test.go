package httpadapter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 反代会折叠 //、也会拒收 %2F，所以几种写法都得解析出同一个上游地址。
func TestPlanAcceptsFoldedAndEncodedForms(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/http/https%3A%2F%2Fexample.com%2Fa.iso", "https://example.com/a.iso"},
		{"/http/https://example.com/a.iso", "https://example.com/a.iso"},
		{"/http/https:/example.com/a.iso", "https://example.com/a.iso"},
		{"/http/https/example.com/a.iso", "https://example.com/a.iso"},
		{"/http/http/example.com/a.iso", "http://example.com/a.iso"},
		{"/http/http%3A%2F%2Fexample.com%2Fb%3Fx%3D1", "http://example.com/b?x=1"},
	}
	a := New(0)
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://gw"+c.path, nil)
		plan, err := a.Plan(r)
		if err != nil {
			t.Errorf("%s: 解析失败 %v", c.path, err)
			continue
		}
		if got := plan.Upstream.String(); got != c.want {
			t.Errorf("%s: 期望 %s，得到 %s", c.path, c.want, got)
		}
	}
}

func TestPlanRejectsJunk(t *testing.T) {
	a := New(0)
	for _, p := range []string{"/http/", "/http/example.com", "/http/ftp/x", "/http/https/"} {
		r := httptest.NewRequest(http.MethodGet, "http://gw"+p, nil)
		if _, err := a.Plan(r); err == nil {
			t.Errorf("%s: 应当报错", p)
		}
	}
}
