package config

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// TestRegistryUpstreamStrictHostname 对应任务书 §4.2 registry.upstream 严格纯主机名矩阵。
func TestRegistryUpstreamStrictHostname(t *testing.T) {
	allowed := []struct{ in, want string }{
		{"registry-1.docker.io", "registry-1.docker.io"},
		{"registry.example.com", "registry.example.com"},
		{"Registry.Example.COM", "registry.example.com"},
		{"registry.example.com.", "registry.example.com"},
		{"  registry.example.com  ", "registry.example.com"},
	}
	for _, tc := range allowed {
		c := Default()
		c.Registry.Enabled = true
		c.Registry.Upstream = tc.in
		if err := c.Validate(); err != nil {
			t.Fatalf("registry.upstream=%q 应允许，却失败: %v", tc.in, err)
		}
		if c.Registry.Upstream != tc.want {
			t.Fatalf("registry.upstream=%q 规范化后 = %q，期望 %q", tc.in, c.Registry.Upstream, tc.want)
		}
	}

	rejected := []string{
		"registry.example.com:5000",
		"203.0.113.10",
		"[2001:db8::10]",
		"[2001:db8::10]:5000",
		"https://registry.example.com",
		"ftp://host",
		"registry.example.com/v2",
		"registry.example.com?x=1",
		"user@registry.example.com",
		"",
		"   ",
		".",
	}
	for _, in := range rejected {
		c := Default()
		c.Registry.Enabled = true
		c.Registry.Upstream = in
		if err := c.Validate(); err == nil {
			t.Fatalf("registry.upstream=%q 应拒绝，却通过", in)
		}
	}
}

// TestRegistryUpstreamAliasNotRewritten 证明 registry-1.docker.io 不会被别名回写成 docker.io。
func TestRegistryUpstreamAliasNotRewritten(t *testing.T) {
	c := Default()
	c.Registry.Enabled = true
	c.Registry.Upstream = "registry-1.docker.io"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Registry.Upstream != "registry-1.docker.io" {
		t.Fatalf("实际路由 Host 被改写为 %q", c.Registry.Upstream)
	}
	if got, err := registryhost.Normalize(c.Registry.Upstream); err != nil || got != "docker.io" {
		t.Fatalf("逻辑身份应为 docker.io，得到 %q, %v", got, err)
	}
}
