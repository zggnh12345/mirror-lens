package safehttp

import (
	"net/netip"
	"testing"
)

func TestOrderIPs(t *testing.T) {
	v4 := netip.MustParseAddr("1.1.1.1")
	v4b := netip.MustParseAddr("8.8.8.8")
	v6 := netip.MustParseAddr("2606:4700::1111")
	in := []netip.Addr{v6, v4, v4b}

	cases := []struct {
		mode string
		want []netip.Addr
	}{
		{"auto", []netip.Addr{v4, v4b, v6}}, // v4 优先，v6 兜底
		{"", []netip.Addr{v4, v4b, v6}},
		{"v4", []netip.Addr{v4, v4b}},
		{"v6", []netip.Addr{v6}},
	}
	for _, c := range cases {
		g := New(Options{IPFamily: c.mode})
		got := g.orderIPs(in)
		if len(got) != len(c.want) {
			t.Fatalf("mode=%q got %v want %v", c.mode, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("mode=%q got %v want %v", c.mode, got, c.want)
			}
		}
	}
	// 输入不能被就地打乱：调用方可能复用切片。
	if in[0] != v6 {
		t.Fatalf("orderIPs 改了入参: %v", in)
	}
	// 只有 v6 时 auto 也要能拨出去（v4 为空不算错）。
	if got := New(Options{IPFamily: "auto"}).orderIPs([]netip.Addr{v6}); len(got) != 1 || got[0] != v6 {
		t.Fatalf("auto 纯 v6: %v", got)
	}
	// 只要 v4 但域名只有 AAAA：返回空，由调用方报错，而不是偷偷用 v6。
	if got := New(Options{IPFamily: "v4"}).orderIPs([]netip.Addr{v6}); len(got) != 0 {
		t.Fatalf("v4 模式不该返回 v6: %v", got)
	}
}
