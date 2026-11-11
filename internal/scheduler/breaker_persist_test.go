package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// candOf 造一个最小候选，避免测试依赖 SourceCandidates 的筛选条件。
func candOf(id int64, name string) Candidate {
	return Candidate{ID: id, Name: name}
}

func brkOf(s *Scheduler, kind string, id int64) *breaker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.brk[pkey(kind, id)]
}

// 429 之后再来一条 404，重启后仍然熔断：中性事件不能盖掉最后一次有效失败。
func TestBreakerSurvivesMissAcrossRestart(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: 5 * time.Minute, MaxCooldown: time.Hour}

	s := New(st, cfg)
	c := candOf(id, "a")
	s.ReportOutcome(kind, c, OutcomeFail, 20*time.Millisecond, Hint{Code: 429})
	s.ReportOutcome(kind, c, OutcomeMiss, 5*time.Millisecond, Hint{Code: 404})

	s2 := New(st, cfg)
	b := brkOf(s2, kind, id)
	if b == nil || b.fails != 1 {
		t.Fatalf("重启后应保留 1 次失败熔断，实际 %+v", b)
	}
	if !b.until.After(time.Now()) {
		t.Fatalf("冷却截止时间应在将来，实际 %v", b.until)
	}
}

// 401 同样不能清除熔断。
func TestBreakerSurvivesAuthAcrossRestart(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: 5 * time.Minute, MaxCooldown: time.Hour}

	s := New(st, cfg)
	s.ReportOutcome(kind, candOf(id, "a"), OutcomeFail, 20*time.Millisecond, Hint{Code: 429})
	s.ReportOutcome(kind, candOf(id, "a"), OutcomeAuth, 5*time.Millisecond, Hint{Code: 401})

	if b := brkOf(New(st, cfg), kind, id); b == nil || b.fails != 1 {
		t.Fatalf("401 后重启应仍是 1 次失败，实际 %+v", b)
	}
}

// 200 之后重启不再熔断。
func TestBreakerClearedByOKAcrossRestart(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: 5 * time.Minute, MaxCooldown: time.Hour}

	s := New(st, cfg)
	s.ReportOutcome(kind, candOf(id, "a"), OutcomeFail, 20*time.Millisecond, Hint{Code: 429})
	s.ReportOutcome(kind, candOf(id, "a"), OutcomeOK, 10*time.Millisecond, Hint{Code: 200})

	if b := brkOf(New(st, cfg), kind, id); b != nil {
		t.Fatalf("成功后重启不该有熔断，实际 %+v", b)
	}
}

// 指数退避的 fails 和绝对截止时间都要原样恢复，不能按「现在 + 一个周期」重算。
func TestBackoffDeadlinePreservedAcrossRestart(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: time.Minute, MaxCooldown: time.Hour}

	s := New(st, cfg)
	c := candOf(id, "a")
	var want time.Time
	for i := 0; i < 5; i++ {
		s.ReportOutcome(kind, c, OutcomeFail, time.Second, Hint{Code: 429})
		want = brkOf(s, kind, id).until
	}
	if got := brkOf(s, kind, id); got.fails != 5 {
		t.Fatalf("连续失败应累计 5 次，实际 %d", got.fails)
	}

	b := brkOf(New(st, cfg), kind, id)
	if b == nil {
		t.Fatal("重启后熔断丢了")
	}
	if b.fails != 5 {
		t.Fatalf("fails 应恢复为 5，实际 %d", b.fails)
	}
	if d := b.until.Sub(want); d < -time.Second || d > time.Second {
		t.Fatalf("截止时间应保持原值，差 %v（want %v got %v）", d, want, b.until)
	}
}

// 剩余 10 秒的冷却重启后还是 10 秒，不能被延长成一整个基础周期。
func TestRemainingCooldownNotExtendedAcrossRestart(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: time.Second, MaxCooldown: time.Hour}

	s := New(st, cfg)
	s.ReportOutcome(kind, candOf(id, "a"), OutcomeFail, time.Second, Hint{Code: 429, RetryAfter: 10 * time.Second})

	b := brkOf(New(st, cfg), kind, id)
	if b == nil {
		t.Fatal("重启后熔断丢了")
	}
	left := time.Until(b.until)
	if left < 5*time.Second || left > 11*time.Second {
		t.Fatalf("剩余冷却应约 10s，实际 %v", left)
	}
}

// 冷却已过期：恢复成半开，允许探一次，但不重新开始完整冷却。
func TestExpiredCooldownRestoresHalfOpen(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: time.Minute, MaxCooldown: time.Hour}
	past := time.Now().Add(-2 * time.Minute)
	if err := st.RecordOutcome(store.OutcomeRecord{
		SourceID: id, Kind: kind, OK: false, Code: 429, Breaker: &store.BreakerState{
			SourceID: id, Kind: kind, Fails: 2, Until: past.Unix(), UpdatedAt: time.Now().UnixNano(),
		},
	}); err != nil {
		t.Fatalf("写状态: %v", err)
	}

	s := New(st, cfg)
	b := brkOf(s, kind, id)
	if b == nil || b.fails != 2 {
		t.Fatalf("应恢复 fails=2，实际 %+v", b)
	}
	if b.until.After(time.Now()) {
		t.Fatalf("过期冷却不该被重新拉长，实际 %v", b.until)
	}
	var found bool
	for _, c := range s.Candidates(kind) {
		if c.ID == id {
			found = true
			if !c.Probe {
				t.Fatal("过期冷却应作为半开探测候选")
			}
		}
	}
	if !found {
		t.Fatal("过期冷却的源应仍在候选里（半开探测）")
	}
}

// 升级前写的历史事件没有 neutral 列：404/401 不能被恢复成失败。
func TestLegacyNeutralEventsDoNotRestoreBreaker(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	st.RecordSourceEvent(id, kind, true, 5, 404)

	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	s.Candidates(kind) // 触发老库兜底
	if b := brkOf(s, kind, id); b != nil {
		t.Fatalf("历史 404 不该恢复熔断，实际 %+v", b)
	}
}

// 历史 429 仍要兜底恢复，截止时间按事件时间算。
func TestLegacyFailureEventRestoresBreaker(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	st.RecordSourceEvent(id, kind, false, 20, 429)

	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	s.Candidates(kind)
	if b := brkOf(s, kind, id); b == nil || b.fails != 1 {
		t.Fatalf("历史 429 应恢复熔断，实际 %+v", b)
	}
}

// 中性事件不写状态行：429 的状态不能被 404 改写。
func TestNeutralDoesNotTouchPersistedState(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: 5 * time.Minute}
	s := New(st, cfg)
	s.ReportOutcome(kind, candOf(id, "a"), OutcomeFail, time.Second, Hint{Code: 429})
	before := brkOf(s, kind, id).until
	s.ReportOutcome(kind, candOf(id, "a"), OutcomeMiss, time.Millisecond, Hint{Code: 404})

	rows, err := st.LoadBreakerStates()
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	if len(rows) != 1 || rows[0].Fails != 1 || rows[0].Until != before.Unix() {
		t.Fatalf("中性事件不应改写状态，实际 %+v（before %v）", rows, before)
	}
}

// 删除源后重启不能复活熔断。
func TestDeletedSourceDoesNotReviveBreaker(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	s.ReportOutcome(kind, candOf(id, "a"), OutcomeFail, time.Second, Hint{Code: 429})
	if err := st.DeleteSource(id); err != nil {
		t.Fatalf("删源: %v", err)
	}
	if b := brkOf(New(st, Config{Enabled: true}), kind, id); b != nil {
		t.Fatalf("已删除的源不该恢复熔断，实际 %+v", b)
	}
}

// 并发失败上报：内存的 fails 和落盘的 fails 最终一致。
func TestConcurrentReportsPersistConsistently(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	s := New(st, Config{Enabled: true, FailCooldown: time.Second, MaxCooldown: time.Hour})
	c := candOf(id, "a")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.ReportOutcome(kind, c, OutcomeFail, time.Millisecond, Hint{Code: 429})
		}()
	}
	wg.Wait()

	mem := brkOf(s, kind, id)
	if mem == nil || mem.fails != 20 {
		t.Fatalf("内存应累计 20 次失败，实际 %+v", mem)
	}
	rows, err := st.LoadBreakerStates()
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	if len(rows) != 1 || rows[0].Fails != 20 {
		t.Fatalf("落盘应为 20 次失败，实际 %+v", rows)
	}
}

// 并发成功与失败混报：最终状态必须与内存一致，不能出现「内存健康、库里还在冷却」。
func TestConcurrentOKAndFailConverge(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	c := candOf(id, "a")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.ReportOutcome(kind, c, OutcomeFail, time.Millisecond, Hint{Code: 429})
		}()
		go func() {
			defer wg.Done()
			s.ReportOutcome(kind, c, OutcomeOK, time.Millisecond, Hint{Code: 200})
		}()
	}
	wg.Wait()

	mem := brkOf(s, kind, id)
	rows, err := st.LoadBreakerStates()
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	if mem == nil {
		if len(rows) != 0 {
			t.Fatalf("内存已恢复但库里还有熔断：%+v", rows)
		}
		return
	}
	if len(rows) != 1 || rows[0].Fails != mem.fails {
		t.Fatalf("库状态与内存不一致：mem=%+v rows=%+v", mem, rows)
	}
}
