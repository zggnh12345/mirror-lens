package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// Lease 是一条回源请求的并发所有权凭证。
//
// 设计见 docs/concurrency.md：
//   - 计数单位是所有权，不是 TCP 连接，也不是客户端任务；
//   - Reserved 在拨号之前取得，覆盖建连与等待响应头；
//   - 同源接管用 Transfer 转移所有权，计数不变；
//   - 旧 Body 退出由独立引用计数确认（I8），Generation 只负责“谁能操作”。

// LeaseKind 决定这次申请走哪一级准入。
type LeaseKind uint8

const (
	// KindNormal 是日常派发，受 dispatch_cap 约束。
	KindNormal LeaseKind = iota
	// KindHandover 是交接（换源/异源接管），受 hard_max 约束，且永不阻塞。
	KindHandover
)

func (k LeaseKind) String() string {
	if k == KindHandover {
		return "handover"
	}
	return "normal"
}

// LeaseState 是 Lease 的生命周期状态。
type LeaseState uint8

const (
	StateReserved LeaseState = iota
	StateActive
	StateReclaiming
	StateReleased
)

func (s LeaseState) String() string {
	switch s {
	case StateReserved:
		return "reserved"
	case StateActive:
		return "active"
	case StateReclaiming:
		return "reclaiming"
	default:
		return "released"
	}
}

var (
	// ErrLeaseAtCap 表示名额已满且本次准入不允许排队。
	ErrLeaseAtCap = errors.New("lease: source at capacity")
	// ErrLeaseWaitTimeout 表示有限等待超时。
	ErrLeaseWaitTimeout = errors.New("lease: wait timeout")
	// ErrLeaseStale 表示 token 已过期（发生过 Transfer）。
	ErrLeaseStale = errors.New("lease: stale token")
	// ErrLeaseState 表示当前状态不允许该操作。
	ErrLeaseState = errors.New("lease: invalid state")
)

// Token 是不可变的租约凭证。Transfer 后旧 Token 立即失效。
type Token struct {
	ID  uint64
	Gen uint64
}

// Lease 由 Manager 创建，调用方只能通过方法操作。
type Lease struct {
	ID       uint64
	SourceID int64
	Kind     LeaseKind

	set *leaseSet

	mu     sync.Mutex
	state  LeaseState
	st     atomic.Int32 // state 的原子镜像，供指标快照读取，避免锁序反转
	gen    uint64
	bodies int
	cancel map[int]context.CancelFunc
	nx     int
}

// Token 返回当前所有权凭证。
func (l *Lease) Token() Token {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Token{ID: l.ID, Gen: l.gen}
}

// State 返回当前状态。
func (l *Lease) State() LeaseState {
	return LeaseState(l.st.Load())
}

// setState 同时更新字段与原子镜像；调用方持 l.mu。
func (l *Lease) setState(st LeaseState) {
	l.state = st
	l.st.Store(int32(st))
}

// BodyRefs 返回尚未退出的 Body 引用数。
func (l *Lease) BodyRefs() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bodies
}

// BindCancel 登记一个 attempt 级取消函数；Lease 被回收时会全部调用。
// 返回的函数用于解绑（attempt 正常结束时调用）。
func (l *Lease) BindCancel(fn context.CancelFunc) func() {
	if fn == nil {
		return func() {}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == StateReclaiming {
		fn()
		return func() {}
	}
	if l.cancel == nil {
		l.cancel = map[int]context.CancelFunc{}
	}
	id := l.nx
	l.nx++
	l.cancel[id] = fn
	return func() {
		l.mu.Lock()
		delete(l.cancel, id)
		l.mu.Unlock()
	}
}

// Activate 把 Reserved 转成 Active，并登记一个 Body 引用。
// 返回的 retire 必须在 Body 真正退出（读取协程结束）后调用，可重复调用。
func (l *Lease) Activate() (func(), error) {
	l.mu.Lock()
	// Active 上再次 Activate 是同源接管的第二阶段：同一 Lease 两个 Body。
	if l.state != StateReserved && l.state != StateActive {
		st := l.state
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: activate from %s", ErrLeaseState, st)
	}
	l.setState(StateActive)
	l.bodies++
	l.mu.Unlock()

	l.set.addBody(l.SourceID, 1)
	l.set.publish()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.set.addBody(l.SourceID, -1)
			l.mu.Lock()
			l.bodies--
			done := l.bodies == 0
			// Reclaiming 表示 owner 已走但 Body 还没退；最后一个 Body 退出才归还名额。
			final := l.state == StateReleased || l.state == StateReclaiming
			l.mu.Unlock()
			if done && final {
				l.finalize()
			}
		})
	}, nil
}

// Transfer 把所有权交给新的 attempt（同源接管）。旧 Token 随即失效。
func (l *Lease) Transfer(tok Token) (Token, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if tok.ID != l.ID || tok.Gen != l.gen {
		return Token{}, ErrLeaseStale
	}
	if l.state != StateActive {
		return Token{}, fmt.Errorf("%w: transfer from %s", ErrLeaseState, l.state)
	}
	l.gen++
	metrics.LeaseTransfer(l.SourceID, true)
	l.mu.Unlock()
	l.set.publish()
	l.mu.Lock()
	return Token{ID: l.ID, Gen: l.gen}, nil
}

// Release 释放所有权。Body 尚未退出时只解绑 owner，名额保留到最后一个 Body 退出。
func (l *Lease) Release(tok Token) {
	l.mu.Lock()
	if tok.ID != l.ID || l.state == StateReleased {
		l.mu.Unlock()
		return
	}
	if tok.Gen != l.gen {
		// 旧 Owner 的 Body 退出：不能动当前 Owner 的状态，
		// 但如果这是最后一个 Body，名额必须在这里归还（I8），否则漏账。
		if l.bodies > 0 {
			l.mu.Unlock()
			return
		}
		l.setState(StateReleased)
		l.mu.Unlock()
		l.finalize()
		return
	}
	l.setState(StateReleased)
	pending := l.bodies > 0
	if pending {
		// 还有 Body 没退出：状态对外呈现为 Reclaiming，名额继续占用（I4/I8）。
		l.setState(StateReclaiming)
	}
	l.mu.Unlock()

	if pending {
		return
	}
	l.finalize()
}

// Reclaim 取消该 Lease 关联的全部 attempt，并保持名额直到 Body 退出（I4）。
func (l *Lease) Reclaim(reason string) bool {
	l.mu.Lock()
	if l.state == StateReleased {
		l.mu.Unlock()
		return false
	}
	l.setState(StateReclaiming)
	fns := make([]context.CancelFunc, 0, len(l.cancel))
	for _, fn := range l.cancel {
		fns = append(fns, fn)
	}
	l.cancel = nil
	empty := l.bodies == 0
	l.mu.Unlock()

	for _, fn := range fns {
		fn()
	}
	if l.set.isDead() {
		return true
	}
	metrics.LeaseReclaim(l.SourceID, reason, empty)
	if empty {
		l.finalize()
	}
	return true
}

// finalize 做真正的名额归还，只会执行一次。
func (l *Lease) finalize() {
	if !l.set.drop(l) {
		return
	}
	if l.set.isDead() {
		return
	}
	metrics.LeaseReleased(l.SourceID, l.Kind == KindHandover)
}

// leaseSet 是一台源的租约集合。
type leaseSet struct {
	id       int64
	dead     bool // 源已删除：停止导出指标，避免迟到回调复活序列
	hardMax  int
	dispatch int

	mu     sync.Mutex
	leases map[uint64]*Lease
	held   int
	bodies int
	nextID uint64
	waitq  []*waiter

	peak int
}

type waiter struct {
	kind LeaseKind
	ch   chan *Lease
}

func newLeaseSet(id int64, hardMax, dispatch int) *leaseSet {
	if hardMax < 1 {
		hardMax = 1
	}
	if dispatch < 1 {
		dispatch = 1
	}
	if dispatch > hardMax {
		dispatch = hardMax
	}
	return &leaseSet{id: id, hardMax: hardMax, dispatch: dispatch, leases: map[uint64]*Lease{}}
}

// admitLocked 判断当前能否放行；调用方持锁。
func (s *leaseSet) admitLocked(kind LeaseKind) bool {
	if kind == KindHandover {
		return s.held < s.hardMax
	}
	return s.held < s.dispatch
}

func (s *leaseSet) newLeaseLocked(kind LeaseKind) *Lease {
	s.nextID++
	l := &Lease{ID: s.nextID, SourceID: s.id, Kind: kind, set: s, state: StateReserved}
	l.st.Store(int32(StateReserved))
	s.leases[l.ID] = l
	s.held++
	if s.held > s.peak {
		s.peak = s.held
	}
	return l
}

// acquire 是准入主路径。observe 模式不阻塞、不拒绝。
func (s *leaseSet) acquire(ctx context.Context, kind LeaseKind, mode string, wait time.Duration) (*Lease, error) {
	s.mu.Lock()
	if mode == "observe" || s.admitLocked(kind) {
		l := s.newLeaseLocked(kind)
		hit := mode == "observe" && !s.admitLocked(kind)
		s.mu.Unlock()
		if hit {
			metrics.LeaseCapHit(s.id, kind == KindHandover)
		}
		s.mu.Lock()
		s.publishLocked()
		s.mu.Unlock()
		return l, nil
	}
	s.mu.Unlock()

	// handover 永不排队（I6）：拿不到硬顶名额就放弃。
	if kind == KindHandover || mode == "non_blocking" {
		metrics.LeaseCapHit(s.id, kind == KindHandover)
		return nil, ErrLeaseAtCap
	}

	w := &waiter{kind: kind, ch: make(chan *Lease, 1)}
	s.mu.Lock()
	// 入队前再试一次，避免释放瞬间的空档。
	if s.admitLocked(kind) {
		l := s.newLeaseLocked(kind)
		s.publishLocked()
		s.mu.Unlock()
		return l, nil
	}
	s.waitq = append(s.waitq, w)
	s.mu.Unlock()

	start := time.Now()
	var timeout <-chan time.Time
	if mode == "wait_then_fallback" {
		if wait <= 0 {
			wait = 2 * time.Second
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		timeout = t.C
	}

	select {
	case l := <-w.ch:
		metrics.LeaseWait(s.id, time.Since(start))
		return l, nil
	case <-timeout:
		if l := s.cancelWaiter(w); l != nil {
			// 刚好在超时边界被交接：不能把名额漏在账上。
			l.Release(l.Token())
		}
		metrics.LeaseWaitTimeout(s.id)
		return nil, ErrLeaseWaitTimeout
	case <-ctx.Done():
		if l := s.cancelWaiter(w); l != nil {
			l.Release(l.Token())
		}
		return nil, ctx.Err()
	}
}

// cancelWaiter 标记取消，并回收可能已经交接过来的 Lease。
// 只标记不搬队列：grant 遇到已取消的 waiter 直接跳过。
func (s *leaseSet) cancelWaiter(w *waiter) *Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.kind = 255 // 已取消
	select {
	case l := <-w.ch:
		return l
	default:
		return nil
	}
}

func (s *leaseSet) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

// addBody 调整 Body 计数。
func (s *leaseSet) addBody(id int64, delta int) {
	s.mu.Lock()
	s.bodies += delta
	if s.bodies < 0 {
		s.bodies = 0
	}
	s.publishLocked()
	s.mu.Unlock()
}

// drop 归还名额并做一次直接交接（FIFO，只唤醒队首可放行者）。
func (s *leaseSet) drop(l *Lease) bool {
	s.mu.Lock()
	if _, ok := s.leases[l.ID]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.leases, l.ID)
	if s.held > 0 {
		s.held--
	}
	metrics.LeaseStuckCleared(s.id)
	l.mu.Lock()
	l.setState(StateReleased)
	l.mu.Unlock()

	for len(s.waitq) > 0 {
		w := s.waitq[0]
		s.waitq = s.waitq[1:]
		if w.kind > 1 {
			continue // 已取消
		}
		if !s.admitLocked(w.kind) {
			// 队首等不到就放回队首，保持 FIFO 且不越级放行。
			s.waitq = append([]*waiter{w}, s.waitq...)
			break
		}
		nl := s.newLeaseLocked(w.kind)
		w.ch <- nl
		break
	}
	s.publishLocked()
	s.mu.Unlock()
	return true
}

// publish 在不持 set 锁时刷新指标。
func (s *leaseSet) publish() {
	s.mu.Lock()
	s.publishLocked()
	s.mu.Unlock()
}

// publishLocked 把当前分项推到指标；调用方持锁。
func (s *leaseSet) publishLocked() {
	if s.dead {
		return
	}
	var reserved, active, reclaiming int
	for _, l := range s.leases {
		switch l.State() {
		case StateReserved:
			reserved++
		case StateActive:
			active++
		case StateReclaiming:
			reclaiming++
		}
	}
	metrics.LeaseGauge(s.id, s.held, reserved, active, reclaiming, s.bodies)
}

// snapshot 返回同一把锁内的一致性快照（I1）。
func (s *leaseSet) snapshot() (held, reserved, active, reclaiming, bodies int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held = s.held
	bodies = s.bodies
	for _, l := range s.leases {
		switch l.State() {
		case StateReserved:
			reserved++
		case StateActive:
			active++
		case StateReclaiming:
			reclaiming++
		}
	}
	return
}

// leaseManager 为每台源维护一个 leaseSet。
type leaseManager struct {
	mu   sync.Mutex
	sets map[int64]*leaseSet
}

func newLeaseManager() *leaseManager {
	return &leaseManager{sets: map[int64]*leaseSet{}}
}

func (m *leaseManager) get(id int64, hardMax, dispatch int) *leaseSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sets[id]; ok {
		s.mu.Lock()
		s.hardMax, s.dispatch = hardMax, dispatch
		s.mu.Unlock()
		return s
	}
	s := newLeaseSet(id, hardMax, dispatch)
	m.sets[id] = s
	return s
}

func (m *leaseManager) forget(id int64) {
	m.mu.Lock()
	if s, ok := m.sets[id]; ok {
		s.mu.Lock()
		s.dead = true
		s.mu.Unlock()
	}
	delete(m.sets, id)
	m.mu.Unlock()
}

// caps 计算三级上限：hard_max / capacity_limit（3.0 固定）/ dispatch_cap。
func (c Config) caps() (hard, dispatch int) {
	hard = c.MaxPerSource
	if hard <= 0 {
		hard = 4
	}
	head := c.Headroom
	if head <= 0 || head > 1 {
		head = 0.8
	}
	dispatch = int(math.Floor(float64(hard) * head))
	if dispatch < 1 {
		dispatch = 1
	}
	if hard > 1 && dispatch > hard-1 {
		dispatch = hard - 1
	}
	if dispatch > hard {
		dispatch = hard
	}
	return hard, dispatch
}
