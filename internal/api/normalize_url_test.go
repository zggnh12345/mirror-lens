package api

import "testing"

// TestNormalizeURLIPv6 对应任务书 §4.1/§4.2 P1-3：
// sources.base_url 必须支持 IPv6 方括号与非默认端口，且不收紧既有能力。
func TestNormalizeURLIPv6(t *testing.T) {
	allowed := []struct{ in, want string }{
		{"https://[2001:db8::10]", "https://[2001:db8::10]"},
		{"https://[2001:db8::10]:5443", "https://[2001:db8::10]:5443"},
		{"https://[2001:db8::10]/", "https://[2001:db8::10]"},
		{"http://[2001:db8::10]:5000/v2", "http://[2001:db8::10]:5000/v2"},
		{"https://[::ffff:192.0.2.10]:5443", "https://[::ffff:192.0.2.10]:5443"},
	}
	for _, tc := range allowed {
		got, err := normalizeURL(tc.in)
		if err != nil {
			t.Fatalf("normalizeURL(%q) 应允许，却失败: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeURL(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	rejected := []string{
		"https://[2001:db8::10",  // 未闭合方括号
		"https://[2001:db8::zz]", // 非法 IPv6
		"https://2001:db8::10",   // 裸 IPv6 歧义
		"https://[2001:db8::10]:70000",
		"https://[2001:db8::10]:abc",
	}
	for _, in := range rejected {
		if _, err := normalizeURL(in); err == nil {
			t.Fatalf("normalizeURL(%q) 应拒绝", in)
		}
	}
}

// TestNormalizeURLKeepsExistingCapabilities 确认域名/IPv4/HTTP/HTTPS/非默认端口不回归。
func TestNormalizeURLKeepsExistingCapabilities(t *testing.T) {
	allowed := []struct{ in, want string }{
		{"https://registry.example.com", "https://registry.example.com"},
		{"http://registry.example.com:5000", "http://registry.example.com:5000"},
		{"registry.example.com", "https://registry.example.com"},
		{"192.0.2.10", "https://192.0.2.10"},
		{"http://192.0.2.10:5000", "http://192.0.2.10:5000"},
		{"https://github.com/zggnh12345/mirror-lens", "https://github.com/zggnh12345/mirror-lens"},
	}
	for _, tc := range allowed {
		got, err := normalizeURL(tc.in)
		if err != nil {
			t.Fatalf("normalizeURL(%q) 应允许，却失败: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeURL(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	rejected := []string{
		"",
		"   ",
		"ftp://registry.example.com",
		"https://host", // 既有产品约束：域名至少包含一个点
		"https://user@registry.example.com",
		"https://registry.example.com?x=1",
		"https://registry.example.com#frag",
		"https://registry.example.com:70000",
		"https://registry.example.com:abc",
	}
	for _, in := range rejected {
		if _, err := normalizeURL(in); err == nil {
			t.Fatalf("normalizeURL(%q) 应拒绝", in)
		}
	}
}
