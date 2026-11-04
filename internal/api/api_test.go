package api

import (
	"net/http"
	"net/url"
	"testing"
)


// 统一写法：完整地址直接拼在域名后面，不必记 /raw/、/gh/ 这些前缀。
func TestUniformTarget(t *testing.T) {
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"/https://raw.githubusercontent.com/o/r/main/f.srs", "https://raw.githubusercontent.com/o/r/main/f.srs", true},
		{"/http://example.com/a.bin", "http://example.com/a.bin", true},
		// 反代把 // 折叠成 / 之后仍要认
		{"/https:/raw.githubusercontent.com/o/r/main/f.srs", "https:/raw.githubusercontent.com/o/r/main/f.srs", true},
		{"/https/raw.githubusercontent.com/o/r/main/f.srs", "https/raw.githubusercontent.com/o/r/main/f.srs", true},
		// 老写法交给 wantsHTTPProxy，别在这里抢
		{"/http/https://example.com/a", "", false},
		{"/raw/o/r/main/f", "", false},
		{"/v2/library/nginx/manifests/latest", "", false},
		{"/", "", false},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.path)
		got, ok := uniformTarget(&http.Request{URL: u})
		if ok != c.ok || got != c.want {
			t.Fatalf("%s → (%q,%v)，期望 (%q,%v)", c.path, got, ok, c.want, c.ok)
		}
	}
}
