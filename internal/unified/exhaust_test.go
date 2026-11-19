package unified

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// P 耗尽必须关闭所有未完成 Body、取消在途 Attempt，且幂等。
func TestExhaustClosesBodiesAndCancelsAttempts(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()

	ctx2, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(s.Context(), cancel)
	defer stop()

	var closed atomic.Int32
	for i := 0; i < 3; i++ {
		if _, ok := s.TrackBody(func() { closed.Add(1) }); !ok {
			t.Fatalf("第 %d 个 Body 登记失败", i+1)
		}
	}
	// 已经打开多个 Body 时耗尽：全部关闭且只关一次。
	s.Exhaust("P")
	if got := closed.Load(); got != 3 {
		t.Fatalf("应关闭 3 个未完成 Body，实际 %d", got)
	}
	select {
	case <-ctx2.Done():
	case <-time.After(time.Second):
		t.Fatal("耗尽后必须取消在途 Attempt")
	}
	// 幂等：再次触发不得重复关闭。
	s.Exhaust("P")
	if got := closed.Load(); got != 3 {
		t.Fatalf("重复耗尽导致重复关闭: %d", got)
	}
	// 耗尽后新登记的 Body 必须立刻关闭，不能进入「稍后清理」。
	if _, ok := s.TrackBody(func() { closed.Add(1) }); ok {
		t.Fatal("耗尽后仍接受新的 Body 登记")
	}
	if got := closed.Load(); got != 4 {
		t.Fatalf("耗尽后新 Body 应立即关闭，实际关闭数 %d", got)
	}
}

// 耗尽只允许发生一次，且不影响重复调用 Exhausted 的幂等读。
func TestExhaustIdempotentFlag(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	if s.Exhausted() {
		t.Fatal("初始不应处于耗尽态")
	}
	s.Exhaust("P")
	s.Exhaust("R")
	if !s.Exhausted() {
		t.Fatal("耗尽标志未置位")
	}
}
