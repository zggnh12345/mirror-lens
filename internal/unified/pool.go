package unified

import (
	"context"
	"sync"
)

// GlobalCapacity 按冻结公式计算全服务 RequestScope 并发容量：
//
//	G = N × max(W+1, C×A, 2) + 4
//
// N=200、W 默认 4（2~16）、C=2、A≤2，Preflight 独立池 4。默认 G=1004；
// W=16 时 G=3404。N 非法时回填 200，不允许变成「无限大」。
func GlobalCapacity(n, workers, c, a int) int {
	if n <= 0 {
		n = 200
	}
	if workers < 2 {
		workers = 4
	}
	if c <= 0 || c > 2 {
		// C=2 是冻结值，不可通过配置任意扩大（1.6 §B.7.1）；与 A 的上限同口径。
		c = 2
	}
	if a <= 0 || a > 2 {
		a = 2
	}
	base := workers + 1
	if ca := c * a; ca > base {
		base = ca
	}
	if base < 2 {
		base = 2
	}
	return n*base + 4
}

// Pool 是 RequestScope 级全局容量池。它与 limits 的外部入口限流是两层：
// limits.enabled=false 只关外部入口，内部仍按 N 硬上限准入（design §3.5）。
type Pool struct {
	mu   sync.Mutex
	sem  chan struct{}
	capN int
	used int
}

// NewPool 创建容量为 capacity 的池。
func NewPool(capacity int) *Pool {
	if capacity <= 0 {
		capacity = GlobalCapacity(200, 4, 2, 2)
	}
	return &Pool{sem: make(chan struct{}, capacity), capN: capacity}
}

// Capacity 返回总容量。
func (p *Pool) Capacity() int {
	if p == nil {
		return 0
	}
	return p.capN
}

// Acquire 取得一个名额；父 Context 取消时返回其错误。
func (p *Pool) Acquire(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryAcquire 非阻塞取得名额。
func (p *Pool) TryAcquire() bool {
	if p == nil {
		return true
	}
	select {
	case p.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release 归还名额。重复归还会阻塞，调用方必须只在 Acquire 成功后调用一次。
func (p *Pool) Release() {
	if p == nil {
		return
	}
	select {
	case <-p.sem:
	default:
	}
}

// InUse 返回当前占用数（诊断用）。
func (p *Pool) InUse() int {
	if p == nil {
		return 0
	}
	return len(p.sem)
}

// ---------------------------------------------------------------------------
// Lease 适配
// ---------------------------------------------------------------------------

// LeaseHandle 是底层并发所有权凭证的最小接口（scheduler.Lease 满足它）。
type LeaseHandle interface {
	Release() error
}

// LeaseAcquireFunc 把 unified 的申请语义映射到底层调度器，避免本包反向
// 依赖 scheduler。handover=true 表示交接（永不阻塞）。
type LeaseAcquireFunc func(ctx context.Context, sourceID int64, handover bool) (LeaseHandle, error)

// LeasePool 封装现有 scheduler.Lease 的申请/释放，统一记录 Scope 归属，
// 保证「每 Attempt 至多持有一份 Lease、释放幂等」。
type LeasePool struct {
	acquire LeaseAcquireFunc
	mu      sync.Mutex
	held    map[uint64]LeaseHandle
	seq     uint64
}

// NewLeasePool 用底层申请函数构造。
func NewLeasePool(fn LeaseAcquireFunc) *LeasePool {
	return &LeasePool{acquire: fn, held: map[uint64]LeaseHandle{}}
}

// Acquire 申请一份 Lease。底层返回 (nil, nil) 表示未启用配额，视为无锁成功。
func (p *LeasePool) Acquire(ctx context.Context, sourceID int64, handover bool) (uint64, LeaseHandle, error) {
	if p == nil || p.acquire == nil || sourceID == 0 {
		return 0, nil, nil
	}
	l, err := p.acquire(ctx, sourceID, handover)
	if err != nil {
		return 0, nil, err
	}
	if l == nil {
		return 0, nil, nil
	}
	p.mu.Lock()
	p.seq++
	id := p.seq
	p.held[id] = l
	p.mu.Unlock()
	return id, l, nil
}

// Release 幂等释放指定 Lease。
func (p *LeasePool) Release(id uint64) error {
	if p == nil || id == 0 {
		return nil
	}
	p.mu.Lock()
	l := p.held[id]
	delete(p.held, id)
	p.mu.Unlock()
	if l == nil {
		return nil
	}
	return l.Release()
}

// Held 返回当前持有数。
func (p *LeasePool) Held() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.held)
}
