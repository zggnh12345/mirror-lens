package store

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/endpoint"
)

func mustEP(t *testing.T, raw string) endpoint.Endpoint {
	t.Helper()
	ep, err := endpoint.Parse(raw)
	if err != nil {
		t.Fatalf("endpoint.Parse(%q): %v", raw, err)
	}
	return ep
}

// TestNormalizeRealmOriginException 锁定 HTTP Realm 窄例外：
// 只允许与对应 SourceEndpoint 完整 Origin 全等的 HTTP Realm；
// HTTPS Realm 身份按 Host 去端口，IP Realm 只允许精确匹配 SourceEndpoint Host。
func TestNormalizeRealmOriginException(t *testing.T) {
	httpOrigin := mustEP(t, "http://h:5000")
	httpsIP := mustEP(t, "https://192.0.2.10:5443")

	cases := []struct {
		name       string
		raw        string
		allowPlain bool
		origin     endpoint.Endpoint
		want       string
		wantErr    bool
	}{
		{"HTTP 精确 Origin 放行", "http://h:5000", true, httpOrigin, "http://h:5000", false},
		{"HTTP 不同端口拒绝", "http://h:8080", true, httpOrigin, "", true},
		{"HTTPS Source 拒绝 HTTP Realm", "http://h:5000", false, mustEP(t, "https://h"), "", true},
		{"HTTPS 非默认端口去端口", "https://h:8443", true, httpOrigin, "https://h", false},
		{"HTTPS 默认端口去端口", "https://h", true, httpOrigin, "https://h", false},
		{"HTTPS 白名单跨 Host 保留", "https://ghcr.io", true, httpOrigin, "https://ghcr.io", false},
		{"HTTPS IP 精确 Origin 放行", "https://192.0.2.10:5443", true, httpsIP, "https://192.0.2.10", false},
		{"HTTPS IP 未匹配 Origin 拒绝", "https://192.0.2.11:5443", true, httpsIP, "", true},
		{"读模式允许已存 HTTP Realm", "http://h:5000", true, endpoint.Endpoint{}, "http://h:5000", false},
		{"读模式允许已存 HTTPS IP Realm", "https://192.0.2.10:5443", true, endpoint.Endpoint{}, "https://192.0.2.10", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeRealm(tc.raw, tc.allowPlain, tc.origin)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应拒绝，得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("应允许，得到 %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q，期望 %q", got, tc.want)
			}
		})
	}
}
