package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// 动态带宽调度（阶段 9）。
//
// 并发上限不能拍脑袋定成 4：一条 5 Mbps 的源，4 路分片只会互相抢带宽，实际
// 比单路还慢；一条 1 Gbps 的源，4 路又喂不饱。真正决定「开几路」的是实测
// 吞吐——这就是 AIMD 在这里的用处：
//
//   - 排队了（有请求在等名额）且吞吐没退步 → 上限 +1，说明还能吃更多并发；
//   - 吞吐比滑动平均掉了 40% → 上限 -1，说明已经在互相踩；
//   - 失败 / 429 / 503 → 上限减半，先把压力撤掉，别把源打死。
//
// 只有实测证据才动上限，没有样本就保持原值——猜出来的并发比固定值更糟。

// gate 是一台源的并发闸门，容量可在运行中调整。
//
// 不用带缓冲 channel 是因为容量要变：channel 的 cap 建好就固定了。这里用
// 「关闭并重建等待通道」广播唤醒，ctx 取消也能立刻退出。
type gate struct {
	mu       sync.Mutex
	inflight int
	limit    int
	max      int
	wait     chan struct{}
	// queued 记录「因为满员而等待」的次数，Observe 用它判断是否还有扩容空间。
	queued int
}

func newGate(limit, max int) *gate {
	if limit < 1 {
		limit = 1
	}
	if max < limit {
		max = limit
	}
	return &gate{limit: limit, max: max, wait: make(chan struct{})}
}

func (g *gate) acquire(ctx context.Context) (func(), error) {
	for {
		g.mu.Lock()
		if g.inflight < g.limit {
			g.inflight++
			g.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					g.inflight--
					close(g.wait)
					g.wait = make(chan struct{})
					g.mu.Unlock()
				})
			}, nil
		}
		g.queued++
		ch := g.wait
		g.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
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

// takeQueued 返回并清零「排队过几次」的标记。
func (g *gate) takeQueued() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := g.queued > 0
	g.queued = 0
	return q
}

// dynState 是每台源的吞吐滑动平均与当前上限。
type dynState struct {
	rate float64 // 字节/秒，EWMA
	last time.Time
}

// Observe 用一次真实传输的结果调整该源的并发上限。
//
// 和 Report 分开：Report 要写库、记健康、开熔断；Observe 只做带宽控制，
// 每次分片完成都会调，必须是纯内存的。
func (s *Scheduler) Observe(id int64, bytes int64, d time.Duration, ok bool, code int) {
	if s == nil || !s.cfg.Dynamic || id == 0 {
		return
	}
	g := s.gateFor(id)
	if !ok {
		// 429/503 是明确的「你在打我」，其它失败也先减半：撤压力不会错。
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
		// 第一次有样本：试探性加一路，下一轮看吞吐有没有跟上。
		if queued {
			g.setLimit(g.current() + 1)
		}
	case rate < prev*0.6:
		// 加了并发反而更慢：退一路，并让平均值快速跟上新水位。
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

// Limit 返回某台源当前的并发上限；动态关闭时就是配置里的固定值。
func (s *Scheduler) Limit(id int64) int {
	if s == nil {
		return 0
	}
	if !s.cfg.Dynamic {
		return s.cfg.MaxPerSource
	}
	return s.gateFor(id).current()
}
