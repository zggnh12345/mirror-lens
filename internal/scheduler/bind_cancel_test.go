package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
)

// Reclaim 与 BindCancel 的三种相对时序都必须取消 Attempt：
// 注册前后都算失效，不能只覆盖“先注册、后回收”。
func TestBindCancelOrdering(t *testing.T) {
	newLease := func(t *testing.T) *Lease {
		t.Helper()
		st := newStore(t)
		id := addSource(t, st, "https://mirror.example", true, 100, 1000)
		s := New(st, Config{Enabled: true, MaxPerSource: 2, Admission: "blocking"})
		l, err := s.AcquireLease(context.Background(), id, KindNormal)
		if err != nil || l == nil {
			t.Fatalf("acquire: %v", err)
		}
		return l
	}

	t.Run("先注册后回收", func(t *testing.T) {
		l := newLease(t)
		var called atomic.Bool
		unbind := l.BindCancel(func() { called.Store(true) })
		l.Reclaim("test")
		if !called.Load() {
			t.Fatal("Reclaim 没有取消已登记的 Attempt")
		}
		unbind() // 已解绑后再次解绑必须安全
	})

	t.Run("先回收后注册", func(t *testing.T) {
		l := newLease(t)
		l.Reclaim("test")
		var called atomic.Bool
		unbind := l.BindCancel(func() { called.Store(true) })
		if !called.Load() {
			t.Fatal("名额已回收，BindCancel 必须立即取消")
		}
		unbind()
	})

	t.Run("已释放后注册", func(t *testing.T) {
		l := newLease(t)
		tok := l.Token()
		l.Release(tok)
		var called atomic.Bool
		l.BindCancel(func() { called.Store(true) })
		if !called.Load() {
			t.Fatal("名额已释放，BindCancel 必须立即取消而不是静默登记")
		}
	})

	t.Run("正常解绑后回收", func(t *testing.T) {
		l := newLease(t)
		var called atomic.Bool
		unbind := l.BindCancel(func() { called.Store(true) })
		unbind()
		l.Reclaim("test")
		if called.Load() {
			t.Fatal("已解绑的 Attempt 不应再被取消")
		}
	})
}
