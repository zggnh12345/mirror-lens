package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// 404/410 是中性结果：事件照留，但不建粘性、不清熔断、不进冷却。
func TestMissIsNeutral(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1000)
	b := addSource(t, st, "https://b.example", true, 120, 900)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	ca := Candidate{ID: a, Name: "a", BaseURL: "https://a.example"}
	cb := Candidate{ID: b, Name: "b", BaseURL: "https://b.example"}
	const kind = "registry:quay.io"

	s.ReportOutcome(kind, ca, OutcomeOK, 10*time.Millisecond, Hint{Code: 200})
	if got := s.sticky[kind].cand.ID; got != a {
		t.Fatalf("成功应建立粘性 a，实际 %d", got)
	}

	// 换一台源命中一次，让粘性指向 b；随后对 b 报 404。
	s.ReportOutcome(kind, cb, OutcomeOK, 10*time.Millisecond, Hint{Code: 200})
	if got := s.sticky[kind].cand.ID; got != b {
		t.Fatalf("粘性应切到 b，实际 %d", got)
	}
	s.ReportOutcome(kind, cb, OutcomeMiss, 5*time.Millisecond, Hint{Code: 404})

	if got := s.sticky[kind].cand.ID; got != b {
		t.Fatalf("404 不应刷新或清除粘性（保持 b），实际 %d", got)
	}
	if _, cooled := s.brk[pkey(kind, b)]; cooled {
		t.Fatal("404 是缺文件，不该进冷却")
	}

	// 事件必须保留：一次 200 + 一次 404，都算「源没坏」。
	okN, failN, _, err := st.SourceEvents(b, 10)
	if err != nil {
		t.Fatalf("读事件: %v", err)
	}
	if okN != 2 || failN != 0 {
		t.Fatalf("事件应保留为 ok=2/fail=0，实际 ok=%d fail=%d", okN, failN)
	}
}

// 401/403 同样是中性结果：没有成功过就绝不建立粘性，也不冷却。
func TestAuthIsNeutral(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	ca := Candidate{ID: a, Name: "a"}
	const kind = "registry:quay.io"

	s.ReportOutcome(kind, ca, OutcomeAuth, 8*time.Millisecond, Hint{Code: 401})

	if got, ok := s.sticky[kind]; ok {
		t.Fatalf("认证失败不该建立粘性，实际粘到 %d", got.cand.ID)
	}
	if _, cooled := s.brk[pkey(kind, a)]; cooled {
		t.Fatal("401/403 不该冷却源")
	}
	okN, failN, _, err := st.SourceEvents(a, 10)
	if err != nil {
		t.Fatalf("读事件: %v", err)
	}
	if okN != 1 || failN != 0 {
		t.Fatalf("认证失败事件应保留且不算故障，实际 ok=%d fail=%d", okN, failN)
	}
}

// 中性结果既不建立也不解除熔断：冷却中的源不会因为一次 404 被提前放出来。
func TestNeutralKeepsBreaker(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	ca := Candidate{ID: a, Name: "a"}
	const kind = "docker"

	s.ReportOutcome(kind, ca, OutcomeFail, 20*time.Millisecond, Hint{Code: 503})
	b := s.brk[pkey(kind, a)]
	if b == nil {
		t.Fatal("503 应建立熔断")
	}
	until := b.until
	fails := b.fails

	s.ReportOutcome(kind, ca, OutcomeMiss, 5*time.Millisecond, Hint{Code: 404})
	if got := s.brk[pkey(kind, a)]; got == nil || !got.until.Equal(until) || got.fails != fails {
		t.Fatal("404 不应清掉或改动已有熔断")
	}

	// 反过来：光有 404 也不能凭空建熔断。
	c := addSource(t, st, "https://c.example", true, 100, 1000)
	s.ReportOutcome(kind, Candidate{ID: c, Name: "c"}, OutcomeMiss, 5*time.Millisecond, Hint{Code: 410})
	if _, cooled := s.brk[pkey(kind, c)]; cooled {
		t.Fatal("410 不该建立熔断")
	}
}

// 429 仍然进冷却，且 Retry-After 生效。
func TestRateLimitedStillCoolsWithRetryAfter(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Second, MaxCooldown: time.Minute})
	ca := Candidate{ID: a, Name: "a"}

	before := time.Now()
	s.ReportOutcome("docker", ca, OutcomeFail, 5*time.Millisecond, Hint{Code: 429, RetryAfter: 20 * time.Second})
	b := s.brk[pkey("docker", a)]
	if b == nil {
		t.Fatal("429 应建立熔断")
	}
	if b.until.Before(before.Add(20 * time.Second)) {
		t.Fatalf("冷却应不短于 Retry-After 20s，实际到 %v", b.until.Sub(before))
	}
}

// Miss/Auth 必须按协议族隔离：quay.io 的 404 不能动 docker/ghcr 的粘性与熔断。
func TestNeutralIsIsolatedByKind(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	ca := Candidate{ID: a, Name: "a"}

	s.ReportOutcome("docker", ca, OutcomeOK, 5*time.Millisecond, Hint{Code: 200})
	dockerSticky := s.sticky["docker"].cand.ID

	s.ReportOutcome("registry:quay.io", ca, OutcomeMiss, 5*time.Millisecond, Hint{Code: 404})
	s.ReportOutcome("registry:ghcr.io", ca, OutcomeAuth, 5*time.Millisecond, Hint{Code: 401})

	if got := s.sticky["docker"].cand.ID; got != dockerSticky {
		t.Fatal("registry 族的中性结果污染了 docker 粘性")
	}
	if _, ok := s.sticky["registry:quay.io"]; ok {
		t.Fatal("quay.io 不该建立粘性")
	}
	if _, ok := s.sticky["registry:ghcr.io"]; ok {
		t.Fatal("ghcr.io 不该建立粘性")
	}
	if len(s.brk) != 0 {
		t.Fatalf("中性结果不该产生任何熔断，实际 %d 条", len(s.brk))
	}
}

// 并发 Report 下无数据竞争（配合 -race 跑）。
func TestReportOutcomeConcurrent(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Millisecond})
	metrics.Enable()
	t.Cleanup(metrics.Disable)
	ca := Candidate{ID: a, Name: "a"}
	outcomes := []Outcome{OutcomeOK, OutcomeMiss, OutcomeAuth, OutcomeFail}
	codes := []int{200, 404, 401, 503}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := (g + i) % len(outcomes)
				s.ReportOutcome("registry:quay.io", ca, outcomes[k], time.Millisecond, Hint{Code: codes[k]})
			}
		}(g)
	}
	wg.Wait()

	// 并发结束后状态仍自洽：熔断计数是个正整数，粘性只可能指向这台源。
	if b := s.brk[pkey("registry:quay.io", a)]; b != nil && b.fails <= 0 {
		t.Fatalf("熔断计数异常: %d", b.fails)
	}
	if p, ok := s.sticky["registry:quay.io"]; ok && p.cand.ID != a {
		t.Fatalf("粘性指向了不存在的源 %d", p.cand.ID)
	}
}

// 兼容入口不能改变语义：true 仍是成功，false 仍是失败。
func TestReportCompatWrapsOutcomes(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	ca := Candidate{ID: a, Name: "a"}

	s.Report("raw", ca, true, time.Millisecond, Hint{Code: 200})
	if _, ok := s.sticky["raw"]; !ok {
		t.Fatal("Report(true) 应等价于 OutcomeOK")
	}
	s.Report("raw", ca, false, time.Millisecond, Hint{Code: 502})
	if _, ok := s.brk[pkey("raw", a)]; !ok {
		t.Fatal("Report(false) 应等价于 OutcomeFail")
	}
}

// 源被删除后迟到的 Report 不能把熔断/粘性写回来。
func TestReportOutcomeSkipsGoneSource(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1000)
	s := New(st, Config{Enabled: true, MaxCandidates: 3})
	s.ReportOutcome("docker", Candidate{ID: a, Name: "a"}, OutcomeOK, 0, Hint{Code: 200})
	if err := st.DeleteSource(a); err != nil {
		t.Fatalf("删源: %v", err)
	}
	s.Forget(a)

	s.ReportOutcome("docker", Candidate{ID: a, Name: "a"}, OutcomeOK, 0, Hint{Code: 200})
	s.ReportOutcome("docker", Candidate{ID: a, Name: "a"}, OutcomeFail, 0, Hint{Code: 503})
	if _, ok := s.sticky["docker"]; ok {
		t.Fatal("已删源不该被写回粘性")
	}
	if len(s.brk) != 0 {
		t.Fatalf("已删源不该被写回熔断，实际 %d 条", len(s.brk))
	}
}
