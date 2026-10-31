package safehttp

import (
	"net/netip"
	"net/url"
	"testing"
)

func TestCheckIPBlocksInternal(t *testing.T) {
	g := New(Options{})
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "10.0.0.1", "172.16.5.4", "172.31.255.255",
		"192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"::1", "fc00::1", "fd12:3456::1", "fe80::1", "::ffff:127.0.0.1",
		"255.255.255.255",
	}
	for _, s := range blocked {
		ip := netip.MustParseAddr(s)
		if err := g.CheckIP(ip); err == nil {
			t.Errorf("%s 应该被拦下", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "140.82.121.3", "2606:4700:4700::1111", "198.18.0.5"} {
		ip := netip.MustParseAddr(s)
		if err := g.CheckIP(ip); err != nil {
			t.Errorf("%s 应该放行，却报 %v", s, err)
		}
	}
}

func TestLoopbackOnlyWhenAllowed(t *testing.T) {
	g := New(Options{AllowLoopback: true})
	if err := g.CheckIP(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatalf("测试模式应放行回环: %v", err)
	}
	if err := g.CheckIP(netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Fatal("测试模式也不该放行内网")
	}
}

func TestCheckURL(t *testing.T) {
	g := New(Options{})
	bad := []string{
		"ftp://example.com/x",
		"file:///etc/passwd",
		"http://user:pass@example.com/x",
		"http://127.0.0.1/x",
		"http://[::1]/x",
		"http://169.254.169.254/latest/meta-data/",
	}
	for _, s := range bad {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("解析失败 %s: %v", s, err)
		}
		if err := g.Check(u); err == nil {
			t.Errorf("%s 应该被拒", s)
		}
	}
	ok, _ := url.Parse("https://raw.githubusercontent.com/o/r/main/a.txt")
	if err := g.Check(ok); err != nil {
		t.Errorf("公网地址被误拒: %v", err)
	}
}
