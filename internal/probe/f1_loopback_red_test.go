// P2-2 复核修复的 RED 用例：F1 probe 权限隔离 / F2 Pin 回落强制严格 /
// F3 Origin 按请求隔离。这些用例在修复前必须失败（先 RED 再 GREEN）。
package probe

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/safehttp"
)

// F1-RED：AllowPrivate=true 不得隐式开启回环。127.0.0.1 与 ::1 必须仍被拒绝。
func TestConfigureTrueDoesNotImplicitlyAllowLoopbackRED(t *testing.T) {
	defer Configure(false) // 恢复包默认（无放宽）
	Configure(true)
	g := currentGuard()
	for _, s := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1"} {
		ip := netip.MustParseAddr(s).Unmap()
		if err := g.CheckIP(ip); !errors.Is(err, safehttp.ErrBlocked) {
			t.Errorf("Configure(true) 后 %s 应拒绝，实际 err=%v", s, err)
		}
	}
}

// F1-RED（正向）：测试需要回环时，须用显式测试构造放行，而不是让生产 Configure 隐式放开。
func TestLoopbackOnlyViaExplicitTestGuardRED(t *testing.T) {
	g := safehttp.New(safehttp.Options{AllowLoopback: true})
	if err := g.CheckIP(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatalf("显式 AllowLoopback Guard 应放行回环，实际 %v", err)
	}
}
