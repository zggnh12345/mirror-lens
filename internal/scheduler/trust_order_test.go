package scheduler

import (
	"testing"
	"time"
)

// 同分时先走内容可信度高的那台：分数只看可用率/延迟/速度，可信度是另一条轴。
func TestOrderPrefersTrustedOnTie(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5})
	s.now = func() time.Time { return time.Unix(1000, 0) }

	in := []Candidate{
		{ID: 1, Name: "unknown", Score: 0.7, Trust: 0},
		{ID: 2, Name: "proven", Score: 0.7, Trust: 3},
		{ID: 3, Name: "verified", Score: 0.7, Trust: 2},
	}
	out := s.order("docker", in)
	if len(out) != 3 {
		t.Fatalf("应有 3 个候选，实际 %d", len(out))
	}
	if out[0].ID != 2 || out[1].ID != 3 || out[2].ID != 1 {
		t.Fatalf("顺序应为 长期一致 > 内容一致 > 未验证，实际 %v %v %v", out[0].ID, out[1].ID, out[2].ID)
	}
}

// 分数不同时仍然看分数：可信度不能把一台慢一倍的源顶到前面。
func TestOrderScoreBeatsTrust(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5})
	s.now = func() time.Time { return time.Unix(1000, 0) }

	out := s.order("docker", []Candidate{
		{ID: 1, Name: "fast-unverified", Score: 0.9, Trust: 0},
		{ID: 2, Name: "slow-proven", Score: 0.6, Trust: 3},
	})
	if out[0].ID != 1 {
		t.Fatalf("高分应排在前面，实际 %v", out[0].ID)
	}
}
