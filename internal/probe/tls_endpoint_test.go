package probe

import "testing"

// TestHostOfStripsPortAndBrackets 锁定三值契约：
// DNS/ServerName/verifyChain 只能用裸 Host，不能带端口或方括号。
func TestHostOfStripsPortAndBrackets(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://registry.example.com/v2/", "registry.example.com"},
		{"https://registry.example.com:5443/v2/", "registry.example.com"},
		{"http://192.0.2.10:5000", "192.0.2.10"},
		{"https://[2001:db8::10]:5443/v2/", "2001:db8::10"},
		{"https://[2001:db8::10]", "2001:db8::10"},
		{"registry.example.com", "registry.example.com"},
	}
	for _, tc := range cases {
		if got := hostOf(tc.in); got != tc.want {
			t.Fatalf("hostOf(%q)=%q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestTLSEndpointUsesEffectivePort 锁定拨号端口来自 SourceEndpoint，而不是固定 443。
func TestTLSEndpointUsesEffectivePort(t *testing.T) {
	cases := []struct {
		in     string
		scheme string
		host   string
		port   int
	}{
		{"https://h.example/v2/", "https", "h.example", 443},
		{"https://h.example:5443/v2/", "https", "h.example", 5443},
		{"http://h.example:5000", "http", "h.example", 5000},
		{"https://[2001:db8::10]:5443", "https", "2001:db8::10", 5443},
		{"https://[2001:db8::10]", "https", "2001:db8::10", 443},
	}
	for _, tc := range cases {
		ep, err := tlsEndpoint(tc.in)
		if err != nil {
			t.Fatalf("tlsEndpoint(%q): %v", tc.in, err)
		}
		if string(ep.Scheme()) != tc.scheme || ep.Host() != tc.host || ep.Port() != tc.port {
			t.Fatalf("tlsEndpoint(%q)=%s/%s/%d，期望 %s/%s/%d",
				tc.in, ep.Scheme(), ep.Host(), ep.Port(), tc.scheme, tc.host, tc.port)
		}
	}
}
