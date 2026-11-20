package gateway

import (
	"strings"
	"testing"
	"time"
)

// 18f：环形缓冲有界、最新在前、序号递增。
func TestCrossRunRingBoundedNewestFirst(t *testing.T) {
	ring := newCrossRunRing(3)
	for i := 0; i < 7; i++ {
		ring.add(CrossRunState{Outcome: "ok", Registry: "r" + string(rune('a'+i))})
	}
	snap := ring.snapshot(10)
	if len(snap) != 3 {
		t.Fatalf("容量 3 的环形缓冲快照应为 3 条，实际 %d", len(snap))
	}
	// 最新写入的 3 条：e/f/g，且 snapshot 最新在前 → g,f,e
	if snap[0].Registry != "rg" || snap[1].Registry != "rf" || snap[2].Registry != "re" {
		t.Fatalf("最新在前顺序错误：%s,%s,%s", snap[0].Registry, snap[1].Registry, snap[2].Registry)
	}
	if !(snap[0].Seq > snap[1].Seq && snap[1].Seq > snap[2].Seq) {
		t.Fatalf("序号应递增，实际 %d,%d,%d", snap[0].Seq, snap[1].Seq, snap[2].Seq)
	}
	// limit 截断
	if got := ring.snapshot(2); len(got) != 2 || got[0].Seq != snap[0].Seq {
		t.Fatalf("limit=2 应返回最新两条，实际 len=%d firstSeq=%d", len(got), got[0].Seq)
	}
}

// 18f：请求关联键脱敏——去掉查询串、截长度，不含凭据/URL。
func TestCrossRunSanitizeHint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"docker:manifest:library/nginx:latest?token=SECRET&x=1", "docker:manifest:library/nginx:latest"},
		{"https://user:pass@host/v2/x?sig=abc", "https://user:pass@host/v2/x"},
		{strings.Repeat("a", 200), strings.Repeat("a", 120)},
	}
	for _, c := range cases {
		got := sanitizeHint(c.in)
		if got != c.want {
			t.Fatalf("sanitizeHint(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(got, "?") {
			t.Fatalf("脱敏后仍含查询串：%q", got)
		}
	}
}

// 18f：&Gateway{}（测试构造）记录与快照不 panic，且时间自动填充。
func TestCrossRunRecordNilSafeWithZeroTime(t *testing.T) {
	g := &Gateway{}
	g.recordCrossRun(CrossRunState{Outcome: "ok"}) // 不应 panic
	if got := g.CrossRunSnapshot(10); len(got) != 0 {
		t.Fatalf("无 ring 的 Gateway 快照应为空，实际 %d 条", len(got))
	}

	g.crossRuns = newCrossRunRing(4)
	g.recordCrossRun(CrossRunState{Outcome: "ok"})
	snap := g.CrossRunSnapshot(10)
	if len(snap) != 1 || snap[0].At.IsZero() || time.Since(snap[0].At) > time.Minute {
		t.Fatalf("记录应自动填时间，实际 %+v", snap)
	}
}
