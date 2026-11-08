package scheduler

import (
	"context"
	"testing"
	"time"
)

// 分组优先级压过评分：用户把慢源放进「好组」是他的选择，但组的顺序必须生效，
// 否则「按分组优先使用」就是个摆设。
func TestOrderGroupRankBeatsScore(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5})
	s.now = func() time.Time { return time.Unix(1000, 0) }

	out := s.order("docker", []Candidate{
		{ID: 1, Name: "fast-ungrouped", Score: 0.99, GroupRank: 0},
		{ID: 2, Name: "slow-in-group", Score: 0.10, GroupRank: 1},
	})
	if len(out) != 2 || out[0].ID != 2 {
		t.Fatalf("优先组里的源应排最前，实际 %v", out)
	}
}

// 同组内仍然按评分/★排序，组只决定档位。
func TestOrderGroupThenScore(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5})
	s.now = func() time.Time { return time.Unix(1000, 0) }

	out := s.order("docker", []Candidate{
		{ID: 1, Name: "g1-slow", Score: 0.20, GroupRank: 1},
		{ID: 2, Name: "g1-fast", Score: 0.90, GroupRank: 1},
		{ID: 3, Name: "g2-fast", Score: 0.95, GroupRank: 2},
	})
	if out[0].ID != 2 || out[1].ID != 1 || out[2].ID != 3 {
		t.Fatalf("组内应按评分、组间应按 rank，实际 %v", []int64{out[0].ID, out[1].ID, out[2].ID})
	}
}

// 容量只在组内生效：高优先组即使有一台满员，也不会被低优先组插队；
// 同组内满员的那台让给有空位的同组源。
func TestOrderAtCapWithinGroup(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5, MaxPerSource: 2})
	s.now = func() time.Time { return time.Unix(1000, 0) }

	rel, err := s.Acquire(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	out := s.order("docker", []Candidate{
		{ID: 1, Name: "g2-free", Score: 0.90, GroupRank: 2},
		{ID: 2, Name: "g1-full", Score: 0.90, GroupRank: 1},
		{ID: 3, Name: "g1-free", Score: 0.10, GroupRank: 1},
	})
	got := []int64{out[0].ID, out[1].ID, out[2].ID}
	if got[0] != 3 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("期望 [3 2 1]（组1优先，组内空位优先），实际 %v", got)
	}
}
