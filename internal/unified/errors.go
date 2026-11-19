// Package unified 实现 3.5.17 统一执行框架的预算、单占权与结果仲裁。
//
// 本包只负责「记账与准入」，不实现 HTTP 细节：RequestScope 拥有 R/P 账本，
// Claim 保证同一身份跨链只派发一次，Chain/Attempt 提供生命周期状态机，
// Ledger 负责请求级全 Miss 仲裁，Vote/Authority 负责 Manifest 身份确认。
//
// 冻结语义见 docs/3.5.17-unified-framework-design.md；数值不得在调用方覆盖。
package unified

import "errors"

var (
	// ErrBudgetExhausted 表示 RequestScope 的 R 或 P 硬上限已用尽。
	// 它是独立通道：不是上游故障，也不能被折叠成 404/401。
	ErrBudgetExhausted = errors.New("unified: request budget exhausted")
	// ErrScopeClosed 表示 RequestScope 已关闭，不再接受新的链或预算申请。
	ErrScopeClosed = errors.New("unified: request scope closed")
	// ErrChainClosed 表示 Chain 已进入 Closing/Closed，拒绝注册新 Attempt。
	ErrChainClosed = errors.New("unified: chain closed")
	// ErrClaimConflict 表示同一 Identity 已被另一条 Chain 占住。
	ErrClaimConflict = errors.New("unified: identity claim conflict")
	// ErrClaimReleased 表示 Claim 已被释放（或已换代），旧持有者不能再提交。
	ErrClaimReleased = errors.New("unified: identity claim released")
	// ErrClaimDispatched 表示 Claim 已越过发送提交边界，不允许释放/被接管。
	ErrClaimDispatched = errors.New("unified: identity claim already dispatched")
	// ErrAttemptState 表示 Attempt 状态机不接受该迁移。
	ErrAttemptState = errors.New("unified: invalid attempt state transition")
	// ErrRetryTokenSpent 表示该 Registry 的重试令牌已耗尽。
	ErrRetryTokenSpent = errors.New("unified: retry token spent")
	// ErrRangeWorkCap 表示分段计划超出 RangeWorkCap。
	ErrRangeWorkCap = errors.New("unified: range work cap exceeded")
)

// 冻结常量（design §3）。改这些值等于改冻结语义，必须重新定版。
const (
	// RMax 是 RequestScope 逻辑 Attempt 累计硬上限。
	RMax = 16
	// PMax 是 RequestScope 应用层受控 RoundTrip 派发硬上限。
	PMax = 8192
	// RangeWorkCap 是 Midfill/tryParallel 的 Range 工作单元上限。
	RangeWorkCap = 1024
	// MaxSegmentCount 是单次分段计划的最大分段数。
	MaxSegmentCount = 256
	// MaxRangeTargets 是每个 Range 分段最多的目标数（含官方源）。
	MaxRangeTargets = 4
)

// ExhaustedError 携带耗尽发生在哪本账上，供 503 文案与指标区分。
type ExhaustedError struct {
	Kind string // "R" 或 "P"
	Max  int64
}

func (e *ExhaustedError) Error() string {
	if e == nil {
		return ErrBudgetExhausted.Error()
	}
	if e.Kind == "" {
		return ErrBudgetExhausted.Error()
	}
	return "unified: request budget exhausted (" + e.Kind + ")"
}

// Unwrap 让 errors.Is(err, ErrBudgetExhausted) 成立。
func (e *ExhaustedError) Unwrap() error { return ErrBudgetExhausted }

// IsBudgetExhausted 判定错误链里是否有预算耗尽。
func IsBudgetExhausted(err error) bool { return errors.Is(err, ErrBudgetExhausted) }
