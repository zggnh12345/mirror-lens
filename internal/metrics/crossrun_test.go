package metrics

import (
	"strings"
	"testing"
)

// 18f：cross_run 指标标签为封闭集合，未知值折叠；不含源 ID/URL/凭据（低基数）。
func TestCrossRunClosedLabels(t *testing.T) {
	r := setup(t)
	CrossRun("race", "ok", "confirmed")
	CrossRun("sequential", "fallback", "authority_compare")
	// 未知值应折叠到 other/none，不新开高基数维度
	CrossRun("attacker-controlled-host", "weird", "evil")

	out := string(r.render())
	if !strings.Contains(out, `mirrorlens_cross_run_total{mode="race",outcome="ok",vote="confirmed"} 1`) {
		t.Fatalf("合法标签未记录：\n%s", out)
	}
	if !strings.Contains(out, `mirrorlens_cross_run_total{mode="other",outcome="other",vote="none"} 1`) {
		t.Fatalf("未知标签未折叠到封闭集合：\n%s", out)
	}
	// 折叠后的标签集合总量必须很小且恒定（封闭）：3 次调用 = 3 条序列
	n := strings.Count(out, "mirrorlens_cross_run_total{mode=")
	if n != 3 {
		t.Fatalf("cross_run 序列数应为 3（封闭标签），实际 %d", n)
	}
	if strings.Contains(out, "evil") || strings.Contains(out, "attacker") || strings.Contains(out, "weird") {
		t.Fatalf("原始高基数/敏感值泄漏进指标：\n%s", out)
	}
}

// 18f：metrics 未启用时 CrossRun 为空操作，不 panic。
func TestCrossRunDisabledNoop(t *testing.T) {
	Disable()
	t.Cleanup(Disable)
	CrossRun("race", "ok", "confirmed") // 不应 panic
}
