package scheduler

import (
	"context"
	"testing"
	"time"
)

// 手动「优先」的源排在同组最前：这是用户的明确意图，优先级高于评分。
func TestOrderPrefersPinnedSource(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5})
	s.now = func() time.Time { return time.Unix(1000, 0) }

	out := s.order("docker", []Candidate{
		{ID: 1, Name: "fast", Score: 0.95},
		{ID: 2, Name: "pinned", Score: 0.30, Preferred: true},
	})
	if len(out) != 2 || out[0].ID != 2 {
		t.Fatalf("优先源应排最前，实际 %v", out)
	}
}

// 优先源不能压过并发上限：占满名额时让位给空闲源，否则请求会全堆在一台上。
func TestOrderPinnedYieldsWhenAtCap(t *testing.T) {
	s := New(nil, Config{Enabled: true, MaxCandidates: 5, MaxPerSource: 1})
	s.now = func() time.Time { return time.Unix(1000, 0) }

	rel, err := s.Acquire(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	out := s.order("docker", []Candidate{
		{ID: 1, Name: "free", Score: 0.10},
		{ID: 2, Name: "pinned-full", Score: 0.90, Preferred: true},
	})
	if len(out) != 2 || out[0].ID != 1 {
		t.Fatalf("占满名额的优先源应后移，实际 %v", out)
	}
}
