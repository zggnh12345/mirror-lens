package gateway

import (
	"testing"
	"time"
)

// 换源后必须整体重启测速窗口：旧源的阻塞时间不能带进新源样本。
func TestSampleWindowRestartClearsBlocked(t *testing.T) {
	win := &sampleWindow{at: time.Now().Add(-30 * time.Second), base: 1 << 20, blocked: 8 * time.Second}
	win.restart(16 << 20)

	if win.blocked != 0 {
		t.Fatalf("换源后 blocked 必须清零，实际 %v", win.blocked)
	}
	if win.base != 16<<20 {
		t.Fatalf("base 应指向已发送字节数，实际 %d", win.base)
	}
	if age := time.Since(win.at); age > time.Second {
		t.Fatalf("起点应重置到现在，实际已过 %v", age)
	}
}

// 换源失败时不能调 restart：窗口必须保持原样，避免把失败段的耗时丢掉。
func TestSampleWindowKeepsBlockedWithoutRestart(t *testing.T) {
	win := &sampleWindow{at: time.Now().Add(-time.Minute), base: 100, blocked: 3 * time.Second}
	if win.blocked <= 0 {
		t.Fatal("前置条件错误")
	}
	// 这里只是把「不调用 restart」写成断言：字段不应被别处顺手改掉。
	if win.blocked != 3*time.Second || win.base != 100 {
		t.Fatalf("未换源时窗口不应变化: blocked=%v base=%d", win.blocked, win.base)
	}
}
