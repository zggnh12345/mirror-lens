package scheduler

import (
	"testing"
	"time"
)

// 手动模式：用户设定的前 3 个源必须排在评分更高的源之前。
func TestManualOrderBeatsScore(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5})
	s.now = func() time.Time { return time.Unix(1000, 0) }
	s.manualMode = "manual"
	s.manual = map[string][]int64{"docker": {3, 1}}

	out := s.order("docker", []Candidate{
		{ID: 1, Name: "score-high", Score: 0.90},
		{ID: 2, Name: "score-mid", Score: 0.80},
		{ID: 3, Name: "manual-first", Score: 0.10},
	})
	got := []int64{out[0].ID, out[1].ID, out[2].ID}
	want := []int64{3, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("手动顺序应为 %v，实际 %v", want, got)
		}
	}
}

// 自动模式：手动列表存在也不能生效，仍按评分排。
func TestAutoModeIgnoresManualList(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5})
	s.now = func() time.Time { return time.Unix(1000, 0) }
	s.manualMode = "auto"
	s.manual = map[string][]int64{"docker": {3}}

	out := s.order("docker", []Candidate{
		{ID: 1, Name: "a", Score: 0.90},
		{ID: 3, Name: "b", Score: 0.10},
	})
	if out[0].ID != 1 {
		t.Fatalf("自动模式应按评分排，实际 %v", out)
	}
}

// 手动名次只认前 3 个，第 4 个及以后不参与固定。
func TestManualOrderCapsAtThree(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5})
	s.now = func() time.Time { return time.Unix(1000, 0) }
	s.manualMode = "manual"
	s.manual = map[string][]int64{"github": {4, 5, 6, 7}}

	out := s.order("github", []Candidate{
		{ID: 7, Name: "fourth", Score: 0.99},
		{ID: 4, Name: "first", Score: 0.10},
	})
	if out[0].ID != 4 {
		t.Fatalf("手动第一位应排最前，实际 %v", out)
	}
}
