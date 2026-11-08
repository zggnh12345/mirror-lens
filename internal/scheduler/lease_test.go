package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func testCaps(hard int) (int, int) {
	return Config{MaxPerSource: hard}.caps()
}

// I1：held 与状态分项在同一快照内自洽。
func TestSnapshotConsistent(t *testing.T) {
	hard, dispatch := testCaps(4)
	set := newLeaseSet(1, hard, dispatch)

	l1, err := set.acquire(context.Background(), KindNormal, "blocking", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l1.Activate(); err != nil {
		t.Fatal(err)
	}
	if _, err := set.acquire(context.Background(), KindNormal, "blocking", 0); err != nil {
		t.Fatal(err)
	}
	held, reserved, active, reclaiming, bodies := set.snapshot()
	if held != 2 || reserved != 1 || active != 1 || reclaiming != 0 || bodies != 1 {
		t.Fatalf("snapshot = %d/%d/%d/%d/%d", held, reserved, active, reclaiming, bodies)
	}
	if held != reserved+active+reclaiming {
		t.Fatalf("I1 被破坏：held=%d sum=%d", held, reserved+active+reclaiming)
	}
}

// I2/I8：同源接管重叠窗口内，两个 Body 对应一个 Lease。
func TestBodiesExceedLeasesDuringTransfer(t *testing.T) {
	hard, dispatch := testCaps(2)
	set := newLeaseSet(1, hard, dispatch)

	l, _ := set.acquire(context.Background(), KindNormal, "blocking", 0)
	retireOld, err := l.Activate()
	if err != nil {
		t.Fatal(err)
	}
	tok := l.Token()
	ntok, err := l.Transfer(tok)
	if err != nil {
		t.Fatal(err)
	}
	_ = ntok
	retireNew, err := l.Activate()
	if err != nil {
		t.Fatalf("transfer 后新 Body 应能激活: %v", err)
	}

	held, _, _, _, bodies := set.snapshot()
	if held != 1 || bodies != 2 {
		t.Fatalf("重叠窗口应为 1 Lease / 2 Body，得到 %d/%d", held, bodies)
	}

	retireOld()
	retireNew()
	l.Release(l.Token())
	if held, _, _, _, bodies := set.snapshot(); held != 0 || bodies != 0 {
		t.Fatalf("退出后应归零，得到 %d/%d", held, bodies)
	}
}

// I3：normal 受 dispatch_cap 限制，handover 受 hard_max 限制。
func TestNormalCapAndHandoverCap(t *testing.T) {
	hard, dispatch := testCaps(4) // 3 / 4
	if dispatch != 3 {
		t.Fatalf("dispatch=%d", dispatch)
	}
	set := newLeaseSet(1, hard, dispatch)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := set.acquire(ctx, KindNormal, "non_blocking", 0); err != nil {
			t.Fatalf("第 %d 个 normal 应放行: %v", i+1, err)
		}
	}
	if _, err := set.acquire(ctx, KindNormal, "non_blocking", 0); !errors.Is(err, ErrLeaseAtCap) {
		t.Fatalf("第 4 个 normal 应拒绝，得到 %v", err)
	}
	if _, err := set.acquire(ctx, KindHandover, "non_blocking", 0); err != nil {
		t.Fatalf("handover 应能用预留名额: %v", err)
	}
	if _, err := set.acquire(ctx, KindHandover, "non_blocking", 0); !errors.Is(err, ErrLeaseAtCap) {
		t.Fatalf("第二个 handover 应撞硬顶，得到 %v", err)
	}
}

// I4：回收期间名额不释放，Body 退出后才归还。
func TestReclaimHoldsUntilBodyExit(t *testing.T) {
	hard, dispatch := testCaps(2)
	set := newLeaseSet(1, hard, dispatch)

	l, _ := set.acquire(context.Background(), KindNormal, "blocking", 0)
	retire, _ := l.Activate()
	if !l.Reclaim("stall") {
		t.Fatal("reclaim 应生效")
	}
	if held, _, _, reclaiming, _ := set.snapshot(); held != 1 || reclaiming != 1 {
		t.Fatalf("回收中必须占名额：held=%d reclaiming=%d", held, reclaiming)
	}
	if _, err := set.acquire(context.Background(), KindNormal, "non_blocking", 0); !errors.Is(err, ErrLeaseAtCap) {
		t.Fatalf("回收中的名额不应被复用，得到 %v", err)
	}
	retire()
	if held, _, _, _, bodies := set.snapshot(); held != 0 || bodies != 0 {
		t.Fatalf("Body 退出后应归零：%d/%d", held, bodies)
	}
}

// I5：Transfer 后旧 token 释放必须失效。
func TestLeaseTransferFencesOldOwner(t *testing.T) {
	hard, dispatch := testCaps(2)
	set := newLeaseSet(1, hard, dispatch)

	l, _ := set.acquire(context.Background(), KindNormal, "blocking", 0)
	retire, err := l.Activate()
	if err != nil {
		t.Fatal(err)
	}
	old := l.Token()
	ntok, err := l.Transfer(old)
	if err != nil {
		t.Fatal(err)
	}
	l.Release(old)
	if held := set.snapshotHeld(); held != 1 {
		t.Fatalf("旧 owner 不应释放已转交的 Lease，held=%d", held)
	}
	retire()
	l.Release(ntok)
	if held := set.snapshotHeld(); held != 0 {
		t.Fatalf("新 owner 释放后应归零，held=%d", held)
	}
}

// I6：handover 永不排队。
func TestHandoverNeverBlocks(t *testing.T) {
	hard, dispatch := testCaps(1) // 硬顶 1，无预留
	set := newLeaseSet(1, hard, dispatch)

	if _, err := set.acquire(context.Background(), KindNormal, "blocking", 0); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := set.acquire(context.Background(), KindHandover, "blocking", 0)
	if !errors.Is(err, ErrLeaseAtCap) {
		t.Fatalf("应直接返回 AtCap，得到 %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("handover 阻塞了 %v", d)
	}
}

// I7：等待者不占名额，释放后按 FIFO 直接交接。
func TestWaitWithoutHoldingLease(t *testing.T) {
	hard, dispatch := testCaps(1)
	set := newLeaseSet(1, hard, dispatch)

	l, _ := set.acquire(context.Background(), KindNormal, "blocking", 0)

	got := make(chan *Lease, 1)
	go func() {
		nl, err := set.acquire(context.Background(), KindNormal, "blocking", 0)
		if err == nil {
			got <- nl
		}
	}()
	time.Sleep(30 * time.Millisecond)
	if held := set.snapshotHeld(); held != 1 {
		t.Fatalf("等待者不应占名额，held=%d", held)
	}
	l.Release(l.Token())
	select {
	case nl := <-got:
		if held := set.snapshotHeld(); held != 1 {
			t.Fatalf("交接后 held=%d", held)
		}
		nl.Release(nl.Token())
	case <-time.After(time.Second):
		t.Fatal("等待者未被唤醒")
	}
	if held := set.snapshotHeld(); held != 0 {
		t.Fatalf("最终应归零，held=%d", held)
	}
}

// I8：最后一个 Body 退出前，Release 不得归还名额。
func TestReleaseWaitsForAllBodies(t *testing.T) {
	hard, dispatch := testCaps(2)
	set := newLeaseSet(1, hard, dispatch)

	l, _ := set.acquire(context.Background(), KindNormal, "blocking", 0)
	r1, _ := l.Activate()
	tok := l.Token()
	if _, err := l.Transfer(tok); err != nil {
		t.Fatal(err)
	}
	r2, _ := l.Activate()

	l.Release(l.Token())
	if held, _, _, reclaiming, bodies := set.snapshot(); held != 1 || reclaiming != 1 || bodies != 2 {
		t.Fatalf("owner 释放后仍应占名额：%d/%d/%d", held, reclaiming, bodies)
	}
	r1()
	if held := set.snapshotHeld(); held != 1 {
		t.Fatalf("还有一个 Body 存活，held=%d", held)
	}
	r2()
	if held := set.snapshotHeld(); held != 0 {
		t.Fatalf("全部退出后应归零，held=%d", held)
	}
}

// 有限等待超时不得影响账面。
func TestWaitThenFallbackTimeout(t *testing.T) {
	hard, dispatch := testCaps(1)
	set := newLeaseSet(1, hard, dispatch)

	l, _ := set.acquire(context.Background(), KindNormal, "blocking", 0)
	start := time.Now()
	_, err := set.acquire(context.Background(), KindNormal, "wait_then_fallback", 50*time.Millisecond)
	if !errors.Is(err, ErrLeaseWaitTimeout) {
		t.Fatalf("应超时，得到 %v", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("等待时长异常：%v", d)
	}
	if held := set.snapshotHeld(); held != 1 {
		t.Fatalf("超时不应改变名额，held=%d", held)
	}
	l.Release(l.Token())
}

// 并发压测：所有路径结束后账面必须归零（-race 下跑）。
func TestLeaseConcurrentBalanced(t *testing.T) {
	hard, dispatch := testCaps(4)
	set := newLeaseSet(1, hard, dispatch)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mode := "blocking"
			if i%3 == 0 {
				mode = "wait_then_fallback"
			}
			l, err := set.acquire(context.Background(), KindNormal, mode, 20*time.Millisecond)
			if err != nil {
				return
			}
			retire, err := l.Activate()
			if err != nil {
				l.Release(l.Token())
				return
			}
			if i%5 == 0 {
				l.Reclaim("cancel")
			}
			retire()
			l.Release(l.Token())
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(time.Second)
	for {
		if held := set.snapshotHeld(); held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("账面未归零：held=%d", set.snapshotHeld())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *leaseSet) snapshotHeld() int {
	h, _, _, _, _ := s.snapshot()
	return h
}

// I8 补充：Transfer 后新旧 Body 各自退出，旧 Token 的 Release 不能动当前
// Owner，但最后一个 Body 退出时名额必须归还，否则租约漏账。
func TestTransferOldTokenFinalizesWhenLastBody(t *testing.T) {
	hard, dispatch := testCaps(2)
	set := newLeaseSet(7, hard, dispatch)

	l, err := set.acquire(context.Background(), KindNormal, "blocking", 0)
	if err != nil {
		t.Fatal(err)
	}
	oldTok := l.Token()
	retireOld, err := l.Activate()
	if err != nil {
		t.Fatal(err)
	}

	tok2, err := l.Transfer(oldTok)
	if err != nil {
		t.Fatal(err)
	}
	retireNew, err := l.Activate()
	if err != nil {
		t.Fatal(err)
	}

	// 旧 Body 先退出：新 Owner 还活着，名额不能还。
	retireOld()
	l.Release(oldTok)
	if held, _, _, _, bodies := set.snapshot(); held != 1 || bodies != 1 {
		t.Fatalf("旧 Body 退出后 held/bodies = %d/%d，期望 1/1", held, bodies)
	}

	// 新 Body 退出：最后一个引用，必须归还。
	retireNew()
	l.Release(tok2)
	if held, _, _, _, bodies := set.snapshot(); held != 0 || bodies != 0 {
		t.Fatalf("最后一个 Body 退出后 held/bodies = %d/%d，期望 0/0", held, bodies)
	}
}
