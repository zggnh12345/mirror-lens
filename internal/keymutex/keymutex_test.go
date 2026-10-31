package keymutex

import (
	"sync"
	"testing"
	"time"
)

// 同一个 key 必须互斥：第二个 Lock 要等第一个解锁。
func TestSameKeySerializes(t *testing.T) {
	k := New()
	unlock := k.Lock("a")

	got := make(chan struct{})
	go func() {
		u := k.Lock("a")
		close(got)
		u()
	}()

	select {
	case <-got:
		t.Fatal("第二个 Lock 不该在解锁前拿到锁")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("解锁后第二个 Lock 没拿到锁")
	}
}

// 不同 key 不互斥。
func TestDifferentKeysRunConcurrently(t *testing.T) {
	k := New()
	unlock := k.Lock("a")
	defer unlock()

	got := make(chan struct{})
	go func() {
		u := k.Lock("b")
		close(got)
		u()
	}()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("不同 key 被错误地串行化了")
	}
}

// 没有人持有或等待时必须回收，否则锁表会一直涨。
func TestEntriesReclaimed(t *testing.T) {
	k := New()
	for i := 0; i < 1000; i++ {
		u := k.Lock("k" + string(rune('a'+i%26)))
		u()
	}
	if n := k.Len(); n != 0 {
		t.Fatalf("解锁后应全部回收，实际还剩 %d", n)
	}
}

// 等待中的协程也要计入引用：解锁方减到 0 删除条目时，
// 若还有人在等，就会有两把不同的锁保护同一个 key。
func TestWaiterKeepsEntryAlive(t *testing.T) {
	k := New()
	u1 := k.Lock("a")

	var wg sync.WaitGroup
	wg.Add(1)
	entered := make(chan struct{})
	go func() {
		defer wg.Done()
		u2 := k.Lock("a")
		close(entered)
		u2()
	}()
	time.Sleep(20 * time.Millisecond) // 让它进入等待
	u1()
	<-entered
	wg.Wait()
	if n := k.Len(); n != 0 {
		t.Fatalf("全部结束后应回收，实际 %d", n)
	}
}
