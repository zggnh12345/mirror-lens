package store

import (
	"sync"
	"sync/atomic"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// GuardKey 是候选屏蔽的键：源 + 协议族 + 规范化 scope（Registry 为 host）。
// 不含 gen：同一把锁要能跨代次延续，代次只用于「谁能解除」的判定。
type GuardKey struct {
	SourceID int64
	Family   string
	Scope    string
}

// GuardToken 是 Block 产生的解除凭据。Gen 与 Seq 必须同时匹配当前条目，
// 旧探测的 token 不得解除后来更新过的屏蔽。
type GuardToken struct {
	Gen int64
	Seq uint64
}

type guardEntry struct {
	gen int64
	seq uint64
}

// RecoveryGuard 是进程内的「写库失败即关闭」集合，不是第二套 recovery
// 状态机：不参与六态派生、不持久化、不改 registry_host_recovery.state。
//
// 它只回答一个问题：某个 (source, family, scope) 最近一次探测结果是否
// 因为写库失败而无法确认——无法确认就不许进候选（fail-closed）。
type RecoveryGuard struct {
	mu      sync.RWMutex
	blocked map[GuardKey]guardEntry
	// epoch 在每次 Block / Unblock 时递增，给 Filter 提供线性化点。
	epoch uint64
	seq   atomic.Uint64

	exhausted atomic.Uint64 // guard_filter_retry_exhausted_total
}

func NewRecoveryGuard() *RecoveryGuard {
	return &RecoveryGuard{blocked: make(map[GuardKey]guardEntry)}
}

// Block 屏蔽一个键并返回本轮 token。多次 Block 会覆盖旧条目：
// 后到的屏蔽代表更新的失败事实，旧 token 因此自动失效。
func (g *RecoveryGuard) Block(k GuardKey, gen int64) GuardToken {
	// seq 的分配与 map 写入必须同处一个同步边界：否则并发的两次 Block
	// 可能「先分配的被后写入」，让更早的失败覆盖更新的失败事实。
	g.mu.Lock()
	seq := g.seq.Add(1)
	g.blocked[k] = guardEntry{gen: gen, seq: seq}
	g.epoch++
	g.mu.Unlock()
	return GuardToken{Gen: gen, Seq: seq}
}

// UnblockIfMatch 只在当前条目的 (gen, seq) 与 token 完全一致时解除。
// 旧代次/旧序号的清理不得误删新条目。
func (g *RecoveryGuard) UnblockIfMatch(k GuardKey, t GuardToken) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.blocked[k]
	if !ok || e.gen != t.Gen || e.seq != t.Seq {
		return false
	}
	delete(g.blocked, k)
	g.epoch++
	return true
}

// Blocked 只用于诊断与测试；候选路径必须走 FilterByGuard 的原子快照。
func (g *RecoveryGuard) Blocked(k GuardKey) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.blocked[k]
	return ok
}

func (g *RecoveryGuard) snapshot() (map[GuardKey]struct{}, uint64) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	snap := make(map[GuardKey]struct{}, len(g.blocked))
	for k := range g.blocked {
		snap[k] = struct{}{}
	}
	return snap, g.epoch
}

func (g *RecoveryGuard) verify(epoch uint64) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.epoch == epoch
}

// Exhausted 返回 Filter 重试耗尽的累计次数（观测用）。
func (g *RecoveryGuard) Exhausted() uint64 { return g.exhausted.Load() }

// FilterByGuard 用一次原子快照过滤整个列表，返回独立结果，不修改输入
// 的 slice 及其底层数组（调度器缓存保留的正是原始列表）。
//
// 协议：Snapshot → 过滤 → Verify。epoch 变化说明期间有 Block / Unblock，
// 本次结果不能作为线性化点，整体重试；三次耗尽返回空集（fail-closed），
// 绝不退化为未过滤的原始列表。
//
// g 为 nil 时原样返回：只允许单元测试省略 Guard，生产装配点必须注入。
func FilterByGuard[T any](g *RecoveryGuard, list []T, key func(T) GuardKey) []T {
	if g == nil {
		return list
	}
	for attempt := 0; attempt < 3; attempt++ {
		snap, epoch := g.snapshot()
		out := make([]T, 0, len(list))
		for _, v := range list {
			if _, bad := snap[key(v)]; !bad {
				out = append(out, v)
			}
		}
		if g.verify(epoch) {
			return out
		}
	}
	g.exhausted.Add(1)
	metrics.GuardFilterExhausted()
	return []T{}
}
