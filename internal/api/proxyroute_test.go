package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// /http/ 目标地址整体编码，路径含 %2F，必须在 ServeMux 之前命中。
func TestWantsHTTPProxy(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://gw/http/https%3A%2F%2Fexample.com%2Fa.iso", true},
		{"http://gw/http/https://example.com/a.iso", true},
		{"http://gw/raw/o/r/main/a.txt", false},
		{"http://gw/httpx/y", false},
		{"http://gw/", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.url, nil)
		if got := wantsHTTPProxy(r); got != c.want {
			t.Errorf("%s: 期望 %v，得到 %v", c.url, c.want, got)
		}
	}
}
