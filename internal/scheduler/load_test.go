package scheduler

import (
	"context"
	"testing"
	"time"
)

// 名额是硬的：第 N+1 个请求排队，而不是被当成源站故障。
func TestAcquireCapsPerSource(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://m.example", true, 100, 900)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, MaxPerSource: 2})

	ctx := context.Background()
	r1, err := s.Acquire(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Acquire(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.InFlight(id); got != 2 {
		t.Fatalf("InFlight want 2 got %d", got)
	}

	wait, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(wait, id); err == nil {
		t.Fatalf("名额满时不应立刻拿到许可")
	} else if err != context.DeadlineExceeded {
		t.Fatalf("等待超时应返回 ctx 错误，得到 %v", err)
	}

	r1()
	if got := s.InFlight(id); got != 1 {
		t.Fatalf("释放后 InFlight want 1 got %d", got)
	}
	r3, err := s.Acquire(ctx, id)
	if err != nil {
		t.Fatalf("释放后应能拿到名额：%v", err)
	}
	r2()
	r3()
	if got := s.InFlight(id); got != 0 {
		t.Fatalf("全部释放后 InFlight want 0 got %d", got)
	}

	// 并发名额不碰健康：没有 source_events，候选照旧。
	if _, failN, _, err := st.SourceEvents(id, 10); err != nil {
		t.Fatal(err)
	} else if failN != 0 {
		t.Fatalf("排队/等待不该记失败，实际 %d 条", failN)
	}
	if got := s.Candidates("raw"); len(got) != 1 {
		t.Fatalf("源应仍然可用，实际 %d 个候选", len(got))
	}
}

// 负数 = 不限，此时 Acquire 是空操作，InFlight 恒为 0。
func TestAcquireUnlimited(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://m.example", true, 100, 900)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, MaxPerSource: -1})

	var rels []func()
	for i := 0; i < 20; i++ {
		r, err := s.Acquire(context.Background(), id)
		if err != nil {
			t.Fatalf("不限并发时不该阻塞：%v", err)
		}
		rels = append(rels, r)
	}
	if got := s.InFlight(id); got != 0 {
		t.Fatalf("不限并发时不该计数，得到 %d", got)
	}
	for _, r := range rels {
		r()
	}
}

// 占满名额的源排到后面但分数不变；同分按负载低者优先。
func TestOrderPushesFullSourceBack(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 900)
	b := addSource(t, st, "https://b.example", true, 100, 900)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, MaxPerSource: 1})

	got := s.Candidates("raw")
	if len(got) != 2 {
		t.Fatalf("want 2 got %d", len(got))
	}
	first := got[0]
	scoreBefore := first.Score

	rel, err := s.Acquire(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	after := s.Candidates("raw")
	if len(after) != 2 {
		t.Fatalf("占满名额的源仍应留在候选里，实际 %d", len(after))
	}
	if after[0].ID == first.ID {
		t.Fatalf("占满名额的源应排到后面，实际仍在队首")
	}
	if after[1].Score != scoreBefore {
		t.Fatalf("分数不应被负载影响：%v → %v", scoreBefore, after[1].Score)
	}
	rel()
	if got := s.Candidates("raw"); len(got) != 2 || got[0].ID != first.ID {
		t.Fatalf("名额释放后应恢复原顺序")
	}
	_ = a
	_ = b
}
