package unified

import (
	"context"
	"sync"
	"time"
)

// VoteWindow 是 B 层 Manifest Digest 投票的默认窗口（AUD-04-4 冻结）。
const VoteWindow = 200 * time.Millisecond

// VoteState 是投票状态机的状态。
type VoteState uint8

const (
	// VoteCollecting 表示窗口内还在收集有效票。
	VoteCollecting VoteState = iota
	// VoteConfirmed 表示至少两票来自不同逻辑 Registry 且不同 source_id，Digest 一致。
	VoteConfirmed
	// VoteMismatch 表示出现 Digest 不一致；不确认，继续顺序补位。
	VoteMismatch
	// VoteAuthorityCompare 表示窗口关闭时只有一个有效候选，需要一次 Authority 比对。
	VoteAuthorityCompare
	// VoteWindowClosed 表示窗口已关闭且无有效候选。
	VoteWindowClosed
)

func (s VoteState) String() string {
	switch s {
	case VoteConfirmed:
		return "confirmed"
	case VoteMismatch:
		return "mismatch"
	case VoteAuthorityCompare:
		return "authority_compare"
	case VoteWindowClosed:
		return "window_closed"
	default:
		return "collecting"
	}
}

// VoteCandidate 是一张票。只有正文已完整读取且 Digest 已校验才构成有效票；
// 仅响应头 Docker-Content-Digest 不算。
type VoteCandidate struct {
	Registry string
	SourceID int64
	Digest   string
	Verified bool
	Complete bool
}

type voteKey struct {
	registry string
	sourceID int64
}

// ManifestVote 是 200ms Digest 投票状态机。它独立于响应头竞速与 Claim：
// 投票只决定 Manifest 身份可信度，不决定 Claim 提交资格（design §4.3）。
type ManifestVote struct {
	mu       sync.Mutex
	window   time.Duration
	now      func() time.Time
	state    VoteState
	started  bool
	deadline time.Time
	votes    []VoteCandidate
	seen     map[voteKey]bool
	agreed   string
	timer    *time.Timer
	onClose  func(VoteState)
}

// NewManifestVote 创建投票器。window<=0 用冻结默认 200ms。
func NewManifestVote(window time.Duration) *ManifestVote {
	if window <= 0 {
		window = VoteWindow
	}
	return &ManifestVote{
		window: window,
		now:    time.Now,
		state:  VoteCollecting,
		seen:   map[voteKey]bool{},
	}
}

// WithClock 注入测试时钟（只影响 deadline 读数，不影响真实计时器）。
func (v *ManifestVote) WithClock(now func() time.Time) *ManifestVote {
	if now != nil {
		v.now = now
	}
	return v
}

// OnClose 注册窗口关闭回调（幂等覆盖）。
func (v *ManifestVote) OnClose(fn func(VoteState)) {
	v.mu.Lock()
	v.onClose = fn
	v.mu.Unlock()
}

// Window 返回窗口长度。
func (v *ManifestVote) Window() time.Duration { return v.window }

// State 返回当前状态。
func (v *ManifestVote) State() VoteState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

// Agreed 返回已确认的 Digest（未确认时为空）。
func (v *ManifestVote) Agreed() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.agreed
}

// Votes 返回接受的有效票数。
func (v *ManifestVote) Votes() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.votes)
}

// Offer 提交一张票。返回当前状态与该票是否被接受。
//
// 拒绝条件：正文未完整读取、Digest 未校验、窗口已关闭、同一 (registry, source)
// 重复投票。同 Digest 且满足「不同逻辑 Registry + 不同 source_id」的第二票
// 立即确认；Digest 不一致则进入 Mismatch 并等待顺序补位，不确认。
func (v *ManifestVote) Offer(c VoteCandidate) (VoteState, bool) {
	if v == nil {
		return VoteWindowClosed, false
	}
	if !c.Verified || !c.Complete || c.Digest == "" || c.Registry == "" {
		return v.State(), false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != VoteCollecting {
		// 窗口外/已锁定：只丢弃，不重开窗口、不改结论。
		return v.state, false
	}
	key := voteKey{registry: c.Registry, sourceID: c.SourceID}
	if v.seen[key] {
		// 同一 Source 不能通过别名贡献两票。
		return v.state, false
	}
	if len(v.votes) == 0 {
		v.seen[key] = true
		v.votes = append(v.votes, c)
		v.agreed = c.Digest
		v.startLocked()
		return v.state, true
	}
	distinctRegistry := true
	distinctSource := true
	for _, e := range v.votes {
		if e.Registry == c.Registry {
			distinctRegistry = false
		}
		if e.SourceID == c.SourceID {
			distinctSource = false
		}
	}
	if !distinctRegistry || !distinctSource {
		// 同一侧（同 Registry 或同 Source）的第二个观察不是独立票。
		return v.state, false
	}
	v.seen[key] = true
	v.votes = append(v.votes, c)
	if c.Digest != v.agreed {
		v.state = VoteMismatch
		v.agreed = ""
		return v.state, true
	}
	v.state = VoteConfirmed
	return v.state, true
}

// startLocked 启动窗口计时。调用方持锁。
func (v *ManifestVote) startLocked() {
	if v.started {
		return
	}
	v.started = true
	v.deadline = v.now().Add(v.window)
	v.timer = time.AfterFunc(v.window, func() { v.CloseWindow() })
}

// CloseWindow 关闭窗口并锁定结论。可重复调用，只有第一次生效。
func (v *ManifestVote) CloseWindow() VoteState {
	if v == nil {
		return VoteWindowClosed
	}
	v.mu.Lock()
	if !v.started {
		// 从未开窗（零有效候选）：直接锁定为空结论。
		v.started = true
		v.state = VoteWindowClosed
		v.mu.Unlock()
		return VoteWindowClosed
	}
	if v.state == VoteConfirmed || v.state == VoteMismatch || v.state == VoteWindowClosed || v.state == VoteAuthorityCompare {
		st := v.state
		v.mu.Unlock()
		return st
	}
	switch {
	case len(v.votes) == 0:
		v.state = VoteWindowClosed
	case len(v.votes) == 1:
		v.state = VoteAuthorityCompare
	default:
		// 多票在这里必然已一致（不一致在 Offer 时已转 Mismatch）。
		v.state = VoteConfirmed
	}
	st := v.state
	timer := v.timer
	cb := v.onClose
	v.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if cb != nil {
		cb(st)
	}
	return st
}

// Deadline 返回窗口截止时间（未开窗时为零值）。
func (v *ManifestVote) Deadline() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.deadline
}

// ---------------------------------------------------------------------------
// Authority：单候选软比对 + 最终兜底，只允许一次 Attempt
// ---------------------------------------------------------------------------

// AuthorityResult 是 Authority 那一次请求的结果。
type AuthorityResult struct {
	Registry string
	SourceID int64
	Digest   string
	Verified bool
}

// Authority 保证同一 RequestScope 内最多执行一次权威请求，软比对与最终
// 兜底复用同一份结果（design §4.4）。
type Authority struct {
	mu       sync.Mutex
	started  bool
	done     chan struct{}
	res      AuthorityResult
	err      error
	attempts int
}

// NewAuthority 创建一次性 Authority。
func NewAuthority() *Authority {
	return &Authority{done: make(chan struct{})}
}

// Acquire 尝试取得执行权。返回 true 者负责执行并调用 Finish；并发/后续
// 调用者返回 false，应使用 Await 复用结果。
func (a *Authority) Acquire() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return false
	}
	a.started = true
	a.attempts = 1
	return true
}

// Finish 提交结果并放行 Await。重复调用只保留第一次。
func (a *Authority) Finish(res AuthorityResult, err error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.isDone() {
		a.mu.Unlock()
		return
	}
	a.res = res
	a.err = err
	close(a.done)
	a.mu.Unlock()
}

func (a *Authority) isDone() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

// Await 等待并返回 Authority 结果；ctx 取消时返回其错误。
func (a *Authority) Await(ctx context.Context) (AuthorityResult, error) {
	if a == nil {
		return AuthorityResult{}, nil
	}
	select {
	case <-a.done:
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.res, a.err
	case <-ctx.Done():
		return AuthorityResult{}, ctx.Err()
	}
}

// Attempts 返回已发起的 Authority 请求次数（恒为 0 或 1）。
func (a *Authority) Attempts() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attempts
}

// Compare 是一次软比对：候选 Digest 与 Authority Digest 相同即确认。
// 它不发起第二次请求，只读已有结果。
func (a *Authority) Compare(ctx context.Context, candidateDigest string) (bool, error) {
	res, err := a.Await(ctx)
	if err != nil {
		return false, err
	}
	if !res.Verified || res.Digest == "" {
		return false, nil
	}
	return res.Digest == candidateDigest, nil
}
