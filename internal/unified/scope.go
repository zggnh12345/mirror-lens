package unified

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// ctxChecker 只取 Err：预算申请不应该把整个 Context 语义拖进账本。
type ctxChecker interface {
	Err() error
}

// CandidateRef 是候选选择快照的最小只读副本。Scope 开始时固定，
// 请求执行中映射/排序变化不改变本 Scope 的候选集合（schema-design §3.3）。
type CandidateRef struct {
	SourceID  int64
	BaseURL   string
	Preferred bool
	Priority  int
}

// ScopeConfig 是 RequestScope 的创建参数。数值默认值就是冻结值。
type ScopeConfig struct {
	RMax int64
	PMax int64

	// Generation 是 Scope 开始时固定的每 Registry 映射代次。
	Generation map[string]uint64
	// Candidates 是 Scope 开始时固定的候选快照（按 registry host）。
	Candidates map[string][]CandidateRef

	// Now 仅用于测试注入；生产用 time.Now。
	Now func() time.Time
	// OnExhaust 在首次预算耗尽时回调一次（指标/日志），不阻塞。
	OnExhaust func(kind string)
}

// Scope 是一个客户端请求的全生命周期容器，统一拥有 R/P/Range 账本、
// Claim 表、Chain 注册表与结果账本。B/fb/Authority/Hedge/Midfill 不得
// 各自创建独立 Scope（design §2.1）。
type Scope struct {
	id     uint64
	ctx    context.Context
	cancel context.CancelFunc
	parent context.Context
	cfg    ScopeConfig

	r  *RBudget
	p  *PBudget
	rw *RangeLedger

	claims *claimStore
	ledger *Ledger
	tokens *RetryTokens

	mu      sync.Mutex
	closed  bool
	closing bool
	chains  map[string]*Chain
	seqC    int64
	seqA    int64
	pool    *Pool
	pooled  bool

	preflight *preflightCache
	bodies    map[uint64]func()
	bodySeq   uint64
	done      chan struct{}
	closeOnce sync.Once

	inflight sync.WaitGroup
	now      func() time.Time

	exhausted atomic.Int32
	gen       atomic.Uint64
}

var scopeSeq atomic.Uint64

// NewScope 创建 RequestScope。pool 非空时先从全局池取得容量；取不到
// 返回 ErrScopeClosed（调用方转 503）。
func NewScope(parent context.Context, cfg ScopeConfig, pool *Pool) (*Scope, error) {
	if parent == nil {
		parent = context.Background()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// 候选与代次必须在创建时冻结：调用方后续修改原 map/slice 不得影响本 Scope。
	cfg.Generation = cloneGeneration(cfg.Generation)
	cfg.Candidates = cloneCandidates(cfg.Candidates)
	if pool != nil {
		if err := pool.Acquire(parent); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Scope{
		id:     scopeSeq.Add(1),
		ctx:    ctx,
		cancel: cancel,
		parent: parent,
		cfg:    cfg,
		r:      newRBudget(cfg.RMax),
		p:      newPBudget(cfg.PMax),
		rw:     newRangeLedger(RangeWorkCap),
		claims: newClaimStore(cfg.Now),
		tokens: NewRetryTokens(1),
		chains: map[string]*Chain{},
		bodies: map[uint64]func(){},
		pool:   pool,
		pooled: pool != nil,
		now:    cfg.Now,
		done:   make(chan struct{}),
	}
	s.ledger = newLedger(s)
	s.preflight = newPreflightCache()
	return s, nil
}

// ID 返回全局自增的 Scope 编号（诊断用）。
func (s *Scope) ID() uint64 {
	if s == nil {
		return 0
	}
	return s.id
}

// Context 返回 Scope 的派生根 Context。取消父 Context 会传播到这里。
func (s *Scope) Context() context.Context {
	if s == nil {
		return context.Background()
	}
	return s.ctx
}

// Cancel 取消 Scope 派生的所有在途 Attempt。可重复调用。
func (s *Scope) Cancel() {
	if s == nil {
		return
	}
	s.cancel()
}

// Err 返回 Scope Context 的取消原因。
func (s *Scope) Err() error {
	if s == nil {
		return nil
	}
	return s.ctx.Err()
}

// Budget 返回 R 账本。
func (s *Scope) R() *RBudget { return s.r }

// Budget 返回 P 账本。
func (s *Scope) P() *PBudget { return s.p }

// Ranges 返回 RangeWork 账本。
func (s *Scope) Ranges() *RangeLedger { return s.rw }

// Tokens 返回 per-Registry 重试令牌表。
func (s *Scope) Tokens() *RetryTokens { return s.tokens }

// Generation 返回 Scope 开始时固定的映射代次。
func (s *Scope) Generation(host string) uint64 {
	if s == nil {
		return 0
	}
	return s.cfg.Generation[host]
}

// Candidates 返回 Scope 开始时固定的候选快照。
func (s *Scope) Candidates(host string) []CandidateRef {
	if s == nil {
		return nil
	}
	return s.cfg.Candidates[host]
}

// Claim 为 chainID 预占 identity（见 claimStore.Acquire）。
func (s *Scope) Claim(chainID string, id Identity) (*ClaimHandle, error) {
	if s == nil {
		return nil, ErrScopeClosed
	}
	s.mu.Lock()
	closed := s.closed || s.closing
	s.mu.Unlock()
	if closed {
		return nil, ErrScopeClosed
	}
	return s.claims.Acquire(chainID, id)
}

// ReserveP 预留一次派发额度，父 Context 或 Scope Context 已取消时失败。
func (s *Scope) ReserveP() (*PReservation, error) {
	if s == nil {
		return nil, ErrScopeClosed
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	res, err := s.p.ReserveP(s.ctx)
	if err != nil {
		s.markExhausted("P")
		return nil, err
	}
	return res, nil
}

// markExhausted 幂等地记录首次耗尽事件，并执行冻结的收尾动作：
// 取消剩余 Attempt、关闭所有未完成 Body。之后的返回路径必须自行保证
// 不提交缓存、不保留 .part、不新增 Range/Hedge/Preflight/换源。
func (s *Scope) markExhausted(kind string) {
	if s == nil {
		return
	}
	if !s.exhausted.CompareAndSwap(0, 1) {
		return
	}
	if s.cfg.OnExhaust != nil {
		s.cfg.OnExhaust(kind)
	}
	s.mu.Lock()
	closers := make([]func(), 0, len(s.bodies))
	for _, fn := range s.bodies {
		closers = append(closers, fn)
	}
	s.bodies = map[uint64]func(){}
	s.mu.Unlock()
	for _, fn := range closers {
		fn()
	}
	// 取消 Scope 派生的所有 Attempt（通过 context.AfterFunc 挂在 sc.ctx 上）。
	s.cancel()
}

// Exhaust 显式触发预算耗尽收尾（供 PTransport 与测试使用）。
// 幂等：只有第一次生效。
func (s *Scope) Exhaust(kind string) {
	s.markExhausted(kind)
}

// Exhausted 报告 Scope 是否发生过预算耗尽。
func (s *Scope) Exhausted() bool {
	return s != nil && s.exhausted.Load() != 0
}

// Ledger 返回结果账本。
func (s *Scope) Ledger() *Ledger { return s.ledger }

// Claims 返回 Claim 表快照（Identity→状态），供验收断言「零重复 Dispatched」。
func (s *Scope) Claims() map[Identity]ClaimState {
	if s == nil {
		return nil
	}
	return s.claims.snapshot()
}

// DispatchedClaims 返回已越过发送提交边界的 Identity 数。
func (s *Scope) DispatchedClaims() int {
	if s == nil {
		return 0
	}
	return s.claims.dispatchedCount()
}

// ClaimStateOf 返回某 Identity 在当前 Scope 内的 Claim 状态。
// 第二返回值为 false 表示表里还没有该 Identity（从未预占）。
func (s *Scope) ClaimStateOf(id Identity) (ClaimState, bool) {
	if s == nil || s.claims == nil {
		return ClaimReleased, false
	}
	s.claims.mu.Lock()
	defer s.claims.mu.Unlock()
	e := s.claims.m[id]
	if e == nil {
		return ClaimReleased, false
	}
	return e.state, true
}

// ChainCount 返回当前登记的执行链数（诊断/验收用）。
func (s *Scope) ChainCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chains)
}

// Close 关闭 Scope：拒绝新链、释放全局池名额。必须在所有 Body 关闭、
// Lease 释放之后调用；它不等待在途 Attempt（Wait 负责）。可重复调用。
func (s *Scope) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.closed = true
		chains := make([]*Chain, 0, len(s.chains))
		for _, c := range s.chains {
			chains = append(chains, c)
		}
		s.mu.Unlock()
		for _, c := range chains {
			c.Close()
		}
		if s.pooled && s.pool != nil {
			s.pool.Release()
		}
		close(s.done)
	})
}

// TrackBody 登记一个未完成的上游 Body 关闭函数。预算耗尽时 Scope 会
// 主动调用它，保证「已发正文后耗尽」能中断传输。返回的函数用于注销。
// 已耗尽/已关闭时立即执行 closeFn 并返回 false。
func (s *Scope) TrackBody(closeFn func()) (func(), bool) {
	if s == nil || closeFn == nil {
		return func() {}, false
	}
	s.mu.Lock()
	if s.closed || s.exhausted.Load() != 0 {
		s.mu.Unlock()
		closeFn()
		return func() {}, false
	}
	s.bodySeq++
	id := s.bodySeq
	if s.bodies == nil {
		s.bodies = map[uint64]func(){}
	}
	s.bodies[id] = closeFn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.bodies, id)
		s.mu.Unlock()
	}, true
}

// Done 返回 Scope 关闭信号。
func (s *Scope) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// Closed 报告 Scope 是否已关闭。
func (s *Scope) Closed() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Wait 等待所有在途 Attempt 结束。
func (s *Scope) Wait() { s.inflight.Wait() }

// Drain 取消并等待，供 defer 收尾：先取消在途请求，再等它们退出。
func (s *Scope) Drain() {
	s.Cancel()
	s.Wait()
}

func (s *Scope) nextChainID(kind ChainKind) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqC++
	return kind.String() + "#" + itoa(s.seqC)
}

func (s *Scope) nextAttemptID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqA++
	return uint64(s.seqA)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// Preflight：每 Scope 一轮，tryParallel / midFill 共享
// ---------------------------------------------------------------------------

// PreflightResult 是单目标的 1 字节探活结果。
type PreflightResult struct {
	Target      string
	SourceID    int64
	OK          bool
	Total       int64
	ContentType string
}

type preflightCache struct {
	mu      sync.Mutex
	running bool
	done    chan struct{}
	once    bool
	results []PreflightResult
	total   int64
	err     error
}

func newPreflightCache() *preflightCache {
	return &preflightCache{done: make(chan struct{})}
}

// Preflight 执行（或复用）本 Scope 唯一的一轮预检。并发调用只有第一个
// 执行 fn，其余等待同一结果——这就是「一轮 Preflight 共享快照」。
func (s *Scope) Preflight(ctx context.Context, fn func(context.Context) ([]PreflightResult, int64, error)) ([]PreflightResult, int64, error) {
	if s == nil {
		return nil, -1, ErrScopeClosed
	}
	pf := s.preflight
	pf.mu.Lock()
	if pf.once {
		res, total, err := pf.results, pf.total, pf.err
		pf.mu.Unlock()
		return res, total, err
	}
	if pf.running {
		done := pf.done
		pf.mu.Unlock()
		<-done
		pf.mu.Lock()
		res, total, err := pf.results, pf.total, pf.err
		pf.mu.Unlock()
		return res, total, err
	}
	pf.running = true
	pf.mu.Unlock()

	res, total, err := fn(ctx)

	pf.mu.Lock()
	pf.results, pf.total, pf.err = res, total, err
	pf.once = true
	pf.running = false
	close(pf.done)
	pf.mu.Unlock()
	return res, total, err
}

// PreflightRuns 返回本 Scope 已执行的预检轮数（0 或 1），供 T55 断言。
func (s *Scope) PreflightRuns() int {
	if s == nil || s.preflight == nil {
		return 0
	}
	s.preflight.mu.Lock()
	defer s.preflight.mu.Unlock()
	if s.preflight.once {
		return 1
	}
	return 0
}

func cloneGeneration(in map[string]uint64) map[string]uint64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]uint64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneCandidates(in map[string][]CandidateRef) map[string][]CandidateRef {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]CandidateRef, len(in))
	for k, v := range in {
		cp := make([]CandidateRef, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}
