package unified

import (
	"sync"
	"sync/atomic"
)

// PBudget 是 RequestScope 级应用层受控派发账本（两阶段）。
//
// outstanding = 已提交 + 未提交预留。已提交永不返还，因此「已提交」本身
// 就是累计派发数；预留未提交时取消必须归还，否则取消会吃掉预算。
type PBudget struct {
	max         int64
	outstanding atomic.Int64
	committed   atomic.Int64
	reserved    atomic.Int64
}

func newPBudget(max int64) *PBudget {
	if max <= 0 {
		max = PMax
	}
	return &PBudget{max: max}
}

// Max 返回硬上限。
func (b *PBudget) Max() int64 { return b.max }

// Committed 返回已经正式消耗的派发数；它只增不减。
func (b *PBudget) Committed() int64 { return b.committed.Load() }

// Outstanding 返回已提交 + 未提交预留。
func (b *PBudget) Outstanding() int64 { return b.outstanding.Load() }

// Remaining 返回还能预留多少。
func (b *PBudget) Remaining() int64 {
	n := b.max - b.outstanding.Load()
	if n < 0 {
		return 0
	}
	return n
}

// ReserveP 原子预留一次派发额度。父 Context 已取消时直接失败，不再占账。
func (b *PBudget) ReserveP(parent ctxChecker) (*PReservation, error) {
	if parent != nil && parent.Err() != nil {
		return nil, parent.Err()
	}
	for {
		cur := b.outstanding.Load()
		if cur >= b.max {
			return nil, &ExhaustedError{Kind: "P", Max: b.max}
		}
		if b.outstanding.CompareAndSwap(cur, cur+1) {
			b.reserved.Add(1)
			return &PReservation{b: b, ctx: parent}, nil
		}
	}
}

// PReservation 是一次派发的两阶段凭证。Commit 与 Release 只能生效一次，
// 已提交后 Release 不返还额度（提交即消耗，无论结果）。
type PReservation struct {
	b    *PBudget
	ctx  ctxChecker
	once atomic.Int32 // 0=待决 1=已提交 2=已返还
}

// Commit 在调用底层 RoundTrip 之前正式消耗额度。父 Context 已取消时
// 不提交并归还预留——否则取消会永久吃掉一个 P 名额。返回 false 表示
// 该凭证已被提交/返还，或提交前已取消。
func (r *PReservation) Commit() bool {
	if r == nil || r.b == nil {
		return false
	}
	if r.ctx != nil && r.ctx.Err() != nil {
		r.Release()
		return false
	}
	if !r.once.CompareAndSwap(0, 1) {
		return false
	}
	r.b.committed.Add(1)
	r.b.reserved.Add(-1)
	return true
}

// Release 返还尚未提交的预留。已提交的额度不返还。
func (r *PReservation) Release() bool {
	if r == nil || r.b == nil {
		return false
	}
	if !r.once.CompareAndSwap(0, 2) {
		return false
	}
	r.b.reserved.Add(-1)
	r.b.outstanding.Add(-1)
	return true
}

// Committed 报告该凭证是否已提交。
func (r *PReservation) Committed() bool {
	return r != nil && r.once.Load() == 1
}

// RBudget 是 RequestScope 逻辑 Attempt 账本。
//
// 只有越过发送提交边界（Dispatched）的 Attempt 才提交 R；Prepared 阶段取消
// 只归还预留，不形成消耗。RangeWork 走 P 与 RangeWorkCap，不经过这里。
type RBudget struct {
	max      int64
	reserved atomic.Int64
	used     atomic.Int64
}

func newRBudget(max int64) *RBudget {
	if max <= 0 {
		max = RMax
	}
	return &RBudget{max: max}
}

// Max 返回硬上限。
func (b *RBudget) Max() int64 { return b.max }

// Used 返回已提交的 R。
func (b *RBudget) Used() int64 { return b.used.Load() }

// Reserved 返回尚未提交的预留。
func (b *RBudget) Reserved() int64 { return b.reserved.Load() }

// ReserveR 原子预留一个逻辑 Attempt 名额。
func (b *RBudget) ReserveR(parent ctxChecker) (*RReservation, error) {
	if parent != nil && parent.Err() != nil {
		return nil, parent.Err()
	}
	for {
		used := b.used.Load()
		res := b.reserved.Load()
		if used+res >= b.max {
			return nil, &ExhaustedError{Kind: "R", Max: b.max}
		}
		if b.reserved.CompareAndSwap(res, res+1) {
			return &RReservation{b: b}, nil
		}
	}
}

// RReservation 是一个逻辑 Attempt 的两阶段凭证。
type RReservation struct {
	b    *RBudget
	once atomic.Int32 // 0=待决 1=已提交 2=已归还
}

// Commit 在越过发送提交边界时调用。
func (r *RReservation) Commit() bool {
	if r == nil || r.b == nil {
		return false
	}
	if !r.once.CompareAndSwap(0, 1) {
		return false
	}
	r.b.reserved.Add(-1)
	r.b.used.Add(1)
	return true
}

// Release 归还尚未提交的预留（Prepared 阶段取消）。
func (r *RReservation) Release() bool {
	if r == nil || r.b == nil {
		return false
	}
	if !r.once.CompareAndSwap(0, 2) {
		return false
	}
	r.b.reserved.Add(-1)
	return true
}

// Committed 报告是否已提交。
func (r *RReservation) Committed() bool {
	return r != nil && r.once.Load() == 1
}

// RangeLedger 记 RangeWork 消耗（与 R 分离）。它只做上限校验与计量，
// 具体的 P 派发仍由每次 Range 请求自己的 ReserveP/CommitP 完成。
type RangeLedger struct {
	max      int64
	mu       sync.Mutex
	work     int64
	segments int
}

func newRangeLedger(max int64) *RangeLedger {
	if max <= 0 {
		max = RangeWorkCap
	}
	return &RangeLedger{max: max}
}

// Reserve 预留 n 个 Range 工作单元；总量超过 RangeWorkCap 时拒绝。
func (r *RangeLedger) Reserve(n int) error {
	if n <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.work+int64(n) > r.max {
		return ErrRangeWorkCap
	}
	r.work += int64(n)
	return nil
}

// Max 返回上限。
func (r *RangeLedger) Max() int64 { return r.max }

// Used 返回已用工作单元。
func (r *RangeLedger) Used() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.work
}

// AddSegments 记录一次分段计划的段数，用于「每 Scope 一轮 Preflight」的审计。
func (r *RangeLedger) AddSegments(n int) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	r.segments += n
	r.mu.Unlock()
}

// Segments 返回累计规划段数。
func (r *RangeLedger) Segments() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.segments
}

// PlanRange 计算分段计划：段数不超过 256、块不小于 minChunk、每段目标不超过 4。
// 返回段数与最终块大小；目标数超限直接报错，不做静默截断——截断会让
// 「每 Range 最多 4 个目标」变成调用方不可见的隐式行为。
func PlanRange(rest, chunk int64, targets int) (int, int64, error) {
	if targets > MaxRangeTargets {
		return 0, 0, ErrRangeWorkCap
	}
	if rest <= 0 {
		return 0, chunk, nil
	}
	if chunk <= 0 {
		chunk = 256 << 10
	}
	n := segmentCount(rest, chunk)
	for n > MaxSegmentCount {
		chunk *= 2
		n = segmentCount(rest, chunk)
	}
	if n > MaxSegmentCount && targets > 0 && n*targets > RangeWorkCap {
		return 0, chunk, ErrRangeWorkCap
	}
	return n, chunk, nil
}

func segmentCount(rest, chunk int64) int {
	if rest <= 0 || chunk <= 0 {
		return 0
	}
	return int((rest + chunk - 1) / chunk)
}
