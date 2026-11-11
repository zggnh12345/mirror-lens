package scheduler

import (
	"errors"
	"sync"
	"sync/atomic"
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

// brkFails 在锁内取副本：并发测试里不能拿指针到锁外读字段。
func brkFails(s *Scheduler, kind string, id int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.brk[pkey(kind, id)]; b != nil {
		return b.fails
	}
	return 0
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
	if len(rows) != 1 || rows[0].Fails != 20 || rows[0].Rev != 20 {
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
	var live []store.BreakerState
	for _, r := range rows {
		if r.Fails > 0 {
			live = append(live, r)
		}
	}
	if mem == nil {
		if len(live) != 0 {
			t.Fatalf("内存已恢复但库里还有熔断：%+v", live)
		}
		return
	}
	if len(live) != 1 || live[0].Fails != mem.fails {
		t.Fatalf("库状态与内存不一致：mem=%+v rows=%+v", mem, live)
	}
}

// 同一纳秒内的两次写入也必须按内存顺序落库。
func TestFrozenClockKeepsWriteOrder(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: time.Minute}
	s := New(st, cfg)
	fixed := time.Now()
	s.now = func() time.Time { return fixed }
	c := candOf(id, "a")

	s.ReportOutcome(kind, c, OutcomeFail, time.Millisecond, Hint{Code: 429})
	s.ReportOutcome(kind, c, OutcomeOK, time.Millisecond, Hint{Code: 200})
	// 同一时间戳下的失败不能被墓碑上的旧 rev 挡住。
	s.ReportOutcome(kind, c, OutcomeFail, time.Millisecond, Hint{Code: 429})

	rows, err := st.LoadBreakerStates()
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	if len(rows) != 1 || rows[0].Fails != 1 || rows[0].Rev != 3 {
		t.Fatalf("应落盘第 3 版失败，实际 %+v", rows)
	}
}

// 旧库连续失败要按退避公式近似恢复，而不是只算一次 FailCooldown。
func TestLegacyFailureRunRestoresBackoff(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	for i := 0; i < 3; i++ {
		st.RecordSourceEvent(id, kind, false, 10, 429)
	}
	cfg := Config{Enabled: true, FailCooldown: time.Minute, MaxCooldown: time.Hour}
	s := New(st, cfg)
	s.Candidates(kind)
	b := brkOf(s, kind, id)
	if b == nil || b.fails != 3 {
		t.Fatalf("应恢复 3 次连续失败，实际 %+v", b)
	}
	// 3 次失败 → 4 倍基础冷却（1m << 2）。
	left := time.Until(b.until)
	if left < 2*time.Minute || left > 4*time.Minute {
		t.Fatalf("近似冷却应约 4 分钟，实际 %v", left)
	}
}

// 已恢复（墓碑）之后重启，旧事件不能把熔断翻出来。
func TestTombstoneBlocksLegacyFallback(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	const kind = "docker"
	cfg := Config{Enabled: true, FailCooldown: time.Minute}
	s := New(st, cfg)
	c := candOf(id, "a")
	s.ReportOutcome(kind, c, OutcomeFail, time.Millisecond, Hint{Code: 429})
	s.ReportOutcome(kind, c, OutcomeOK, time.Millisecond, Hint{Code: 200})

	s2 := New(st, cfg)
	s2.Candidates(kind)
	if b := brkOf(s2, kind, id); b != nil {
		t.Fatalf("墓碑存在时不该用旧事件恢复熔断，实际 %+v", b)
	}
	// 重启后的新失败仍要能写入（rev 从墓碑续上）。
	s2.ReportOutcome(kind, c, OutcomeFail, time.Millisecond, Hint{Code: 429})
	if b := brkOf(New(st, cfg), kind, id); b == nil || b.fails != 1 {
		t.Fatalf("重启后新失败应写入，实际 %+v", b)
	}
}

// 旧库里 ok=0 的 404/401/403/410 同样是中性，不能算失败连击。
func TestLegacyNeutralCodesNeverFail(t *testing.T) {
	for _, code := range []int{401, 403, 404, 410} {
		st := newStore(t)
		id := addSource(t, st, "https://a.example", true, 100, 1000)
		st.RecordSourceEvent(id, "docker", false, 5, code)
		s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
		s.Candidates("docker")
		if b := brkOf(s, "docker", id); b != nil {
			t.Fatalf("旧库 ok=0 code=%d 不该恢复熔断，实际 %+v", code, b)
		}
	}
}

// 写库失败的当前结果必须靠重试收敛，不能内存恢复、库里还熔断。
func TestPersistFailureConverges(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	cfg := Config{Enabled: true, FailCooldown: time.Minute}
	s := New(st, cfg)
	c := candOf(id, "a")
	real := s.persistFn
	var mu sync.Mutex
	fails := 2
	s.persistFn = func(r store.OutcomeRecord) error {
		mu.Lock()
		n := fails
		if fails > 0 {
			fails--
		}
		mu.Unlock()
		if n > 0 {
			return errors.New("disk full")
		}
		return real(r)
	}
	s.ReportOutcome("docker", c, OutcomeOK, time.Millisecond, Hint{Code: 200})
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _ := st.LoadBreakerStates()
		if len(rows) == 1 && rows[0].Fails == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("重试未收敛，rows=%+v", rows)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Forget 期间在飞的写入不能在清理之后把旧状态写回来。
func TestForgetDropsPendingRetry(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	cfg := Config{Enabled: true, FailCooldown: time.Minute}
	s := New(st, cfg)
	s.persistFn = func(store.OutcomeRecord) error { return errors.New("down") }
	s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 429})
	s.Forget(id)
	time.Sleep(600 * time.Millisecond)
	rows, _ := st.LoadBreakerStates()
	if len(rows) != 0 {
		t.Fatalf("Forget 之后不该再有持久化状态，实际 %+v", rows)
	}
}

// 截止时间上限：旧事件时间戳落在未来时也不能超过 now+MaxCooldown。
func TestCapUntil(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := capUntil(now.Add(24*time.Hour), now, time.Hour); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("应截到 now+MaxCooldown，实际 %v", got)
	}
	if got := capUntil(now.Add(10*time.Minute), now, time.Hour); !got.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("未超限不该改，实际 %v", got)
	}
}

// 老库兜底每个协议只跑一次，后续刷新不再扫历史事件。
func TestLegacyFallbackRunsOnce(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	s.Candidates("docker")
	// 第一次之后的旧事件不应再被应用。
	st.RecordSourceEvent(id, "docker", false, 10, 429)
	s.Invalidate()
	s.Candidates("docker")
	if b := brkOf(s, "docker", id); b != nil {
		t.Fatalf("兜底应只执行一次，实际又恢复了 %+v", b)
	}
}

// 中性事件成功落库不能把还没写盘的熔断状态从重试队列里挤掉。
func TestNeutralDoesNotDiscardPendingBreakerState(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	c := candOf(id, "a")
	real := s.persistFn
	var mu sync.Mutex
	broken := true
	s.persistFn = func(r store.OutcomeRecord) error {
		mu.Lock()
		fail := broken && r.Breaker != nil
		mu.Unlock()
		if fail {
			return errors.New("db down")
		}
		return real(r)
	}
	s.ReportOutcome("docker", c, OutcomeFail, time.Millisecond, Hint{Code: 429})
	s.ReportOutcome("docker", c, OutcomeMiss, time.Millisecond, Hint{Code: 404})
	mu.Lock()
	broken = false
	mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _ := st.LoadBreakerStates()
		if len(rows) == 1 && rows[0].Fails == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("熔断状态被中性事件挤掉，rows=%+v", rows)
		}
		time.Sleep(30 * time.Millisecond)
	}
	codes, _ := st.SourceEventCodes(id, 10)
	if len(codes) != 2 {
		t.Fatalf("两条事件都应落库，实际 %v", codes)
	}
}

// 队列里有多条时，旧的一条写成功后必须继续调度后面的，不能停在 retryBusy。
func TestFlushReschedulesNewerPending(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	c := candOf(id, "a")
	real := s.persistFn
	var mu sync.Mutex
	broken := true
	s.persistFn = func(r store.OutcomeRecord) error {
		mu.Lock()
		fail := broken
		mu.Unlock()
		if fail {
			return errors.New("db down")
		}
		return real(r)
	}
	for i := 0; i < 3; i++ {
		s.ReportOutcome("docker", c, OutcomeFail, time.Millisecond, Hint{Code: 429})
	}
	mu.Lock()
	broken = false
	mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _ := st.LoadBreakerStates()
		codes, _ := st.SourceEventCodes(id, 10)
		if len(rows) == 1 && rows[0].Fails == 3 && len(codes) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("较新的 pending 停滞，rows=%+v codes=%v", rows, codes)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// 持久化被慢写入挡住时新结果仍要按 rev 顺序落库，旧结果不能覆盖新状态。
func TestFlushKeepsOrderWhenBlocked(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	c := candOf(id, "a")
	real := s.persistFn
	var n int32
	// 前两次落库都失败：两条事件按 rev 进队列，之后由 flush 统一补写。
	s.persistFn = func(r store.OutcomeRecord) error {
		if atomic.AddInt32(&n, 1) <= 2 {
			return errors.New("db down")
		}
		return real(r)
	}
	s.ReportOutcome("docker", c, OutcomeFail, time.Millisecond, Hint{Code: 429})
	s.ReportOutcome("docker", c, OutcomeFail, time.Millisecond, Hint{Code: 500})
	s.retryMu.Lock()
	queued := len(s.pending[pkey("docker", id)])
	s.retryMu.Unlock()
	if queued != 2 {
		t.Fatalf("两条事件没有都进队列: %d", queued)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _ := st.LoadBreakerStates()
		codes, _ := st.SourceEventCodes(id, 10)
		if len(rows) == 1 && rows[0].Fails == 2 && len(codes) == 2 {
			if codes[0] != 500 || codes[1] != 429 {
				t.Fatalf("flush 顺序与 rev 相反: %v", codes)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("并写入未收敛，rows=%+v codes=%v", rows, codes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 事件队列溢出（>64 条）也不能丢掉唯一一份未落盘的熔断状态。
func TestQueueOverflowPreservesPendingBreakerState(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	c := candOf(id, "a")
	real := s.persistFn
	var mu sync.Mutex
	broken := true
	s.persistFn = func(r store.OutcomeRecord) error {
		mu.Lock()
		fail := broken
		mu.Unlock()
		if fail {
			return errors.New("db down")
		}
		return real(r)
	}
	s.ReportOutcome("docker", c, OutcomeFail, time.Millisecond, Hint{Code: 429})
	for i := 0; i < pendingCap+10; i++ {
		s.ReportOutcome("docker", c, OutcomeMiss, time.Millisecond, Hint{Code: 404})
	}
	mu.Lock()
	broken = false
	mu.Unlock()

	deadline := time.Now().Add(8 * time.Second)
	for {
		rows, _ := st.LoadBreakerStates()
		if len(rows) == 1 && rows[0].Fails == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("溢出后熔断状态丢失，rows=%+v", rows)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Forget 的延迟清库任务不能删掉 Revive 之后新生命周期的状态。
func TestForgetRetryDoesNotClearRevivedSource(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	realClear := s.clearFn
	var mu sync.Mutex
	failFirst := true
	s.clearFn = func(sid int64) error {
		mu.Lock()
		f := failFirst
		failFirst = false
		mu.Unlock()
		if f {
			return errors.New("db busy")
		}
		return realClear(sid)
	}
	s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 429})
	s.Forget(id)
	s.Revive(id)
	s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 429})

	time.Sleep(900 * time.Millisecond)
	rows, _ := st.LoadBreakerStates()
	if len(rows) != 1 || rows[0].Fails != 1 {
		t.Fatalf("旧清理任务删掉了新状态，rows=%+v", rows)
	}
}

// 失败写入与入队顺序反转时，两条事件都必须保留（按 rev 插入而非丢弃）。
func TestConcurrentEnqueuePreservesAllEvents(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	// 模拟 rev=2 先入队、rev=1 后入队。
	s.enqueue(0, store.OutcomeRecord{SourceID: id, Kind: "docker", OK: true, Code: 200, Rev: 2})
	s.enqueue(0, store.OutcomeRecord{SourceID: id, Kind: "docker", OK: true, Code: 200, Rev: 1})

	deadline := time.Now().Add(3 * time.Second)
	for {
		codes, _ := st.SourceEventCodes(id, 10)
		if len(codes) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("乱序入队丢了事件，codes=%v", codes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 在飞写入不能穿过 Revive：Revive 返回后，旧生命周期的熔断行必须不在库里。
func TestReviveWaitsForInflightWrite(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	real := s.persistFn
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	s.persistFn = func(r store.OutcomeRecord) error {
		once.Do(func() {
			entered <- struct{}{}
			<-release
		})
		return real(r)
	}
	go s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 429})
	<-entered // 此时写入已过代次校验、持 pmu 停在 persistFn 里

	revived := make(chan struct{})
	go func() {
		s.Revive(id)
		close(revived)
	}()
	select {
	case <-revived:
		close(release)
		t.Fatal("Revive 没等在飞写入结束就返回了")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case <-revived:
	case <-time.After(3 * time.Second):
		t.Fatal("Revive 未返回")
	}

	rows, err := st.LoadBreakerStates()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.SourceID == id {
			t.Fatalf("旧生命周期的熔断行被写回: %+v", r)
		}
	}
}

// DB 恢复后的直写不能插到待重试的旧事件前面：事件表顺序必须是 rev 顺序。
func TestDirectWriteDoesNotJumpQueue(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	real := s.persistFn
	var n int32
	s.persistFn = func(r store.OutcomeRecord) error {
		if atomic.AddInt32(&n, 1) == 1 {
			return errors.New("db down")
		}
		return real(r)
	}
	// 第一条落库失败，事件进入待重试队列。
	s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 429})
	// 第二条不得直写越过它：必须同样入队。
	s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 500})
	s.retryMu.Lock()
	queued := len(s.pending[pkey("docker", id)])
	s.retryMu.Unlock()
	if queued != 2 {
		t.Fatalf("直写越过了待重试队列: 队列长度=%d", queued)
	}

	ddl := time.Now().Add(3 * time.Second)
	for {
		codes, err := st.SourceEventCodes(id, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(codes) == 2 {
			// DESC：最新在前。rev 顺序正确时应为 [500, 429]。
			if codes[0] != 500 || codes[1] != 429 {
				t.Fatalf("事件顺序被直写打乱: %v", codes)
			}
			return
		}
		if time.Now().After(ddl) {
			t.Fatalf("事件未补齐: %v", codes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 候选带着旧生命周期的代次回来时，不能把结果写进复用同一 id 的新源。
func TestReportRejectsStaleLifecycleCandidate(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	list := s.CandidatesN("docker", 10)
	if len(list) == 0 {
		t.Fatal("没有候选")
	}
	c := list[0]
	if c.Gen == 0 {
		t.Fatal("候选没有携带代次")
	}
	s.Revive(id) // 新生命周期：代次加一
	s.ReportOutcome("docker", c, OutcomeFail, time.Millisecond, Hint{Code: 500})

	codes, err := st.SourceEventCodes(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 0 {
		t.Fatalf("旧候选的结果被写进新生命周期: %v", codes)
	}
	rows, _ := st.LoadBreakerStates()
	for _, r := range rows {
		if r.SourceID == id {
			t.Fatalf("旧候选写入了熔断状态: %+v", r)
		}
	}
	if got := brkFails(s, "docker", id); got != 0 {
		t.Fatalf("内存里出现旧候选的失败计数: %d", got)
	}
}

// rev 分配与落库串行：先拿到 rev 的上报必须先落库，事件表 id 顺序即 rev 顺序。
func TestConcurrentReportsKeepRevOrder(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var first int32
	// 用 CompareAndSwap 而不是 sync.Once：Once 会让并发的第二次调用一起
	// 阻塞，等于自己把两个上报串起来了，测不出 key 锁的作用。
	s.persistBarrier = func() {
		if atomic.CompareAndSwapInt32(&first, 0, 1) {
			entered <- struct{}{}
			<-release
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 429})
	}()
	<-entered // 第一条已取 rev 且卡在落库前
	// 确定性断言：此刻 key 锁必须仍被持有，说明 rev 分配、状态更新和落库
	// 在同一段临界区里，而不是只包住 persist()。
	if lk := s.keyLock(pkey("docker", id)); lk.TryLock() {
		lk.Unlock()
		t.Fatal("rev 已分配但 key 锁没覆盖到落库前")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 500})
	}()
	// 给第二条一个进入 ReportOutcome 的机会；它只能卡在 key 锁上。
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for {
		codes, err := st.SourceEventCodes(id, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(codes) == 2 {
			if codes[0] != 500 || codes[1] != 429 {
				t.Fatalf("事件顺序与 rev 相反: %v", codes)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("事件未补齐: %v", codes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Revive 清库失败时，新生命周期的状态仍必须能覆盖旧行（rev 继续递增）。
func TestReviveClearFailureStillAllowsNewState(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	for i := 0; i < 3; i++ {
		s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 500})
	}
	waitRows := func(want int) store.BreakerState {
		deadline := time.Now().Add(3 * time.Second)
		for {
			rows, err := st.LoadBreakerStates()
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.SourceID == id && r.Kind == "docker" {
					if r.Fails == want {
						return r
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("数据库里没有 fails=%d 的行: %+v", want, rows)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	old := waitRows(3)

	s.clearFn = func(int64) error { return errors.New("db locked") }
	s.Revive(id)
	s.ReportOutcome("docker", candOf(id, "a"), OutcomeFail, time.Millisecond, Hint{Code: 429})
	now := waitRows(1)
	if now.Rev <= old.Rev {
		t.Fatalf("新状态没有覆盖旧行: old rev=%d new rev=%d", old.Rev, now.Rev)
	}

	// 重启后按库恢复，仍是新生命周期的状态。
	s2 := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	if got := brkFails(s2, "docker", id); got != 1 {
		t.Fatalf("重启恢复了错误的失败计数: %d", got)
	}
}
