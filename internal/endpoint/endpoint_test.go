package endpoint

import (
	"testing"
)

// TestParseOriginMatrix 对应任务书 §4.1 地址模型 RED 矩阵。
func TestParseOriginMatrix(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		scheme Scheme
		host   string
		port   int
		origin string
	}{
		{"域名默认端口", "https://registry.example.com:443/v2", HTTPS, "registry.example.com", 443, "https://registry.example.com:443"},
		{"域名省略默认端口", "https://registry.example.com/v2", HTTPS, "registry.example.com", 443, "https://registry.example.com:443"},
		{"域名非默认端口", "http://registry.example.com:5000", HTTP, "registry.example.com", 5000, "http://registry.example.com:5000"},
		{"大小写与尾点", "HTTPS://REGISTRY.Example.COM.:443", HTTPS, "registry.example.com", 443, "https://registry.example.com:443"},
		{"IPv4", "http://192.0.2.10:5000", HTTP, "192.0.2.10", 5000, "http://192.0.2.10:5000"},
		{"IPv4 默认端口", "https://192.0.2.10", HTTPS, "192.0.2.10", 443, "https://192.0.2.10:443"},
		{"IPv6", "https://[2001:db8::10]:5443", HTTPS, "2001:db8::10", 5443, "https://[2001:db8::10]:5443"},
		{"IPv6 默认端口", "https://[2001:db8::10]", HTTPS, "2001:db8::10", 443, "https://[2001:db8::10]:443"},
		{"IPv4-mapped IPv6", "http://[::ffff:192.0.2.10]:5000", HTTP, "192.0.2.10", 5000, "http://192.0.2.10:5000"},
		{"IPv6 大写压缩", "https://[2001:DB8:0:0:0:0:0:10]:443", HTTPS, "2001:db8::10", 443, "https://[2001:db8::10]:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) 失败: %v", tc.in, err)
			}
			if ep.Scheme() != tc.scheme || ep.Host() != tc.host || ep.Port() != tc.port {
				t.Fatalf("Parse(%q) = (%s,%s,%d), 期望 (%s,%s,%d)",
					tc.in, ep.Scheme(), ep.Host(), ep.Port(), tc.scheme, tc.host, tc.port)
			}
			if got := ep.Origin(); got != tc.origin {
				t.Fatalf("Origin(%q) = %q, 期望 %q", tc.in, got, tc.origin)
			}
			if got := ep.Authority(); got != "" && tc.port != 0 {
				// Authority 必须始终显式带端口，且 IPv6 带方括号。
				want := tc.host + ":" + itoa(tc.port)
				if tc.host == "2001:db8::10" {
					want = "[2001:db8::10]:" + itoa(tc.port)
				}
				if got != want {
					t.Fatalf("Authority(%q) = %q, 期望 %q", tc.in, got, want)
				}
			}
		})
	}
}

// TestParseRejects 覆盖非法 Scheme、非法端口、歧义输入与不安全输入。
func TestParseRejects(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"ftp://registry.example.com",
		"registry.example.com",
		"https://",
		"https://:443",
		"https://host:70000",
		"https://host:0",
		"https://host:-1",
		"https://host:",
		"https://host:abc",
		"https://2001:db8::10",          // 裸 IPv6 未加方括号
		"https://[2001:db8::10",         // 未闭合方括号
		"https://user:pass@host",        // 不允许用户信息
		"https://host?x=1",              // 不允许 query
		"https://host#frag",             // 不允许 fragment
		"https://010.0.0.1",             // 非法 IPv4 形态
		"https://256.0.0.1",             // 非法 IPv4 形态
		"https://-bad.example.com",      // 非法域名标签
		"https://bad-.example.com",      // 非法域名标签
		"https://bad_label.example.com", // 非法字符
		"https://例子.测试",                 // 既有规则不接受非 ASCII
		"https://host..example.com",     // 空标签
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if ep, err := Parse(in); err == nil {
				t.Fatalf("Parse(%q) 应拒绝，却得到 %s", in, ep.Origin())
			}
		})
	}
}

// TestParseOriginStrict 确认严格模式拒绝路径。
func TestParseOriginStrict(t *testing.T) {
	if _, err := ParseOrigin("https://registry.example.com/v2"); err == nil {
		t.Fatal("ParseOrigin 应拒绝带路径的输入")
	}
	if ep, err := ParseOrigin("https://registry.example.com:443"); err != nil || ep.Port() != 443 {
		t.Fatalf("ParseOrigin 纯 Origin 失败: %v", err)
	}
}

// TestCanonicalHostCompat 证明域名规范化与既有 registryhost 规则兼容。
func TestCanonicalHostCompat(t *testing.T) {
	cases := map[string]string{
		"Registry.Example.COM.": "registry.example.com",
		"ghcr.io":               "ghcr.io",
		"gcr.io.":               "gcr.io",
		"a-b.c-d.example":       "a-b.c-d.example",
	}
	for in, want := range cases {
		got, err := CanonicalHost(in)
		if err != nil {
			t.Fatalf("CanonicalHost(%q) 失败: %v", in, err)
		}
		if got != want {
			t.Fatalf("CanonicalHost(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestEqualAndZero 确认值语义。
func TestEqualAndZero(t *testing.T) {
	a, _ := Parse("https://Host.Example.COM:443")
	b, _ := Parse("https://host.example.com")
	if !a.Equal(b) {
		t.Fatal("默认端口与省略端口应相等")
	}
	var zero Endpoint
	if !zero.IsZero() {
		t.Fatal("零值应报告 IsZero")
	}
	if a.IsZero() {
		t.Fatal("非零值不应报告 IsZero")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
