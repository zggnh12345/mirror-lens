package scheduler

import (
	"context"
	"testing"
	"time"
)

// 排队者取消（客户端断开）后，queued 必须还回去：
// 只加不减会让调度一直以为还有人在排队，从而误判该扩容。
func TestGateQueuedRollsBackOnCancel(t *testing.T) {
	g := newGate(1, 1, 4)
	rel, err := g.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, e := g.acquire(ctx)
		done <- e
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		q := g.queued
		g.mu.Unlock()
		if q == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等到超时也没进入排队，queued=%d", q)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	if err := <-done; err == nil {
		t.Fatal("取消后应返回错误")
	}
	g.mu.Lock()
	q := g.queued
	g.mu.Unlock()
	if q != 0 {
		t.Fatalf("取消后 queued 应归零，实际 %d", q)
	}
}
