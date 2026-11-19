package unified

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// Outcome 是单 Attempt 的结果分类（scheduler 四态 + 取消/预算两条独立语义）。
type Outcome uint8

const (
	// OutcomeUnknown 表示未结算（尚未执行完）。
	OutcomeUnknown Outcome = iota
	// OutcomeOK 表示成功交付。
	OutcomeOK
	// OutcomeMiss 表示上游明确 404/410。
	OutcomeMiss
	// OutcomeAuth 表示认证被拒（401/403）。
	OutcomeAuth
	// OutcomeFail 表示上游故障（5xx/超时/连接失败/内容校验失败）。
	OutcomeFail
	// OutcomeCancelled 表示 Attempt 在派发后被取消；它阻止全 Miss 结论。
	OutcomeCancelled
	// OutcomeBudget 表示预算耗尽；独立最高优先级通道。
	OutcomeBudget
	// OutcomePending 表示仍在执行，暂不结算。
	OutcomePending
)

func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeMiss:
		return "miss"
	case OutcomeAuth:
		return "auth"
	case OutcomeFail:
		return "fail"
	case OutcomeCancelled:
		return "cancelled"
	case OutcomeBudget:
		return "budget"
	case OutcomePending:
		return "pending"
	default:
		return "unknown"
	}
}

// ErrNoAttempts 表示一个请求走完全部候选后没有任何结果记录。
var ErrNoAttempts = errors.New("unified: no attempt results")

// AttemptResult 是结果账本的一行。Chain 归属必须保留：同 Scope 内
// Main/A/B/fb 都写这张表，但仲裁需要知道结果来自哪条链。
type AttemptResult struct {
	ChainID   string
	ChainKind ChainKind
	AttemptID uint64
	SourceID  int64
	Registry  string
	Outcome   Outcome
	Code      int
	Digest    string
	Verified  bool
	Err       error
	At        time.Time
}

// Decision 是请求级仲裁结论。NegativeCache 永远为 false：失败的请求
// 不写负缓存，下一次必须重新走候选。
type Decision struct {
	Outcome       Outcome
	Code          int
	Err           error
	Reason        string
	Settled       bool
	NegativeCache bool
}

// Ledger 是 RequestScope 的结果账本。
type Ledger struct {
	mu       sync.Mutex
	scope    *Scope
	rows     []AttemptResult
	policy   error
	budgetFn func() bool
}

func newLedger(s *Scope) *Ledger {
	return &Ledger{scope: s}
}

// Record 追加一条结果。OK 之后仍可追加（竞速输家清理时也会记账），
// 由仲裁规则处理优先级。
func (l *Ledger) Record(r AttemptResult) {
	if l == nil {
		return
	}
	if r.At.IsZero() {
		r.At = time.Now()
	}
	l.mu.Lock()
	l.rows = append(l.rows, r)
	l.mu.Unlock()
}

// SetPolicyError 记录策略/身份缺失。它优先于任何上游状态码。
func (l *Ledger) SetPolicyError(err error) {
	if l == nil || err == nil {
		return
	}
	l.mu.Lock()
	if l.policy == nil {
		l.policy = err
	}
	l.mu.Unlock()
}

// Snapshot 返回结果副本。
func (l *Ledger) Snapshot() []AttemptResult {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AttemptResult, len(l.rows))
	copy(out, l.rows)
	return out
}

// Len 返回结果行数。
func (l *Ledger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.rows)
}

// CountByOutcome 统计某个结果的数量。
func (l *Ledger) CountByOutcome(o Outcome) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.rows {
		if r.Outcome == o {
			n++
		}
	}
	return n
}

// Decide 计算请求级仲裁。规则顺序（pre-freeze audit §3.5 + AUD-04-6）：
// 成功 > P 耗尽 > policyErr > 未完成 > Auth > Cancelled > 全 Miss(404) > Fail。
func (l *Ledger) Decide() Decision {
	if l == nil {
		return Decision{Outcome: OutcomeFail, Code: http.StatusBadGateway, Err: ErrNoAttempts}
	}
	l.mu.Lock()
	rows := make([]AttemptResult, len(l.rows))
	copy(rows, l.rows)
	policy := l.policy
	l.mu.Unlock()

	var (
		ok, miss, auth, fail, cancelled, pending, budget int
		lastFail, lastAuth                               error
		failCode, authCode                               int
	)
	for _, r := range rows {
		switch r.Outcome {
		case OutcomeOK:
			ok++
		case OutcomeMiss:
			miss++
		case OutcomeAuth:
			auth++
			if r.Err != nil {
				lastAuth = r.Err
			}
			if r.Code != 0 {
				authCode = r.Code
			}
		case OutcomeFail:
			fail++
			if r.Err != nil {
				lastFail = r.Err
			}
			if r.Code != 0 {
				failCode = r.Code
			}
		case OutcomeCancelled:
			cancelled++
		case OutcomePending:
			pending++
		case OutcomeBudget:
			budget++
		}
	}

	if ok > 0 {
		return Decision{Outcome: OutcomeOK, Code: http.StatusOK, Settled: true, Reason: "success"}
	}
	if budget > 0 || (l.scope != nil && l.scope.Exhausted()) {
		return Decision{
			Outcome: OutcomeBudget,
			Code:    http.StatusServiceUnavailable,
			Err:     ErrBudgetExhausted,
			Settled: true,
			Reason:  "budget exhausted",
		}
	}
	if policy != nil {
		return Decision{Outcome: OutcomeFail, Code: http.StatusServiceUnavailable, Err: policy, Settled: true, Reason: "policy error"}
	}
	if pending > 0 {
		return Decision{Outcome: OutcomePending, Settled: false, Reason: "attempts still running"}
	}
	if auth > 0 {
		code := authCode
		if code == 0 {
			code = http.StatusUnauthorized
		}
		return Decision{Outcome: OutcomeAuth, Code: code, Err: lastAuth, Settled: true, Reason: "auth denied"}
	}
	if cancelled > 0 {
		return Decision{Outcome: OutcomeCancelled, Code: http.StatusBadGateway, Settled: true, Reason: "cancelled attempts block all-miss"}
	}
	if len(rows) == 0 {
		return Decision{Outcome: OutcomeFail, Code: http.StatusBadGateway, Err: ErrNoAttempts, Settled: true, Reason: "no attempts"}
	}
	if miss == len(rows) {
		return Decision{Outcome: OutcomeMiss, Code: http.StatusNotFound, Settled: true, Reason: "all dispatched attempts miss"}
	}
	if fail > 0 {
		code := failCode
		if code == 0 {
			code = http.StatusBadGateway
		}
		if lastFail == nil {
			lastFail = errors.New("upstream failure")
		}
		return Decision{Outcome: OutcomeFail, Code: code, Err: lastFail, Settled: true, Reason: "partial failure is not a miss"}
	}
	return Decision{Outcome: OutcomeFail, Code: http.StatusBadGateway, Err: ErrNoAttempts, Settled: true, Reason: "unclassified"}
}
