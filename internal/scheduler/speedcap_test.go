package scheduler

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 速度项必须能区分快慢：12MB/s 饱和点下 5MB/s 与 20MB/s 不能同分。
func TestScoreSpeedCapDifferentiates(t *testing.T) {
	st := func(kbps int64) store.SourceStat {
		return store.SourceStat{Samples: 10, OK: 10, LatencyMS: 50, SpeedKBps: kbps}
	}
	fast := score(st(20000), 12000)
	mid := score(st(5000), 12000)
	slow := score(st(1500), 12000)
	if !(fast > mid && mid > slow) {
		t.Fatalf("速度分未拉开：fast=%.3f mid=%.3f slow=%.3f", fast, mid, slow)
	}
	if d := fast - mid; d < 0.1 {
		t.Fatalf("20MB/s 与 5MB/s 的总分差应 >0.1，实际 %.3f", d)
	}
	// 饱和点以上同分（12MB/s 与 30MB/s 都是满分）。
	if a, b := score(st(12000), 12000), score(st(30000), 12000); a != b {
		t.Fatalf("饱和点以上应同分：%.4f vs %.4f", a, b)
	}
}
