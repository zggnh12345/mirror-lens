package unified

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestB02VoteConfirmsOnlyDistinctRegistryAndSource(t *testing.T) {
	v := NewManifestVote(30 * time.Millisecond)
	st, ok := v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 1, Digest: "sha256:aa", Verified: true, Complete: true})
	if !ok || st != VoteCollecting {
		t.Fatalf("首票 accepted=%v state=%v", ok, st)
	}
	// 同一 Source 通过别名再投一票：必须忽略。
	st, ok = v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 1, Digest: "sha256:aa", Verified: true, Complete: true})
	if ok || st != VoteCollecting {
		t.Fatalf("同源别名不应构成第二票，accepted=%v state=%v", ok, st)
	}
	// 同 Registry 不同 Source：不是独立投票方。
	st, ok = v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 2, Digest: "sha256:aa", Verified: true, Complete: true})
	if ok || st != VoteCollecting {
		t.Fatalf("同 Registry 不同 Source 不应确认，accepted=%v state=%v", ok, st)
	}
	// 不同逻辑 Registry + 不同 source_id + 同 Digest：确认。
	st, ok = v.Offer(VoteCandidate{Registry: "registry.k8s.io", SourceID: 3, Digest: "sha256:aa", Verified: true, Complete: true})
	if !ok || st != VoteConfirmed {
		t.Fatalf("独立两票应确认，accepted=%v state=%v", ok, st)
	}
	if v.Agreed() != "sha256:aa" {
		t.Fatalf("agreed=%q", v.Agreed())
	}
	if got := v.CloseWindow(); got != VoteConfirmed {
		t.Fatalf("关窗后结论被改写为 %v", got)
	}
}

func TestB02VoteRejectsUnverifiedOrIncomplete(t *testing.T) {
	v := NewManifestVote(20 * time.Millisecond)
	if _, ok := v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 1, Digest: "sha256:aa", Verified: false, Complete: true}); ok {
		t.Fatal("未校验正文的票必须拒绝")
	}
	if _, ok := v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 1, Digest: "sha256:aa", Verified: true, Complete: false}); ok {
		t.Fatal("未完整读取正文的票必须拒绝")
	}
	if v.Votes() != 0 {
		t.Fatalf("votes=%d，want 0", v.Votes())
	}
	// 只有响应头 Digest（无正文校验）不能开窗。
	if v.Votes() != 0 || v.State() != VoteCollecting {
		t.Fatalf("零有效票不应改变状态：%v", v.State())
	}
}

func TestB02VoteMismatchDoesNotConfirm(t *testing.T) {
	v := NewManifestVote(20 * time.Millisecond)
	v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 1, Digest: "sha256:aa", Verified: true, Complete: true})
	st, ok := v.Offer(VoteCandidate{Registry: "registry.k8s.io", SourceID: 2, Digest: "sha256:bb", Verified: true, Complete: true})
	if !ok || st != VoteMismatch {
		t.Fatalf("不一致票应进入 Mismatch，accepted=%v state=%v", ok, st)
	}
	if v.Agreed() != "" {
		t.Fatalf("不一致时不得给出 agreed=%q", v.Agreed())
	}
	if got := v.CloseWindow(); got != VoteMismatch {
		t.Fatalf("关窗后必须保持 Mismatch，得到 %v", got)
	}
}

func TestB02VoteSingleCandidateNeedsAuthorityCompare(t *testing.T) {
	v := NewManifestVote(20 * time.Millisecond)
	v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 1, Digest: "sha256:aa", Verified: true, Complete: true})
	if got := v.CloseWindow(); got != VoteAuthorityCompare {
		t.Fatalf("单候选关窗应转 Authority 比对，得到 %v", got)
	}
}

func TestB02VoteWindowExpiryIsAuthoritative(t *testing.T) {
	v := NewManifestVote(30 * time.Millisecond)
	v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 1, Digest: "sha256:aa", Verified: true, Complete: true})
	deadline := time.Now().Add(2 * time.Second)
	for v.State() == VoteCollecting && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if v.State() != VoteAuthorityCompare {
		t.Fatalf("窗口超时后 state=%v，want authority_compare", v.State())
	}
	// 窗口关闭后到达的票必须被丢弃，不改结论。
	st, ok := v.Offer(VoteCandidate{Registry: "registry.k8s.io", SourceID: 9, Digest: "sha256:zz", Verified: true, Complete: true})
	if ok || st != VoteAuthorityCompare {
		t.Fatalf("窗后票应丢弃，accepted=%v state=%v", ok, st)
	}
	if v.Agreed() != "sha256:aa" {
		t.Fatalf("窗后结论被改写：%q", v.Agreed())
	}
}

func TestB02VoteNoVotesClosesEmpty(t *testing.T) {
	v := NewManifestVote(10 * time.Millisecond)
	if got := v.CloseWindow(); got != VoteWindowClosed {
		t.Fatalf("零候选关窗应 closed，得到 %v", got)
	}
}

// T63：Authority 只允许一次 Attempt，软比对与兜底复用同一结果。
func TestT63AuthoritySingleAttempt(t *testing.T) {
	a := NewAuthority()
	var winners int
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if a.Acquire() {
				winners++
				a.Finish(AuthorityResult{Registry: "ghcr.io", SourceID: 1, Digest: "sha256:aa", Verified: true}, nil)
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("执行权获取者=%d，want 1", winners)
	}
	if a.Attempts() != 1 {
		t.Fatalf("attempts=%d，want 1", a.Attempts())
	}
	ok, err := a.Compare(context.Background(), "sha256:aa")
	if err != nil || !ok {
		t.Fatalf("软比对应复用同一结果：%v %v", ok, err)
	}
	if ok, _ := a.Compare(context.Background(), "sha256:bb"); ok {
		t.Fatal("不一致候选不得确认")
	}
}

// B06：全已派发 Miss → 404。
func TestB06AllMissIs404(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	l := s.Ledger()
	l.Record(AttemptResult{Outcome: OutcomeMiss, Code: 404})
	l.Record(AttemptResult{Outcome: OutcomeMiss, Code: 410})
	d := l.Decide()
	if d.Outcome != OutcomeMiss || d.Code != http.StatusNotFound {
		t.Fatalf("decision=%+v，want 404 miss", d)
	}
	if d.NegativeCache {
		t.Fatal("失败请求不得写负缓存")
	}
}

// B06：部分 Fail 不能折叠成 404。
func TestB06PartialFailNotMiss(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	l := s.Ledger()
	l.Record(AttemptResult{Outcome: OutcomeMiss, Code: 404})
	l.Record(AttemptResult{Outcome: OutcomeFail, Code: 502, Err: errors.New("boom")})
	d := l.Decide()
	if d.Code == http.StatusNotFound {
		t.Fatalf("混合失败被折叠成 404：%+v", d)
	}
	if d.Outcome != OutcomeFail || d.Code != 502 {
		t.Fatalf("decision=%+v，want fail 502", d)
	}
}

// B06：Auth 阻止全 Miss。
func TestB06AuthBlocksAllMiss(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	l := s.Ledger()
	l.Record(AttemptResult{Outcome: OutcomeAuth, Code: 401, Err: errors.New("denied")})
	d := l.Decide()
	if d.Outcome != OutcomeAuth || d.Code == http.StatusNotFound {
		t.Fatalf("decision=%+v，Auth 必须阻止 404", d)
	}
}

// B06：Cancelled 阻止全 Miss。
func TestB06CancelledBlocksAllMiss(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	l := s.Ledger()
	l.Record(AttemptResult{Outcome: OutcomeCancelled})
	d := l.Decide()
	if d.Outcome != OutcomeCancelled || d.Code == http.StatusNotFound {
		t.Fatalf("decision=%+v，取消必须阻止 404", d)
	}
}

// B06：未完成暂不结算。
func TestB06PendingNotSettled(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	l := s.Ledger()
	l.Record(AttemptResult{Outcome: OutcomePending})
	d := l.Decide()
	if d.Settled || d.Outcome != OutcomePending {
		t.Fatalf("decision=%+v，未完成不应结算", d)
	}
}

// B06：P 耗尽独立最高优先级通道。
func TestB06BudgetOutranksEverything(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	l := s.Ledger()
	l.SetPolicyError(errors.New("policy"))
	l.Record(AttemptResult{Outcome: OutcomeAuth, Code: 401})
	l.Record(AttemptResult{Outcome: OutcomeBudget})
	d := l.Decide()
	if d.Outcome != OutcomeBudget || d.Code != http.StatusServiceUnavailable {
		t.Fatalf("decision=%+v，预算耗尽必须是独立通道", d)
	}
	if !d.BudgetExhausted() {
		t.Fatal("BudgetExhausted() 应为真")
	}
}

// B06：policyErr 优先于上游状态码。
func TestB06PolicyErrorPriority(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	l := s.Ledger()
	l.Record(AttemptResult{Outcome: OutcomeMiss, Code: 404})
	l.SetPolicyError(errors.New("missing policy"))
	d := l.Decide()
	if d.Code == http.StatusNotFound || d.Err == nil {
		t.Fatalf("policyErr 被上游 404 覆盖：%+v", d)
	}
}

// B06：成功优先，即使还有失败行。
func TestB06SuccessWins(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	l := s.Ledger()
	l.Record(AttemptResult{Outcome: OutcomeFail, Code: 500})
	l.Record(AttemptResult{Outcome: OutcomeOK})
	if d := l.Decide(); d.Outcome != OutcomeOK {
		t.Fatalf("decision=%+v，成功必须优先", d)
	}
}

// 无结果也要有确定结论，且不写负缓存。
func TestB06NoAttempts(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	d := s.Ledger().Decide()
	if d.Settled != true || d.Err == nil || d.Code == 0 {
		t.Fatalf("空账本应给确定失败：%+v", d)
	}
	if d.NegativeCache {
		t.Fatal("空账本不得写负缓存")
	}
}

// 所有结果都带 Chain 归属，仲裁可读。
func TestLedgerKeepsChainAttribution(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()
	s.Ledger().Record(AttemptResult{ChainID: "fb#1", ChainKind: ChainFallback, Outcome: OutcomeFail})
	rows := s.Ledger().Snapshot()
	if len(rows) != 1 || rows[0].ChainKind != ChainFallback || rows[0].ChainID != "fb#1" {
		t.Fatalf("链归属丢失：%+v", rows)
	}
}
