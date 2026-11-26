package v2auth

import (
	"net/url"
	"testing"
)

func redirectTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestRedirectPolicyOriginMatrix 对应任务书 §4.5 普通重定向表（冻结）。
func TestRedirectPolicyOriginMatrix(t *testing.T) {
	cases := []struct {
		name        string
		kind        RedirectKind
		method      string
		from, to    string
		wantStrip   bool
		wantRecheck bool
		wantErr     bool
	}{
		{
			name: "1 HTTP 同 Origin 保留凭据", kind: RedirectRegistry, method: "GET",
			from: "http://h:5000/v2/a/manifests/1", to: "http://h:5000/v2/a/manifests/2",
			wantStrip: false, wantRecheck: true,
		},
		{
			name: "2 HTTP 同 Host 不同端口清除凭据", kind: RedirectRegistry, method: "GET",
			from: "http://h:5000/v2/a", to: "http://h:8080/v2/a",
			wantStrip: true, wantRecheck: true,
		},
		{
			name: "3 HTTP 升级 HTTPS 清除凭据", kind: RedirectRegistry, method: "GET",
			from: "http://h/v2/a", to: "https://h/v2/a",
			wantStrip: true, wantRecheck: true,
		},
		{
			name: "4 HTTPS 降级 HTTP 拒绝", kind: RedirectRegistry, method: "GET",
			from: "https://h/v2/a", to: "http://h/v2/a",
			wantErr: true,
		},
		{
			name: "5 HTTP 跨 Host 清除凭据后交给目标策略", kind: RedirectRegistry, method: "GET",
			from: "http://a/v2/a", to: "http://b/v2/a",
			wantStrip: true, wantRecheck: true,
		},
		{
			name: "HTTPS 默认端口等价视为同 Origin", kind: RedirectRegistry, method: "GET",
			from: "https://h/v2/a", to: "https://h:443/v2/a",
			wantStrip: false, wantRecheck: true,
		},
		{
			name: "HTTPS 非默认端口清除凭据", kind: RedirectRegistry, method: "GET",
			from: "https://h/v2/a", to: "https://h:8443/v2/a",
			wantStrip: true, wantRecheck: true,
		},
		{
			name: "Token 默认端口等价同源", kind: RedirectToken, method: "GET",
			from: "https://h/token", to: "https://h:443/token2",
			wantStrip: false, wantRecheck: false,
		},
		{
			name: "Token 不同端口拒绝", kind: RedirectToken, method: "GET",
			from: "https://h/token", to: "https://h:8443/token",
			wantErr: true,
		},
		{
			// 冻结 §4.5 HTTP→HTTP 同 Host：来源已是已授权 HTTP Realm（过 Allows
			// 完整 Origin 门禁），Token 3xx 同源跳转不得再被 HTTPS-only 拒绝。
			name: "Token HTTP→HTTP 同源允许", kind: RedirectToken, method: "GET",
			from: "http://h:5000/token", to: "http://h:5000/token2",
			wantStrip: false, wantRecheck: false,
		},
		{
			name: "Token HTTP→HTTP 同 Host 不同端口拒绝", kind: RedirectToken, method: "GET",
			from: "http://h:5000/token", to: "http://h:8080/token",
			wantErr: true,
		},
		{
			name: "Token HTTP→HTTP 跨 Host 拒绝", kind: RedirectToken, method: "GET",
			from: "http://h:5000/token", to: "http://other:5000/token",
			wantErr: true,
		},
		{
			name: "Token HTTPS→HTTP 降级拒绝", kind: RedirectToken, method: "GET",
			from: "https://h:5000/token", to: "http://h:5000/token",
			wantErr: true,
		},
		{
			name: "CDN HTTP 拒绝", kind: RedirectCDN, method: "GET",
			from: "https://h/blob", to: "http://cdn.example/blob",
			wantErr: true,
		},
		{
			name: "非法 Scheme 拒绝", kind: RedirectRegistry, method: "GET",
			from: "ftp://h/a", to: "https://h/a",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := RedirectPolicy(tc.kind, tc.method,
				redirectTestURL(t, tc.from), redirectTestURL(t, tc.to), 1)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应拒绝，得到 %+v", d)
				}
				return
			}
			if err != nil {
				t.Fatalf("应允许，得到 %v", err)
			}
			if d.StripAuth != tc.wantStrip || d.RecheckPolicy != tc.wantRecheck {
				t.Fatalf("decision=%+v，期望 StripAuth=%v RecheckPolicy=%v",
					d, tc.wantStrip, tc.wantRecheck)
			}
		})
	}
}
