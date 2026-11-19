package unified

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// T52：ReserveR/CommitR 竞态——并发申请只能有 RMax 个成功，
// 每个凭证只能提交一次，提交后不返还。
func TestT52ReserveRCommitRace(t *testing.T) {
	b := newRBudget(RMax)
	var ok, exhausted, doubleCommit, earlyRefund atomic.Int32
	var committed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := b.ReserveR(context.Background())
			if err != nil {
				if IsBudgetExhausted(err) {
					exhausted.Add(1)
				}
				return
			}
			ok.Add(1)
			if i%2 == 0 {
				if !res.Commit() {
					t.Errorf("首次提交失败")
				}
				if res.Commit() {
					doubleCommit.Add(1)
				}
				if res.Release() {
					earlyRefund.Add(1)
				}
				committed.Add(1)
			} else {
				if res.Commit() {
					committed.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()
	if got := ok.Load(); got != RMax {
		t.Fatalf("成功预留 = %d, want %d", got, RMax)
	}
	if exhausted.Load() != 64-RMax {
		t.Fatalf("耗尽计数 = %d, want %d", exhausted.Load(), 64-RMax)
	}
	if doubleCommit.Load() != 0 || earlyRefund.Load() != 0 {
		t.Fatalf("重复提交=%d 提前返还=%d，都必须为 0", doubleCommit.Load(), earlyRefund.Load())
	}
	if b.Used() != committed.Load() || b.Used() != RMax {
		t.Fatalf("used=%d committed=%d，want 全 16", b.Used(), committed.Load())
	}
	if b.Reserved() != 0 {
		t.Fatalf("reserved=%d，提交/返还后必须归零", b.Reserved())
	}
}

// T60：R 预留后取消必须归还，不消耗 R。
func TestT60RReleaseOnPreparedCancel(t *testing.T) {
	b := newRBudget(RMax)
	res, err := b.ReserveR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Release() {
		t.Fatal("未提交的预留必须可返还")
	}
	if b.Used() != 0 || b.Reserved() != 0 {
		t.Fatalf("取消后 used=%d reserved=%d，都必须为 0", b.Used(), b.Reserved())
	}
	// 归还后名额应可再次使用。
	if _, err := b.ReserveR(context.Background()); err != nil {
		t.Fatalf("归还后应能重新预留：%v", err)
	}
}

// T60：P 两阶段门控绝不超额，提交后不返还。
func TestT60PBudgetNeverExceeds(t *testing.T) {
	b := newPBudget(8)
	var committed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := b.ReserveP(context.Background())
			if err != nil {
				return
			}
			if res.Commit() {
				committed.Add(1)
			}
		}()
	}
	wg.Wait()
	if b.Committed() != 8 || committed.Load() != 8 {
		t.Fatalf("committed=%d/%d，want 8", b.Committed(), committed.Load())
	}
	if b.Outstanding() != 8 {
		t.Fatalf("outstanding=%d，want 8（提交后不返还）", b.Outstanding())
	}
	if b.Remaining() != 0 {
		t.Fatalf("remaining=%d，want 0", b.Remaining())
	}
	if _, err := b.ReserveP(context.Background()); !IsBudgetExhausted(err) {
		t.Fatalf("耗尽后应返回预算错误，得到 %v", err)
	}
}

// T60：P 冻结上限是 8192，整量并发也不能多派发一次。
func TestT60PMaxIsFrozen(t *testing.T) {
	b := newPBudget(PMax)
	if PMax != 8192 {
		t.Fatalf("PMax=%d，冻结值为 8192", PMax)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < PMax+64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := b.ReserveP(context.Background())
			if err != nil {
				return
			}
			if res.Commit() {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != PMax || b.Committed() != PMax {
		t.Fatalf("committed=%d ok=%d，want %d", b.Committed(), ok.Load(), PMax)
	}
}

// T60：未提交的 P 预留被取消时必须归还，避免取消吃掉预算。
func TestT60PReleaseBeforeCommit(t *testing.T) {
	b := newPBudget(2)
	ctx, cancel := context.WithCancel(context.Background())
	res, err := b.ReserveP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if res.Commit() {
		t.Fatal("取消后不应再提交")
	}
	// 提交被拒时预留已自动归还，重复归还必须无效。
	if res.Release() {
		t.Fatal("提交被拒后不得二次归还")
	}
	if b.Outstanding() != 0 {
		t.Fatalf("outstanding=%d，want 0", b.Outstanding())
	}
	if b.Committed() != 0 {
		t.Fatalf("committed=%d，取消不得消耗 P", b.Committed())
	}
}

// T60：P 耗尽响应是 503 + no-store + REQUEST_BUDGET_EXHAUSTED。
func TestT60BudgetExhaustedResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteBudgetExhausted(rec)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d，want 503", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q，want no-store", got)
	}
	if got := rec.Header().Get(HeaderError); got != CodeBudgetExhausted {
		t.Fatalf("%s=%q，want %s", HeaderError, got, CodeBudgetExhausted)
	}
}

// T57：Lease 池封装不重复计数，释放幂等。
func TestT57LeasePoolNoDoubleCount(t *testing.T) {
	var releases atomic.Int32
	p := NewLeasePool(func(ctx context.Context, id int64, handover bool) (LeaseHandle, error) {
		return fakeLease{onRelease: func() { releases.Add(1) }}, nil
	})
	id1, h1, err := p.Acquire(context.Background(), 7, false)
	if err != nil || h1 == nil {
		t.Fatalf("acquire: %v %v", id1, err)
	}
	if p.Held() != 1 {
		t.Fatalf("held=%d，want 1", p.Held())
	}
	if err := p.Release(id1); err != nil {
		t.Fatal(err)
	}
	if err := p.Release(id1); err != nil {
		t.Fatalf("重复释放必须幂等：%v", err)
	}
	if p.Held() != 0 || releases.Load() != 1 {
		t.Fatalf("held=%d releases=%d，want 0/1", p.Held(), releases.Load())
	}
}

type fakeLease struct{ onRelease func() }

func (f fakeLease) Release() error {
	if f.onRelease != nil {
		f.onRelease()
	}
	return nil
}

// RangeWork：分段上限 256、每分段最多 4 个目标、256×4=1024。
func TestRangeWorkPlanLimits(t *testing.T) {
	if MaxSegmentCount*MaxRangeTargets != RangeWorkCap {
		t.Fatalf("256×4 必须等于 RangeWorkCap=%d", RangeWorkCap)
	}
	n, chunk, err := PlanRange(10<<30, 256<<10, MaxRangeTargets)
	if err != nil {
		t.Fatal(err)
	}
	if n > MaxSegmentCount {
		t.Fatalf("段数=%d 超过 256", n)
	}
	if n*MaxRangeTargets > RangeWorkCap {
		t.Fatalf("工作量=%d 超过 1024", n*MaxRangeTargets)
	}
	if chunk <= 0 {
		t.Fatal("chunk 必须为正")
	}
	if _, _, err := PlanRange(1<<20, 256<<10, MaxRangeTargets+1); !errors.Is(err, ErrRangeWorkCap) {
		t.Fatalf("第 5 个目标必须拒绝，得到 %v", err)
	}
	rl := newRangeLedger(RangeWorkCap)
	if err := rl.Reserve(RangeWorkCap); err != nil {
		t.Fatal(err)
	}
	if err := rl.Reserve(1); !errors.Is(err, ErrRangeWorkCap) {
		t.Fatalf("超过 RangeWorkCap 必须拒绝，得到 %v", err)
	}
}

// G 公式：默认 1004，W=16 时 3404。
func TestGlobalCapacityFormula(t *testing.T) {
	if got := GlobalCapacity(200, 4, 2, 2); got != 1004 {
		t.Fatalf("默认 G=%d，want 1004", got)
	}
	if got := GlobalCapacity(200, 16, 2, 2); got != 3404 {
		t.Fatalf("W=16 G=%d，want 3404", got)
	}
	if got := GlobalCapacity(0, 0, 0, 0); got != 1004 {
		t.Fatalf("非法输入应回填默认，得到 %d", got)
	}
	// limits.enabled=false 时 N 仍必须有限：N=1 也不能变成无限。
	if got := GlobalCapacity(1, 2, 2, 2); got <= 0 {
		t.Fatalf("G 必须为正，得到 %d", got)
	}
}
