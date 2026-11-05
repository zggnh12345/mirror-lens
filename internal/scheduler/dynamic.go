package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// 动态带宽调度：并发上限由实测吞吐决定，而不是拍一个固定值。
//
//   - 排队且吞吐没退步 → +1；吞吐掉 40% → -1；失败/429/503 → 减半。
//   - 只有实测证据才动上限，没样本就保持原值。

// gate 是一台源的并发闸门，容量可在运行中调整。
// 不用带缓冲 channel 是因为 cap 建好就固定；这里靠关闭并重建 wait
// 通道广播唤醒，ctx 取消也能立刻退出。
type gate struct {
	id       int64
	mu       sync.Mutex
	inflight int
	limit    int
	max      int
	wait     chan struct{}
	// queued 记录因满员而等待的次数，Observe 据此判断还有没有扩容空间。
	queued int
	// P0 观测专用：只读累计，不参与任何调度判断。
	start time.Time
	since time.Time
	area  float64 // 并发数对时间的积分（并发·秒）
}

func newGate(id int64, limit, max int) *gate {
	if limit < 1 {
		limit = 1
	}
	if max < limit {
		max = limit
	}
	now := time.Now()
	return &gate{id: id, limit: limit, max: max, wait: make(chan struct{}), start: now, since: now}
}

// settle 把上次结算以来的并发面积累加进 area，调用方持锁。
func (g *gate) settle() {
	now := time.Now()
	if !g.since.IsZero() {
		g.area += float64(g.inflight) * now.Sub(g.since).Seconds()
	}
	g.since = now
}

// avg 返回自闸门创建以来并发占用的时间加权均值，调用方持锁。
func (g *gate) avg() float64 {
	if g.start.IsZero() {
		return 0
	}
	elapsed := time.Since(g.start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return (g.area + float64(g.inflight)*time.Since(g.since).Seconds()) / elapsed
}

func (g *gate) acquire(ctx context.Context) (func(), error) {
	var waitStart time.Time
	for {
		g.mu.Lock()
		if g.inflight < g.limit {
			g.settle()
			g.inflight++
			avg := g.avg()
			g.mu.Unlock()
			if !waitStart.IsZero() {
				metrics.SourceGateWait(g.id, time.Since(waitStart))
			}
			metrics.SourceInflightAvg(g.id, avg)
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					g.settle()
					g.inflight--
					close(g.wait)
					g.wait = make(chan struct{})
					avg := g.avg()
					g.mu.Unlock()
					metrics.SourceInflightAvg(g.id, avg)
				})
			}, nil
		}
		if waitStart.IsZero() {
			waitStart = time.Now()
		}
		g.queued++
		ch := g.wait
		g.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			// 排过队就得撤掉计数，否则 queued 只增不减，
			// 面板和调度会一直以为有人在排队。
			g.mu.Lock()
			if g.queued > 0 {
				g.queued--
			}
			g.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

func (g *gate) setLimit(n int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n < 1 {
		n = 1
	}
	if n > g.max {
		n = g.max
	}
	if n == g.limit {
		return g.limit
	}
	g.limit = n
	close(g.wait)
	g.wait = make(chan struct{})
	return n
}

func (g *gate) current() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// takeQueued 返回并清零「排队过」标记。
func (g *gate) takeQueued() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := g.queued > 0
	g.queued = 0
	return q
}

// dynState 是每台源的吞吐滑动平均。
type dynState struct {
	rate float64 // 字节/秒，EWMA
	last time.Time
}

// Observe 用一次真实传输的结果调整该源的并发上限。
// 与 Report 分开：Report 要写库和熔断，Observe 每片都调，必须纯内存。
func (s *Scheduler) Observe(id int64, bytes int64, d time.Duration, ok bool, code int) {
	if s == nil || !s.cfg.Dynamic || id == 0 {
		return
	}
	if s.deleted(id) {
		// 源已删除：不再调上限，也不重建 dyn。
		return
	}
	g := s.gateFor(id)
	if !ok {
		// 429/503 是明确限流，其它失败也先减半：撤压力不会错。
		g.setLimit(g.current() / 2)
		metrics.SourceLimit(id, g.current())
		s.mu.Lock()
		if st := s.dyn[id]; st != nil {
			st.rate = 0
		}
		s.mu.Unlock()
		return
	}
	if bytes <= 0 || d <= 0 {
		return
	}
	rate := float64(bytes) / d.Seconds()

	s.mu.Lock()
	st := s.dyn[id]
	if st == nil {
		st = &dynState{}
		s.dyn[id] = st
	}
	prev := st.rate
	queued := g.takeQueued()
	switch {
	case prev == 0:
		st.rate = rate
		// 首个样本：试探加一路，下轮看吞吐是否跟上。
		if queued {
			g.setLimit(g.current() + 1)
		}
	case rate < prev*0.6:
		// 加并发反而更慢：退一路，均值直接对齐新水位。
		st.rate = rate
		g.setLimit(g.current() - 1)
	default:
		if queued && rate >= prev*0.9 {
			g.setLimit(g.current() + 1)
		}
		st.rate = 0.7*prev + 0.3*rate
	}
	st.last = s.now()
	s.mu.Unlock()
	metrics.SourceLimit(id, g.current())
}

// Limit 返回某台源当前的并发上限，动态关闭时是配置的固定值。
func (s *Scheduler) Limit(id int64) int {
	if s == nil {
		return 0
	}
	if !s.cfg.Dynamic {
		return s.cfg.MaxPerSource
	}
	return s.gateFor(id).current()
}
