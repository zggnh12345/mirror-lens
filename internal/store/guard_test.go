package store

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

func TestGuardFilterDoesNotMutateInput(t *testing.T) {
	g := NewRecoveryGuard()
	k1 := GuardKey{SourceID: 1, Family: "registry", Scope: "quay.io"}
	k2 := GuardKey{SourceID: 2, Family: "registry", Scope: "quay.io"}
	in := []SourceStat{{ID: 1}, {ID: 2}}

	tok := g.Block(k1, 3)
	out := FilterByGuard(g, in, func(s SourceStat) GuardKey {
		return GuardKey{SourceID: s.ID, Family: "registry", Scope: "quay.io"}
	})
	if len(out) != 1 || out[0].ID != k2.SourceID {
		t.Fatalf("屏蔽后结果=%+v", out)
	}
	if len(in) != 2 || in[0].ID != 1 || in[1].ID != 2 {
		t.Fatalf("输入列表被修改: %+v", in)
	}

	// 解除后，同一份原始列表必须能恢复被屏蔽的源（证明缓存未被就地过滤）。
	if !g.UnblockIfMatch(k1, tok) {
		t.Fatal("解除失败")
	}
	out = FilterByGuard(g, in, func(s SourceStat) GuardKey {
		return GuardKey{SourceID: s.ID, Family: "registry", Scope: "quay.io"}
	})
	if len(out) != 2 {
		t.Fatalf("解除后未恢复: %+v", out)
	}
}

func TestGuardTokenLifecycleAcrossGenerations(t *testing.T) {
	g := NewRecoveryGuard()
	k := GuardKey{SourceID: 1, Family: "registry", Scope: "quay.io"}

	old := g.Block(k, 5) // gen=5 的失败
	cur := g.Block(k, 6) // 凭据推进后新的失败，seq 更大
	if g.UnblockIfMatch(k, old) {
		t.Fatal("gen=5 的旧 token 不应解除 gen=6 的屏蔽")
	}
	if !g.Blocked(k) {
		t.Fatal("新屏蔽被旧 token 清掉了")
	}
	if g.UnblockIfMatch(k, GuardToken{Gen: 6, Seq: old.Seq}) {
		t.Fatal("seq 不匹配不应解除")
	}
	if !g.UnblockIfMatch(k, cur) {
		t.Fatal("当前 token 应能解除")
	}
	if g.Blocked(k) {
		t.Fatal("解除后仍处于屏蔽")
	}
}

// 同一代次内的 ABA：旧序号不得清掉后来更新的条目。
func TestGuardTokenRejectsStaleSeqSameGen(t *testing.T) {
	g := NewRecoveryGuard()
	k := GuardKey{SourceID: 1, Family: "registry", Scope: "quay.io"}
	older := g.Block(k, 0)
	newer := g.Block(k, 0)
	if g.UnblockIfMatch(k, older) {
		t.Fatal("旧 seq 不应解除")
	}
	if !g.UnblockIfMatch(k, newer) {
		t.Fatal("新 seq 应能解除")
	}
}

// epoch 在过滤期间变化时整体重试；三次耗尽必须返回空集，不能退化为未过滤列表。
func TestGuardFilterRetryExhaustionFailsClosed(t *testing.T) {
	g := NewRecoveryGuard()
	before := metrics.GuardFilterExhaustedTotal()
	key := GuardKey{Family: "registry", Scope: "q"}
	// 模拟「过滤期间持续有新的 Block / Unblock」：每次取键都改变 epoch。
	out := FilterByGuard(g, []int{1, 2, 3}, func(i int) GuardKey {
		g.Block(GuardKey{SourceID: int64(1000 + i), Family: "registry", Scope: "other"}, 0)
		return key
	})
	if len(out) != 0 {
		t.Fatalf("耗尽后应返回空集，实际 %v", out)
	}
	if g.Exhausted() != 1 {
		t.Fatalf("计数器=%d，期望 1", g.Exhausted())
	}
	if got := metrics.GuardFilterExhaustedTotal() - before; got != 1 {
		t.Fatalf("guard_filter_retry_exhausted_total 增量=%d，期望 1", got)
	}
}

// 断言 36（Store 侧）：Store 持有的必须是装配点注入的同一个指针，
// 不是「另一个同样非 nil 的 Guard」。
func TestStoreHoldsInjectedGuardIdentity(t *testing.T) {
	st, _ := newTestStore(t)
	g := NewRecoveryGuard()
	if err := st.SetRecoveryGuard(g); err != nil {
		t.Fatal(err)
	}
	if st.guard == nil || st.RecoveryGuard() == nil {
		t.Fatal("Guard 未注入")
	}
	if st.guard != g || st.RecoveryGuard() != g {
		t.Fatal("Store 未持有装配点注入的同一 Guard 指针")
	}
}
