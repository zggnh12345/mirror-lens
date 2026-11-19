package unified

import (
	"errors"
	"sync"
	"testing"
)

// F19 冻结：Claim 只有「链关闭 + ActiveAttempts==0 + 从未 Dispatched」可释放。
// 本文件逐条把这些规则钉成可执行断言。

func mustClaim(t *testing.T, s *Scope, chain *Chain, id Identity) *ClaimHandle {
	t.Helper()
	h, err := s.Claim(chain.ID(), id)
	if err != nil {
		t.Fatalf("Claim(%s): %v", chain.ID(), err)
	}
	return h
}

func mustAttempt(t *testing.T, c *Chain, h *ClaimHandle) *Attempt {
	t.Helper()
	a, err := c.RegisterAttempt(h)
	if err != nil {
		t.Fatalf("RegisterAttempt: %v", err)
	}
	return a
}

// A 已 Dispatched 后链关闭：Claim 必须保持 Dispatched，其他链不能取得。
func TestClaimDispatchedSurvivesChainClose(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	id := testIdentity()

	a, err := s.NewChain("A", ChainA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.NewChain("B", ChainB)
	if err != nil {
		t.Fatal(err)
	}
	h := mustClaim(t, s, a, id)
	att := mustAttempt(t, a, h)
	if err := att.Dispatch(); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	att.Complete()
	a.Close()

	if st, ok := s.ClaimStateOf(id); !ok || st != ClaimDispatched {
		t.Fatalf("链关闭后 Claim 状态 = %v(ok=%v)，期望 dispatched", st, ok)
	}
	if _, err := s.Claim(b.ID(), id); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("已 Dispatched 的 Claim 被另一条链取得：err=%v", err)
	}
}

// 从未 Dispatched：链关闭且无在途 Attempt 后允许释放，后续链可重新预占。
func TestClaimReleasedAfterAllPreparedAborted(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	id := testIdentity()

	a, _ := s.NewChain("A", ChainA)
	b, _ := s.NewChain("B", ChainB)
	h := mustClaim(t, s, a, id)
	att := mustAttempt(t, a, h)
	att.Abort()
	a.Close()

	if st, ok := s.ClaimStateOf(id); !ok || st != ClaimReleased {
		t.Fatalf("全 Prepared/Aborted 关闭后状态 = %v(ok=%v)，期望 released", st, ok)
	}
	h2, err := s.Claim(b.ID(), id)
	if err != nil {
		t.Fatalf("释放后另一条链应能取得：%v", err)
	}
	if h2.ChainID() != b.ID() {
		t.Fatalf("取得者 = %s，期望 %s", h2.ChainID(), b.ID())
	}
}

// 链关闭时仍有在途 Attempt：释放必须延后到最后一个 Attempt 终止，且期间
// 其他链不能取得。
func TestChainCloseDefersReleaseUntilActiveZero(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	id := testIdentity()

	a, _ := s.NewChain("A", ChainA)
	b, _ := s.NewChain("B", ChainB)
	h := mustClaim(t, s, a, id)
	att := mustAttempt(t, a, h)

	a.Close()
	if st, _ := s.ClaimStateOf(id); st != ClaimReserved {
		t.Fatalf("有在途 Attempt 时状态 = %v，期望 reserved（释放被推迟）", st)
	}
	if _, err := s.Claim(b.ID(), id); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("在途 Attempt 未结束时另一条链取得了 Claim：err=%v", err)
	}

	att.Abort()
	if st, _ := s.ClaimStateOf(id); st != ClaimReleased {
		t.Fatalf("最后一个 Attempt 结束后状态 = %v，期望 released", st)
	}
	if _, err := s.Claim(b.ID(), id); err != nil {
		t.Fatalf("释放后另一条链应能取得：%v", err)
	}
}

// A 竞速阶段结束不等于 Chain 结束：同一条链必须还能继续注册顺序补位 Attempt，
// 且已 Dispatched 的 Claim 仍是同一个（不重复建 Claim、不重复 Dispatched）。
func TestSameChainRaceThenSequentialFallback(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	id := testIdentity()

	a, _ := s.NewChain("A", ChainA)
	h1 := mustClaim(t, s, a, id)
	first := mustAttempt(t, a, h1)
	if err := first.Dispatch(); err != nil {
		t.Fatalf("竞速 Attempt Dispatch: %v", err)
	}
	first.Complete()

	// 竞速阶段结束：不关闭链，顺序补位继续注册同一 Claim。
	h2, err := s.Claim(a.ID(), id)
	if err != nil {
		t.Fatalf("同链重新取 Claim 失败：%v", err)
	}
	second := mustAttempt(t, a, h2)
	if err := second.Dispatch(); err != nil {
		t.Fatalf("顺序补位 Dispatch 失败（不应被自己的 Claim 抑制）：%v", err)
	}
	second.Complete()

	if got := s.DispatchedClaims(); got != 1 {
		t.Fatalf("Dispatched Identity 数 = %d，期望 1（同 Identity 只占一个 Claim）", got)
	}
	if st, _ := s.ClaimStateOf(id); st != ClaimDispatched {
		t.Fatalf("最终状态 = %v，期望 dispatched", st)
	}

	// 链最终关闭也不释放已 Dispatched 的 Claim。
	a.Close()
	if st, _ := s.ClaimStateOf(id); st != ClaimDispatched {
		t.Fatalf("链关闭后状态 = %v，期望仍为 dispatched", st)
	}
	if n := a.ActiveAttempts(); n != 0 {
		t.Fatalf("ActiveAttempts = %d，期望 0", n)
	}
}

// Closed 后禁止注册新 Attempt（T53）。
func TestClosedChainRejectsRegister(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	c, _ := s.NewChain("A", ChainA)
	h := mustClaim(t, s, c, testIdentity())
	c.Close()
	if _, err := c.RegisterAttempt(h); !errors.Is(err, ErrChainClosed) {
		t.Fatalf("Closed 后注册 err=%v，期望 ErrChainClosed", err)
	}
}

// ActiveAttempts 归零与链关闭并发：不重复释放、不提前释放、不重复取得。
func TestClaimReleaseConcurrentWithAttemptFinish(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	id := testIdentity()

	a, _ := s.NewChain("A", ChainA)
	b, _ := s.NewChain("B", ChainB)
	h := mustClaim(t, s, a, id)
	const n = 8
	atts := make([]*Attempt, 0, n)
	for i := 0; i < n; i++ {
		atts = append(atts, mustAttempt(t, a, h))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, att := range atts {
		wg.Add(1)
		go func(att *Attempt) {
			defer wg.Done()
			<-start
			att.Abort()
		}(att)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		a.Close()
	}()
	close(start)
	wg.Wait()

	// 从未 Dispatched：所有 Attempt 结束后必须恰好释放一次，另一条链可取得。
	if st, _ := s.ClaimStateOf(id); st != ClaimReleased {
		t.Fatalf("并发收尾后状态 = %v，期望 released", st)
	}
	h2, err := s.Claim(b.ID(), id)
	if err != nil {
		t.Fatalf("并发收尾后另一条链应能取得：%v", err)
	}
	if h2.ChainID() != b.ID() {
		t.Fatalf("取得者 = %s，期望 %s", h2.ChainID(), b.ID())
	}
	if got := s.DispatchedClaims(); got != 0 {
		t.Fatalf("Dispatched 数 = %d，期望 0", got)
	}
}

// 旧句柄不能把后来者预占的 Claim 打回 Released（重复释放/重复取得的最危回归面）。
func TestStaleHandleCannotReleaseNewOwner(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	id := testIdentity()

	a, err := s.NewChain("A", ChainA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.NewChain("B", ChainB)
	if err != nil {
		t.Fatal(err)
	}
	h1 := mustClaim(t, s, a, id)
	att := mustAttempt(t, a, h1)
	att.Abort()
	a.Close()
	if st, _ := s.ClaimStateOf(id); st != ClaimReleased {
		t.Fatalf("全 Prepared/Aborted 后应释放，实得 %v", st)
	}
	h2 := mustClaim(t, s, b, id)
	if st, _ := s.ClaimStateOf(id); st != ClaimReserved {
		t.Fatalf("B 预占失败: %v", st)
	}
	// A 的旧句柄此时释放必须失效，不能影响 B。
	if err := h1.Release(); !errors.Is(err, ErrClaimReleased) && !errors.Is(err, ErrClaimDispatched) {
		t.Fatalf("旧句柄释放应返回已释放类错误，实得 %v", err)
	}
	if st, _ := s.ClaimStateOf(id); st != ClaimReserved {
		t.Fatalf("旧句柄把新持有者的 Claim 打回 %v", st)
	}
	if err := h2.Dispatch(); err != nil {
		t.Fatalf("B 仍应能派发: %v", err)
	}
}
