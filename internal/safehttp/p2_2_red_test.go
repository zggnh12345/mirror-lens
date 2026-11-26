package safehttp

import (
	"errors"
	"net/netip"
	"net/url"
	"testing"
)

// AP-01..AP-18：AllowPrivate 只负责 RFC1918 + ULA。
// 当前实现会因 safehttp.go:112-114 的提前 return 全部放行，这些用例应先 RED。
func TestAllowPrivateDoesNotAllowSpecialPurpose(t *testing.T) {
	g := New(Options{AllowPrivate: true})

	cases := []string{
		"198.18.0.4", "198.19.255.255", // FakeIP / benchmarking
		"192.0.2.10",           // TEST-NET-1
		"2001:2::10",           // benchmarking
		"100::10",              // discard-only
		"169.254.169.254",      // link-local / metadata
		"100.64.0.1",           // CGNAT
		"fe80::1",              // IPv6 link-local
		"224.0.0.1", "ff02::1", // multicast
		"0.0.0.0", "::", // unspecified
		"255.255.255.255",   // broadcast
		"2001:db8::1",       // documentation
		"2001::1",           // 2001::/23 (Teredo)
		"2002::1",           // 6to4
		"3fff::1",           // documentation (RFC 9637)
		"5f00::1",           // SRv6 SID
		"64:ff9b:1::1",      // local NAT64
		"::ffff:198.18.0.4", // v4-mapped FakeIP
	}
	for _, s := range cases {
		if err := g.CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s 不应被 AllowPrivate 放行，实际 %v", s, err)
		}
	}
}

// AP-19..AP-24：真正的兼容性保持项。
func TestAllowPrivateStillAllowsRFC1918AndULA(t *testing.T) {
	g := New(Options{AllowPrivate: true})
	for _, s := range []string{
		"10.0.0.1", "172.16.5.5", "192.168.1.5",
		"fc00::1", "fd12:3456::1",
		"::ffff:10.0.0.1",
	} {
		if err := g.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s 应被 AllowPrivate 放行，实际 %v", s, err)
		}
	}
}

// LP-01..LP-03：AllowPrivate 不得再代为放行回环。
func TestLoopbackNeedsItsOwnSwitch(t *testing.T) {
	g := New(Options{AllowPrivate: true})
	for _, s := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1"} {
		if err := g.CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s 应只由 AllowLoopback 放行，实际 %v", s, err)
		}
	}
	// 独立开关仍然有效。
	ok := New(Options{AllowLoopback: true})
	for _, s := range []string{"127.0.0.1", "::1"} {
		if err := ok.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("AllowLoopback=true 应放行 %s，实际 %v", s, err)
		}
	}
}

// FP-01：URL 直接写 198.18 字面量必须拒绝，默认 Guard 也不例外。
func TestCheckURLRejectsFakeIPLiteral(t *testing.T) {
	g := New(Options{})
	for _, raw := range []string{
		"http://198.18.0.4/",
		"https://198.19.1.1/x",
		"http://[::ffff:198.18.0.4]/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("解析 %s: %v", raw, err)
		}
		if err := g.Check(u); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s 应被拒，实际 %v", raw, err)
		}
	}
}

// FP-02：198.18.0.0/15 的四个边界点，只有 /15 内拒绝。
func TestFakeIPRangeBoundaries(t *testing.T) {
	g := New(Options{})
	cases := []struct {
		ip   string
		want bool // true = blocked
	}{
		{"198.17.255.255", false},
		{"198.18.0.0", true},
		{"198.19.255.255", true},
		{"198.20.0.0", false},
	}
	for _, c := range cases {
		err := g.CheckIP(netip.MustParseAddr(c.ip))
		if got := errors.Is(err, ErrBlocked); got != c.want {
			t.Errorf("%s blocked=%v，期望 %v（err=%v）", c.ip, got, c.want, err)
		}
	}
}

// AP-25..AP-27：AllowPrivate=false 的回归护栏。
func TestDefaultStillBlocksPrivateAndFakeIP(t *testing.T) {
	g := New(Options{})
	for _, s := range []string{"10.0.0.1", "fc00::1", "198.18.0.4"} {
		if err := g.CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s 应被默认 Guard 拒绝，实际 %v", s, err)
		}
	}
}
