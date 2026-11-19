package unified

import (
	"sync"
	"sync/atomic"
	"time"
)

// Identity 是 Claim 的身份键：规范化逻辑 Registry + Repository + Manifest 身份。
// 认证上下文不参与身份等价（design §2.2）；Manifest 内含 Ref 与内容协商，
// 单看 Host/Repo/Ref 不足以判等价。
type Identity struct {
	Registry   string
	Repository string
	Manifest   ManifestIdentity
}

// Empty 报告身份是否完整。不完整的身份不允许参与 Claim。
func (i Identity) Empty() bool {
	return i.Registry == "" || i.Repository == "" || i.Manifest.Empty()
}

// ClaimState 是 Claim 三态。
type ClaimState uint8

const (
	// ClaimReserved 表示已预占但尚未越过发送提交边界。
	ClaimReserved ClaimState = iota
	// ClaimDispatched 表示至少一个 Attempt 已越过发送提交边界。
	// 该状态不可被另一条 Chain 接管，也不回退为 Reserved。
	ClaimDispatched
	// ClaimReleased 表示完成/失败/取消后释放，可由后续 Chain 重新预占。
	ClaimReleased
)

func (s ClaimState) String() string {
	switch s {
	case ClaimReserved:
		return "reserved"
	case ClaimDispatched:
		return "dispatched"
	default:
		return "released"
	}
}

// claimEntry 是某个 Identity 在 Scope 内的唯一占位记录。
type claimEntry struct {
	state ClaimState
	owner string
	epoch uint64
	at    time.Time
}

// claimStore 是 Scope 内的 Claim 表。
type claimStore struct {
	mu  sync.Mutex
	m   map[Identity]*claimEntry
	now func() time.Time
}

func newClaimStore(now func() time.Time) *claimStore {
	if now == nil {
		now = time.Now
	}
	return &claimStore{m: map[Identity]*claimEntry{}, now: now}
}

// ClaimHandle 是一次预占的凭证。同 Chain 的多个 Attempt 共享同一个 handle
// 语义：它们都拿到同一 epoch 的句柄，Dispatch 幂等。
type ClaimHandle struct {
	store *claimStore
	key   Identity
	owner string
	epoch uint64
	state atomic.Int32
}

// Acquire 为 chainID 预占 identity。
//
//   - 表内无记录：新建 Reserved；
//   - 已 Released：由本 Chain 重新预占并递增 epoch（旧句柄失效）；
//   - 同 Chain 持有：返回同 epoch 句柄，多个 Attempt 可共享；
//   - 另一 Chain 持有（Reserved 或 Dispatched）：拒绝，绝不抢占。
func (s *claimStore) Acquire(chainID string, key Identity) (*ClaimHandle, error) {
	if key.Empty() {
		return nil, ErrClaimConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[key]
	if e == nil {
		e = &claimEntry{state: ClaimReserved, owner: chainID, epoch: 1, at: s.now()}
		s.m[key] = e
		return newClaimHandle(s, key, chainID, e.epoch, ClaimReserved), nil
	}
	if e.state == ClaimReleased {
		// 释放后的 Identity 可以由任何链（含原持有者）重新预占；
		// 每次重新预占都换 epoch，旧句柄一律失效。
		e.state = ClaimReserved
		e.owner = chainID
		e.epoch++
		e.at = s.now()
		return newClaimHandle(s, key, chainID, e.epoch, ClaimReserved), nil
	}
	if e.owner != chainID {
		return nil, ErrClaimConflict
	}
	// 同 Chain：共享同一 epoch 与状态。多个 Attempt 可并发使用。
	return newClaimHandle(s, key, chainID, e.epoch, e.state), nil
}

func newClaimHandle(s *claimStore, key Identity, owner string, epoch uint64, st ClaimState) *ClaimHandle {
	h := &ClaimHandle{store: s, key: key, owner: owner, epoch: epoch}
	h.state.Store(int32(st))
	return h
}

// State 返回句柄当前的声明状态（缓存值，权威值在 store 内）。
func (h *ClaimHandle) State() ClaimState {
	if h == nil {
		return ClaimReleased
	}
	return ClaimState(h.state.Load())
}

// Identity 返回句柄对应的身份。
func (h *ClaimHandle) Identity() Identity {
	if h == nil {
		return Identity{}
	}
	return h.key
}

// ChainID 返回持有者。
func (h *ClaimHandle) ChainID() string {
	if h == nil {
		return ""
	}
	return h.owner
}

// Dispatch 把 Claim 推进到 Dispatched。已 Dispatched 时幂等返回 nil；
// 句柄过期（epoch 变了）或已被释放时返回 ErrClaimReleased。
func (h *ClaimHandle) Dispatch() error {
	if h == nil {
		return ErrClaimReleased
	}
	s := h.store
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[h.key]
	if e == nil || e.epoch != h.epoch || e.owner != h.owner {
		return ErrClaimReleased
	}
	if e.state == ClaimReleased {
		return ErrClaimReleased
	}
	if e.state == ClaimDispatched {
		h.state.Store(int32(ClaimDispatched))
		return nil
	}
	e.state = ClaimDispatched
	e.at = s.now()
	h.state.Store(int32(ClaimDispatched))
	return nil
}

// Release 释放 Claim。
//
// 冻结（F19）：只有从未 Dispatched 的 Claim 允许释放；一旦有 Attempt 越过
// 发送提交边界，Claim 必须永久保持 Dispatched（直到 Scope 关闭），不得被
// 其他链重新取得，也不因「A 竞速阶段结束」而释放。重复释放幂等。
func (h *ClaimHandle) Release() error {
	if h == nil {
		return nil
	}
	s := h.store
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[h.key]
	if e == nil || e.epoch != h.epoch || e.owner != h.owner {
		return ErrClaimReleased
	}
	switch e.state {
	case ClaimDispatched:
		// 已越过提交边界：拒绝释放，状态保持 Dispatched。
		return ErrClaimDispatched
	case ClaimReleased:
		h.state.Store(int32(ClaimReleased))
		return nil
	}
	e.state = ClaimReleased
	e.at = s.now()
	h.state.Store(int32(ClaimReleased))
	return nil
}

// snapshot 用于测试与诊断：返回当前表内状态。
func (s *claimStore) snapshot() map[Identity]ClaimState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Identity]ClaimState, len(s.m))
	for k, v := range s.m {
		out[k] = v.state
	}
	return out
}

// dispatchedCount 统计 Dispatched 的 Identity 数，用于「零重复 Dispatched」断言。
func (s *claimStore) dispatchedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.m {
		if v.state == ClaimDispatched {
			n++
		}
	}
	return n
}
