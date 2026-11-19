package unified

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestScope(t *testing.T) *Scope {
	t.Helper()
	s, err := NewScope(context.Background(), ScopeConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testIdentity() Identity {
	return Identity{Registry: "ghcr.io", Repository: "acme/app", Manifest: ManifestIdentity{Ref: "sha256:abc"}}
}

// T50：同 Identity 跨链只允许一条链获得 Reserved→Dispatched，零重复。
func TestT50ClaimCrossChainExclusive(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	c1, err := s.NewChain("A", ChainA)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.NewChain("B", ChainB)
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity()

	var winners atomic.Int32
	var conflicts atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, c := range []*Chain{c1, c2} {
		wg.Add(1)
		go func(c *Chain) {
			defer wg.Done()
			<-start
			h, err := s.Claim(c.ID(), id)
			if err != nil {
				if errors.Is(err, ErrClaimConflict) {
					conflicts.Add(1)
				}
				return
			}
			a, err := c.RegisterAttempt(h)
			if err != nil {
				t.Error(err)
				return
			}
			defer a.Abort()
			if err := a.Dispatch(); err != nil {
				t.Error(err)
				return
			}
			winners.Add(1)
		}(c)
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("成功派发链数=%d，want 1（Claim 独占）", winners.Load())
	}
	if conflicts.Load() != 1 {
		t.Fatalf("冲突链数=%d，want 1", conflicts.Load())
	}
	if got := s.claims.dispatchedCount(); got != 1 {
		t.Fatalf("Dispatched Identity=%d，want 1（零重复）", got)
	}
}

// T51：同链多 Attempt 共享同一 Claim，可并发派发，不重复登记。
func TestT51SameChainSharesClaim(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	c, err := s.NewChain("A", ChainA)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Claim(c.ID(), testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	a1, err := c.RegisterAttempt(h)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := c.RegisterAttempt(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := a1.Dispatch(); err != nil {
		t.Fatalf("a1 dispatch: %v", err)
	}
	if err := a2.Dispatch(); err != nil {
		t.Fatalf("a2 应与 a1 共享 Claim，得到 %v", err)
	}
	if got := s.claims.dispatchedCount(); got != 1 {
		t.Fatalf("Dispatched Identity=%d，want 1", got)
	}
	if err := a1.Complete(); err != nil {
		t.Fatal(err)
	}
	if err := a2.Complete(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if s.r.Used() != 2 {
		t.Fatalf("R used=%d，want 2（两个 Attempt 各一次）", s.r.Used())
	}
}

// T50/T51：已 Dispatched 的 Claim 不得释放；从未 Dispatched 的 Claim 释放后
// 换链可重新预占，旧 handle 不能继续提交（F19 冻结口径）。
func TestClaimEpochAfterRelease(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()

	main, err := s.NewChain("main", ChainMain)
	if err != nil {
		t.Fatal(err)
	}
	h1, err := s.Claim(main.ID(), testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if err := h1.Dispatch(); err != nil {
		t.Fatal(err)
	}
	if err := h1.Release(); !errors.Is(err, ErrClaimDispatched) {
		t.Fatalf("已 Dispatched 的 Claim 必须拒绝释放，得到 %v", err)
	}
	if _, err := s.Claim("fb", testIdentity()); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("已 Dispatched 的 Claim 被另一链取得：%v", err)
	}

	other := Identity{Registry: "ghcr.io", Repository: "acme/other", Manifest: ManifestIdentity{Ref: "v2"}}
	h2, err := s.Claim(main.ID(), other)
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.Release(); err != nil {
		t.Fatalf("从未 Dispatched 的 Claim 应可释放：%v", err)
	}
	h3, err := s.Claim("fb", other)
	if err != nil {
		t.Fatalf("释放后应可由另一链重新预占：%v", err)
	}
	if err := h2.Dispatch(); !errors.Is(err, ErrClaimReleased) {
		t.Fatalf("旧句柄提交必须失败，得到 %v", err)
	}
	if err := h3.Dispatch(); err != nil {
		t.Fatalf("新句柄提交失败：%v", err)
	}
	if got := s.claims.dispatchedCount(); got != 2 {
		t.Fatalf("Dispatched=%d，want 2", got)
	}
}

// T53：Chain Closing 后 RegisterAttempt 必须被拒。
func TestT53ChainClosingRejectsRegister(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	c, err := s.NewChain("main", ChainMain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterAttempt(nil); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if c.State() != ChainClosed {
		t.Fatalf("state=%v，want closed", c.State())
	}
	if _, err := c.RegisterAttempt(nil); !errors.Is(err, ErrChainClosed) {
		t.Fatalf("Closing 后注册必须拒绝，得到 %v", err)
	}
	// Scope 关闭后也不能再开新链。
	s.Close()
	if _, err := s.NewChain("late", ChainMain); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("Scope 关闭后 NewChain 必须拒绝，得到 %v", err)
	}
}

// T56：Body 未关闭前 Attempt 不进入 Completed，CloseBody 幂等。
func TestT56BodyCloseIdempotent(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	c, _ := s.NewChain("main", ChainMain)
	a, err := c.RegisterAttempt(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Dispatch(); err != nil {
		t.Fatal(err)
	}
	var closed atomic.Int32
	if !a.CloseBody(func() error { closed.Add(1); return nil }) {
		t.Fatal("首次关闭应生效")
	}
	if a.CloseBody(func() error { closed.Add(1); return nil }) {
		t.Fatal("重复关闭必须无效")
	}
	if closed.Load() != 1 {
		t.Fatalf("关闭次数=%d，want 1", closed.Load())
	}
	if !a.BodyClosed() {
		t.Fatal("BodyClosed 应为真")
	}
	if err := a.Complete(); err != nil {
		t.Fatal(err)
	}
	if a.State() != AttemptCompleted {
		t.Fatalf("state=%v，want completed", a.State())
	}
}

// Prepared 取消：不消耗 R、不形成 Dispatched。
func TestAttemptPreparedCancelKeepsR(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	c, _ := s.NewChain("main", ChainMain)
	h, err := s.Claim(c.ID(), testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.RegisterAttempt(h)
	if err != nil {
		t.Fatal(err)
	}
	a.Abort()
	if s.r.Used() != 0 {
		t.Fatalf("Prepared 取消消耗了 R=%d", s.r.Used())
	}
	if a.State() != AttemptAborted {
		t.Fatalf("state=%v，want aborted", a.State())
	}
	// Prepared 取消不越过提交边界；Claim 释放属于链级决策：链仍活跃时
	// 不释放（F19），链关闭且无在途 Attempt 后才让给另一条链。
	if _, err := s.Claim("fb", testIdentity()); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("链仍活跃时不应让出 Claim，err=%v", err)
	}
	c.Close()
	if _, err := s.Claim("fb", testIdentity()); err != nil {
		t.Fatalf("链关闭后 Prepared 取消应让出 Claim：%v", err)
	}
}

// R 上限：第 17 个 Attempt 必须失败，而不是静默超额。
func TestRMaxBlocksSeventeenthAttempt(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	c, _ := s.NewChain("main", ChainMain)
	for i := 0; i < RMax; i++ {
		if _, err := c.RegisterAttempt(nil); err != nil {
			t.Fatalf("第 %d 个: %v", i+1, err)
		}
	}
	if _, err := c.RegisterAttempt(nil); !IsBudgetExhausted(err) {
		t.Fatalf("第 %d 个必须被 R 上限拒绝，得到 %v", RMax+1, err)
	}
}

// 父 Context 取消传播到 Scope，并阻止新的预算申请。
func TestScopeContextCancelPropagates(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	s, err := NewScope(parent, ScopeConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cancel()
	select {
	case <-s.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("父 Context 取消未传播")
	}
	if _, err := s.ReserveP(); err == nil {
		t.Fatal("取消后不应再能预留 P")
	}
}

// 每个 Scope 只执行一轮 Preflight，tryParallel 与 midFill 共享结果。
func TestScopePreflightSharedOnce(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	var runs atomic.Int32
	fn := func(ctx context.Context) ([]PreflightResult, int64, error) {
		runs.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []PreflightResult{{Target: "a", OK: true, Total: 42}}, 42, nil
	}
	var wg sync.WaitGroup
	results := make([][]PreflightResult, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _, _ = s.Preflight(context.Background(), fn)
		}(i)
	}
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("Preflight 执行 %d 次，want 1（一轮共享）", runs.Load())
	}
	if s.PreflightRuns() != 1 {
		t.Fatalf("PreflightRuns=%d，want 1", s.PreflightRuns())
	}
	for i, r := range results {
		if len(r) != 1 || r[0].Total != 42 {
			t.Fatalf("第 %d 个调用方未拿到共享结果: %+v", i, r)
		}
	}
}

// Scope 快照：Generation 与候选在创建时固定，之后外部变化不影响本 Scope。
func TestScopeSnapshotFrozen(t *testing.T) {
	gen := map[string]uint64{"ghcr.io": 3}
	cands := map[string][]CandidateRef{"ghcr.io": {{SourceID: 1, BaseURL: "https://a"}}}
	s, err := NewScope(context.Background(), ScopeConfig{Generation: gen, Candidates: cands}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gen["ghcr.io"] = 99
	cands["ghcr.io"] = append(cands["ghcr.io"], CandidateRef{SourceID: 2})
	if got := s.Generation("ghcr.io"); got != 3 {
		t.Fatalf("generation=%d，want 3（快照固定）", got)
	}
	if got := len(s.Candidates("ghcr.io")); got != 1 {
		t.Fatalf("候选数=%d，want 1（快照固定）", got)
	}
}

// G 池：容量用尽时非阻塞申请失败，释放后可再次申请。
func TestPoolCapacity(t *testing.T) {
	p := NewPool(2)
	if !p.TryAcquire() || !p.TryAcquire() {
		t.Fatal("前两次申请应成功")
	}
	if p.TryAcquire() {
		t.Fatal("容量已满时 TryAcquire 必须失败")
	}
	p.Release()
	if !p.TryAcquire() {
		t.Fatal("释放后应能再次申请")
	}
	if p.InUse() != 2 {
		t.Fatalf("inuse=%d，want 2", p.InUse())
	}
	p.Release()
	p.Release()
	if p.InUse() != 0 {
		t.Fatalf("inuse=%d，want 0", p.InUse())
	}
}

// NewScope 在池满时必须失败而不是无限等待。
func TestNewScopeRespectsPool(t *testing.T) {
	p := NewPool(1)
	s1, err := NewScope(context.Background(), ScopeConfig{}, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := NewScope(ctx, ScopeConfig{}, p); err == nil {
		t.Fatal("池满时 NewScope 必须失败")
	}
	s1.Close()
	if p.InUse() != 0 {
		t.Fatalf("关闭后 inuse=%d，want 0", p.InUse())
	}
}
