package scheduler

import (
	"context"
	"testing"
	"time"
)

func dynSched(dynamic bool, max int) *Scheduler {
	return New(nil, Config{Enabled: true, Dynamic: dynamic, MaxPerSource: max, MaxCandidates: 3})
}

func TestGateBlocksAtLimitAndReleases(t *testing.T) {
	g := newGate(2, 4)
	ctx := context.Background()
	r1, err := g.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := g.acquire(ctx)
	if g.current() != 2 {
		t.Fatalf("limit = %d", g.current())
	}

	done := make(chan struct{})
	go func() {
		r3, err := g.acquire(ctx)
		if err == nil {
			r3()
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("第三个名额不该立刻拿到")
	case <-time.After(50 * time.Millisecond):
	}
	r1()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("释放后第三个名额没被唤醒")
	}
	r2()
	if n := g.current(); n != 2 {
		t.Fatalf("limit 被释放动作改掉了：%d", n)
	}
}

func TestGateAcquireHonorsContext(t *testing.T) {
	g := newGate(1, 4)
	release, _ := g.acquire(context.Background())
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := g.acquire(ctx); err == nil {
		t.Fatal("满员时应该等 ctx 超时")
	}
}

func TestGateSetLimitWakesWaiters(t *testing.T) {
	g := newGate(1, 4)
	release, _ := g.acquire(context.Background())
	got := make(chan struct{})
	go func() {
		r, err := g.acquire(context.Background())
		if err == nil {
			close(got)
			r()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	g.setLimit(2)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("扩容后等待者没被唤醒")
	}
	release()
}

func TestDynamicStartsAtMax(t *testing.T) {
	s := dynSched(true, 4)
	if n := s.Limit(int64(7)); n != 4 {
		t.Fatalf("没有样本时应保持配置上限 4，得到 %d", n)
	}
}

func TestDynamicGrowsWhenQueued(t *testing.T) {
	s := dynSched(true, 4)
	const id = int64(7)
	s.gateFor(id).setLimit(2)
	// 先制造排队：占满两个名额，再排一个等待者。
	r1, err := s.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	go func() {
		r, err := s.Acquire(context.Background(), id)
		if err == nil {
			close(waiting)
			r()
		}
	}()
	time.Sleep(30 * time.Millisecond)

	// 吞吐很好且确实在排队 → 加一路。
	s.Observe(id, 8<<20, 2*time.Second, true, 200)
	if n := s.Limit(id); n != 3 {
		t.Fatalf("排队且吞吐正常应扩容到 3，得到 %d", n)
	}
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("扩容后等待者应被放行")
	}
	r1()
	r2()
}

func TestDynamicDoesNotGrowWithoutQueueing(t *testing.T) {
	s := dynSched(true, 4)
	const id = int64(8)
	for i := 0; i < 5; i++ {
		s.Observe(id, 8<<20, 2*time.Second, true, 200)
	}
	if n := s.Limit(id); n != 4 {
		t.Fatalf("没有排队就不该扩容，得到 %d", n)
	}
}

func TestDynamicHalvesOnFailure(t *testing.T) {
	s := dynSched(true, 8)
	const id = int64(9)
	g := s.gateFor(id)
	g.setLimit(8)
	s.Observe(id, 0, 0, false, 429)
	if n := s.Limit(id); n != 4 {
		t.Fatalf("429 后应减半到 4，得到 %d", n)
	}
	s.Observe(id, 0, 0, false, 503)
	s.Observe(id, 0, 0, false, 503)
	s.Observe(id, 0, 0, false, 503)
	if n := s.Limit(id); n != 1 {
		t.Fatalf("连续失败应压到 1，得到 %d", n)
	}
}

func TestDynamicShrinksWhenThroughputDrops(t *testing.T) {
	s := dynSched(true, 4)
	const id = int64(10)
	s.gateFor(id).setLimit(4)
	// 建立基线：10 MB/s
	s.Observe(id, 20<<20, 2*time.Second, true, 206)
	// 掉到 3 MB/s（低于基线 60%）→ 退一路
	s.Observe(id, 6<<20, 2*time.Second, true, 206)
	if n := s.Limit(id); n != 3 {
		t.Fatalf("吞吐掉 70%% 应退到 3，得到 %d", n)
	}
}

func TestLimitFixedWhenDynamicOff(t *testing.T) {
	s := dynSched(false, 4)
	const id = int64(11)
	s.Observe(id, 100<<20, time.Second, true, 200)
	s.Observe(id, 0, 0, false, 429)
	if n := s.Limit(id); n != 4 {
		t.Fatalf("关掉动态后上限必须固定为 4，得到 %d", n)
	}
}

func TestDynamicIgnoresZeroByteSamples(t *testing.T) {
	s := dynSched(true, 4)
	const id = int64(12)
	s.gateFor(id).setLimit(3)
	s.Observe(id, 0, time.Second, true, 200)
	if n := s.Limit(id); n != 3 {
		t.Fatalf("没有字节数的样本不该动上限，得到 %d", n)
	}
}
