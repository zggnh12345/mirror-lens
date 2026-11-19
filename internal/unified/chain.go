package unified

import (
	"sync"
	"time"
)

// ChainKind 是执行链类型。它们共享同一个 RequestScope 账本，只是归属不同。
type ChainKind uint8

const (
	// ChainMain 是主执行链（顺序回退/首选源）。
	ChainMain ChainKind = iota
	// ChainA 是站内竞速链（同 Identity 多 Source 共享 Claim）。
	ChainA
	// ChainB 是跨站竞速链。
	ChainB
	// ChainFallback 是 fb 备用链，受静态抑制快照约束。
	ChainFallback
)

func (k ChainKind) String() string {
	switch k {
	case ChainA:
		return "A"
	case ChainB:
		return "B"
	case ChainFallback:
		return "fb"
	default:
		return "main"
	}
}

// ChainState 是 Chain 生命周期。
type ChainState uint8

const (
	ChainCreated ChainState = iota
	ChainActive
	ChainClosing
	ChainClosed
)

func (s ChainState) String() string {
	switch s {
	case ChainActive:
		return "active"
	case ChainClosing:
		return "closing"
	case ChainClosed:
		return "closed"
	default:
		return "created"
	}
}

// Chain 是一条执行链。Closing 之后不得再注册 Attempt（T53）。
type Chain struct {
	id    string
	kind  ChainKind
	scope *Scope

	mu     sync.Mutex
	state  ChainState
	atts   []*Attempt
	claims []*ClaimHandle
	closed chan struct{}
}

// NewChain 在 Scope 内创建一条链。Scope 已关闭时返回 ErrScopeClosed。
func (s *Scope) NewChain(id string, kind ChainKind) (*Chain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing {
		return nil, ErrScopeClosed
	}
	if id == "" {
		id = s.nextChainID(kind)
	}
	if _, dup := s.chains[id]; dup {
		return nil, ErrClaimConflict
	}
	c := &Chain{id: id, kind: kind, scope: s, state: ChainCreated, closed: make(chan struct{})}
	s.chains[id] = c
	return c, nil
}

// chainFor 返回 Scope 内某类链的唯一实例表（内部用）。默认链 ID 固定为
// 类型名，因此同一请求的同类执行路径必然共享同一条链与同一个 Claim。
func (s *Scope) chainFor(kind ChainKind) (*Chain, error) {
	if s == nil {
		return nil, ErrScopeClosed
	}
	id := "bind-" + kind.String()
	s.mu.Lock()
	c := s.chains[id]
	closed := s.closed || s.closing
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	if closed {
		return nil, ErrScopeClosed
	}
	return s.NewChain(id, kind)
}

// ChainFor 是 chainFor 的导出形式，供执行层把请求绑定到某类链。
// 同一 Scope 内同类链只创建一次；返回的链关闭后再调用会重新创建。
func (s *Scope) ChainFor(kind ChainKind) (*Chain, error) {
	c, err := s.chainFor(kind)
	if err == nil && c.State() == ChainClosed {
		s.mu.Lock()
		delete(s.chains, "bind-"+kind.String())
		s.mu.Unlock()
		return s.chainFor(kind)
	}
	return c, err
}

// ID 返回链标识。
func (c *Chain) ID() string {
	if c == nil {
		return ""
	}
	return c.id
}

// Kind 返回链类型。
func (c *Chain) Kind() ChainKind {
	if c == nil {
		return ChainMain
	}
	return c.kind
}

// State 返回链状态。
func (c *Chain) State() ChainState {
	if c == nil {
		return ChainClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Activate 把 Created 推进到 Active。重复调用幂等。
func (c *Chain) Activate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.state == ChainCreated {
		c.state = ChainActive
	}
	c.mu.Unlock()
}

// RegisterAttempt 注册一个新的 Attempt。Closing/Closed 后一律拒绝（T53），
// Scope 已关闭同样拒绝。R 在注册时预留，越界返回 ErrBudgetExhausted。
func (c *Chain) RegisterAttempt(claim *ClaimHandle) (*Attempt, error) {
	if c == nil {
		return nil, ErrChainClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == ChainClosing || c.state == ChainClosed {
		return nil, ErrChainClosed
	}
	if c.state == ChainCreated {
		c.state = ChainActive
	}
	sc := c.scope
	rr, err := sc.r.ReserveR(sc.ctx)
	if err != nil {
		return nil, err
	}
	a := &Attempt{
		id:    sc.nextAttemptID(),
		chain: c,
		scope: sc,
		state: AttemptPrepared,
		rres:  rr,
		claim: claim,
		done:  make(chan struct{}),
	}
	c.atts = append(c.atts, a)
	if claim != nil && !c.hasClaimLocked(claim) {
		c.claims = append(c.claims, claim)
	}
	sc.inflight.Add(1)
	return a, nil
}

// Close 把链推进到 Closing→Closed。它不等待在途 Attempt：调用方用
// Scope.Wait 或 Attempt.Done 自行同步，避免在 Attempt goroutine 内死锁。
func (c *Chain) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == ChainClosed {
		return
	}
	c.state = ChainClosing
	c.state = ChainClosed
	for _, h := range c.claims {
		_ = h.Release()
	}
	c.claims = nil
	close(c.closed)
}

// Done 返回链关闭的只读通道。
func (c *Chain) Done() <-chan struct{} {
	if c == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.closed
}

// hasClaimLocked 判断同一 epoch 的句柄是否已登记，避免重复释放。调用方持锁。
func (c *Chain) hasClaimLocked(h *ClaimHandle) bool {
	for _, e := range c.claims {
		if e == h {
			return true
		}
	}
	return false
}

// Attempts 返回已注册 Attempt 的快照（诊断用）。
func (c *Chain) Attempts() []*Attempt {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Attempt, len(c.atts))
	copy(out, c.atts)
	return out
}

// AttemptState 是 Attempt 四态。
type AttemptState uint8

const (
	// AttemptPrepared 已登记，未越过发送提交边界。
	AttemptPrepared AttemptState = iota
	// AttemptDispatched 已越过发送提交边界，消耗 R 与 P。
	AttemptDispatched
	// AttemptAborted 取消或失败退出。
	AttemptAborted
	// AttemptCompleted 成功提交。
	AttemptCompleted
)

func (s AttemptState) String() string {
	switch s {
	case AttemptDispatched:
		return "dispatched"
	case AttemptAborted:
		return "aborted"
	case AttemptCompleted:
		return "completed"
	default:
		return "prepared"
	}
}

// Attempt 是一个逻辑候选执行单元。
type Attempt struct {
	id    uint64
	chain *Chain
	scope *Scope

	mu       sync.Mutex
	state    AttemptState
	rres     *RReservation
	claim    *ClaimHandle
	bodyDone bool
	at       time.Time
	done     chan struct{}
	doneOnce sync.Once
}

// ID 返回 Attempt 编号。
func (a *Attempt) ID() uint64 {
	if a == nil {
		return 0
	}
	return a.id
}

// State 返回当前状态。
func (a *Attempt) State() AttemptState {
	if a == nil {
		return AttemptAborted
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// ChainID 返回所属链。
func (a *Attempt) ChainID() string {
	if a == nil || a.chain == nil {
		return ""
	}
	return a.chain.id
}

// Done 返回 Attempt 终止信号。
func (a *Attempt) Done() <-chan struct{} {
	if a == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return a.done
}

// Dispatch 越过发送提交边界：提交 R、推进 Claim 到 Dispatched。
// 只允许 Prepared→Dispatched 一次；Claim 已失效时拒绝并保持 Prepared，
// 调用方必须 Abort 释放 R 预留。
func (a *Attempt) Dispatch() error {
	if a == nil {
		return ErrAttemptState
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != AttemptPrepared {
		return ErrAttemptState
	}
	if a.claim != nil {
		if err := a.claim.Dispatch(); err != nil {
			return err
		}
	}
	if a.rres != nil {
		a.rres.Commit()
	}
	a.state = AttemptDispatched
	a.at = a.scope.now()
	return nil
}

// Complete 把 Dispatched 推进到 Completed，并释放 Claim。
func (a *Attempt) Complete() error {
	if a == nil {
		return ErrAttemptState
	}
	a.mu.Lock()
	if a.state != AttemptDispatched {
		a.mu.Unlock()
		return ErrAttemptState
	}
	a.state = AttemptCompleted
	a.mu.Unlock()
	a.finish(true)
	return nil
}

// Abort 终止 Attempt。Prepared 阶段取消不消耗 R、不形成结果记录；
// Dispatched 阶段取消记为 Cancelled（阻止全 Miss），R 与 P 都不返还。
func (a *Attempt) Abort() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.state == AttemptAborted || a.state == AttemptCompleted {
		a.mu.Unlock()
		return
	}
	prev := a.state
	a.state = AttemptAborted
	a.mu.Unlock()
	if prev == AttemptPrepared && a.rres != nil {
		a.rres.Release()
	}
	if a.claim != nil && prev == AttemptPrepared {
		// Prepared 取消还没越过提交边界，可以让出 Claim 给同 Scope 的其他链。
		_ = a.claim.Release()
	}
	a.finish(prev == AttemptDispatched)
}

// CloseBody 幂等地关闭 Body 并标记已完成。返回是否由本次调用关闭。
// lease 释放必须发生在这里之后（T56），不能提前。
func (a *Attempt) CloseBody(closeFn func() error) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	if a.bodyDone {
		a.mu.Unlock()
		return false
	}
	a.bodyDone = true
	a.mu.Unlock()
	if closeFn != nil {
		_ = closeFn()
	}
	return true
}

// BodyClosed 报告 Body 是否已关闭。
func (a *Attempt) BodyClosed() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bodyDone
}

// finish 关闭终止信号并归还 Scope 在途计数；cancelled 用于结果账本。
func (a *Attempt) finish(cancelled bool) {
	a.doneOnce.Do(func() {
		close(a.done)
		if a.scope != nil {
			a.scope.inflight.Done()
		}
	})
	_ = cancelled
}
