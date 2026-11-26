package safehttp

import (
	"errors"
	"net/netip"
	"testing"
)

// COMPAT-01..04：AllowPrivate 收窄之前必须先锁定的既有行为。
// 这些用例在收窄前后都必须 PASS，否则说明“兼容性保持”只是口头声明。
func TestCompatAllowPrivateKeepsRFC1918AndULA(t *testing.T) {
	g := New(Options{AllowPrivate: true})

	for _, s := range []string{"10.0.0.1", "10.255.255.254", "172.16.0.1", "172.31.255.255", "192.168.0.1", "192.168.255.254", "fc00::1", "fd12:3456::1"} {
		if err := g.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("AllowPrivate=true 应放行 %s，实际 %v", s, err)
		}
	}

	for _, s := range []string{"10.0.0.1", "172.16.0.1", "192.168.0.1", "fc00::1"} {
		if err := New(Options{}).CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrBlocked) {
			t.Errorf("默认 Guard 应拒绝 %s，实际 %v", s, err)
		}
	}
}

// COMPAT-04：回环由独立开关控制；AllowLoopback 不得顺带放行私网。
func TestCompatAllowLoopbackIsIndependent(t *testing.T) {
	g := New(Options{AllowLoopback: true})
	for _, s := range []string{"127.0.0.1", "127.255.255.254", "::1", "::ffff:127.0.0.1"} {
		if err := g.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("AllowLoopback=true 应放行 %s，实际 %v", s, err)
		}
	}
	for _, s := range []string{"10.0.0.1", "192.168.1.1", "fc00::1"} {
		if err := g.CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrBlocked) {
			t.Errorf("AllowLoopback 不应放行 %s，实际 %v", s, err)
		}
	}
	if err := New(Options{}).CheckIP(netip.MustParseAddr("127.0.0.1")); !errors.Is(err, ErrBlocked) {
		t.Errorf("默认 Guard 应拒绝回环，实际 %v", err)
	}
}
