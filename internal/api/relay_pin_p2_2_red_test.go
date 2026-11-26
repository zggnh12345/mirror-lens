package api

import (
	"net/netip"
	"testing"
)

// PIN-02..PIN-08：Pin 创建阶段的保留段判定。
// 当前 bogusPinAddr 只覆盖一部分网段，下列地址会返回 ""（视为可用），应先 RED。
func TestBogusPinAddrCoversSpecialPurposeRanges(t *testing.T) {
	blocked := []string{
		"198.18.0.4",
		"192.0.2.10",
		"2001:2::10",
		"100::10",
		"2002::1",
		"3fff::1",
		"5f00::1",
		"64:ff9b:1::1",
		"169.254.169.254",
		"100.64.0.1",
		"fc00::1",
	}
	for _, s := range blocked {
		if got := bogusPinAddr(netip.MustParseAddr(s)); got == "" {
			t.Errorf("%s 应被判为不可用，实际放行", s)
		}
	}
	if got := bogusPinAddr(netip.MustParseAddr("1.1.1.1")); got != "" {
		t.Errorf("1.1.1.1 应可用，实际 %q", got)
	}
}
