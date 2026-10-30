// Package runner schedules probes, applies digest consensus and maintains
// source status.
package runner

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

type Engine struct {
	st       *store.Store
	mu       sync.Mutex
	inflight bool
	queued   map[string]int // 探测中又来的请求：kind -> 需要的最高 tier
	lastRun  map[string]time.Time

	officialDigest string
	officialAt     time.Time
}

func New(st *store.Store) *Engine {
	return &Engine{st: st, lastRun: map[string]time.Time{}, queued: map[string]int{}}
}

// Busy 表示当前是否有探测在跑，面板用来显示"检测中"。
func (e *Engine) Busy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inflight
}

func (e *Engine) Run(kind string, tier int) {
	e.mu.Lock()
	if e.inflight {
		// 已经在跑：记下来，等这轮结束后补跑，不能直接丢掉
		// （否则刚发现/刚添加的源会一直停在 pending）。
		if tier > e.queued[kind] {
			e.queued[kind] = tier
		}
		e.mu.Unlock()
		return
	}
	e.inflight = true
	e.lastRun[kind] = time.Now()
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.inflight = false
		q := e.queued
		e.queued = map[string]int{}
		e.mu.Unlock()
		for k, t := range q {
			go e.Run(k, t)
		}
	}()

	srcs, err := e.st.Enabled(kind, tier)
	if err != nil {
		log.Printf("runner: list sources: %v", err)
		return
	}
	// tier3 要下载真实文件，只对还活着的源做，省带宽也省对方限流额度。
	if tier >= 3 {
		live := srcs[:0]
		for _, s := range srcs {
			if s.Status == "usable" || s.Status == "degraded" {
				live = append(live, s)
			}
		}
		srcs = live
	}
	if len(srcs) == 0 {
		return
	}

	expected := ""
	if kind == "docker" && tier >= 2 {
		expected = e.baseline(context.Background())
	}

	type pair struct {
		src store.Source
		res probe.Result
	}
	results := make([]pair, len(srcs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range srcs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			s := srcs[i]
			var r probe.Result
			if s.Kind == "docker" {
				r = probe.Docker(ctx, s.BaseURL, tier, expected)
			} else {
				r = probe.GitHub(ctx, s.BaseURL, tier, "")
			}
			results[i] = pair{s, r}
		}(i)
	}
	wg.Wait()

	// 共识复核：tier>=3 且没有官方基准时，用多数派的 digest 作为基准。
	if kind == "docker" && tier >= 3 && expected == "" {
		expected = consensus(results, func(p pair) string { return p.res.Digest })
		if expected != "" {
			for i := range results {
				if results[i].res.OK && results[i].res.Digest != "" {
					if results[i].res.Digest == expected {
						results[i].res.DigestOK = 1
					} else {
						results[i].res.DigestOK = 0
					}
				}
			}
		}
	}

	for _, p := range results {
		c := store.Check{
			SourceID:  p.src.ID,
			Tier:      tier,
			OK:        p.res.OK,
			LatencyMS: p.res.LatencyMS,
			SpeedKBps: p.res.SpeedKBps,
			Digest:    p.res.Digest,
			DigestOK:  p.res.DigestOK,
			TLSOK:     p.res.TLSOK,
			CertDays:  p.res.CertDays,
			HTTPCode:  p.res.HTTPCode,
			Error:     p.res.Error,
			TS:        time.Now().Unix(),
		}
		if err := e.st.AddCheck(c); err != nil {
			log.Printf("runner: add check %d: %v", p.src.ID, err)
			continue
		}
		e.updateStatus(p.src, p.res, tier)
	}
	log.Printf("runner: %s tier%d done, %d sources", kind, tier, len(srcs))
}

func (e *Engine) updateStatus(s store.Source, r probe.Result, tier int) {
	switch {
	case r.DigestOK == 0:
		_ = e.st.SetStatus(s.ID, "blocked") // 内容不一致：直接封禁，等人看
		return
	case r.OK:
		if tier >= 2 {
			_ = e.st.SetStatus(s.ID, "usable")
		} else if s.Status == "dead" {
			_ = e.st.SetStatus(s.ID, "degraded")
		}
		return
	}
	// 失败：连续三次 tier>=2 失败才判死，避免一次抖动就下架。
	if tier < 2 {
		if s.Status == "usable" {
			_ = e.st.SetStatus(s.ID, "degraded")
		}
		return
	}
	fails := 0
	if series, err := e.st.Series(s.ID, 3); err == nil {
		for i := len(series) - 1; i >= 0; i-- {
			if series[i].OK {
				break
			}
			fails++
		}
	}
	if fails >= 3 {
		_ = e.st.SetStatus(s.ID, "dead")
	} else {
		_ = e.st.SetStatus(s.ID, "degraded")
	}
}

func consensus[T any](items []T, digest func(T) string) string {
	counts := map[string]int{}
	best, bestN := "", 0
	for _, it := range items {
		d := digest(it)
		if d == "" {
			continue
		}
		counts[d]++
		if counts[d] > bestN {
			best, bestN = d, counts[d]
		}
	}
	if bestN < 2 {
		return ""
	}
	return best
}

// baseline 取官方 registry 的 digest 作为权威基准；6 小时缓存，失败回退空。
func (e *Engine) baseline(ctx context.Context) string {
	e.mu.Lock()
	if time.Since(e.officialAt) < 6*time.Hour && e.officialDigest != "" {
		d := e.officialDigest
		e.mu.Unlock()
		return d
	}
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r := probe.Docker(ctx, "https://registry-1.docker.io", 2, "")
	if r.OK && r.Digest != "" {
		e.mu.Lock()
		e.officialDigest, e.officialAt = r.Digest, time.Now()
		e.mu.Unlock()
		return r.Digest
	}
	return ""
}

// Loop 启动内置调度：L1 每 3 分钟、L2 每 15 分钟、L3 每 6 小时、清理每天一次。
func (e *Engine) Loop(ctx context.Context) {
	go func() {
		// 启动后先跑一轮，面板立刻有数据
		time.Sleep(2 * time.Second)
		e.Run("docker", 2)
		e.Run("github", 2)
		e.Run("docker", 3)
		e.Run("github", 3)
	}()
	t1 := time.NewTicker(3 * time.Minute)
	t2 := time.NewTicker(15 * time.Minute)
	t3 := time.NewTicker(6 * time.Hour)
	pr := time.NewTicker(24 * time.Hour)
	defer t1.Stop()
	defer t2.Stop()
	defer t3.Stop()
	defer pr.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t1.C:
			e.Run("docker", 1)
			e.Run("github", 1)
		case <-t2.C:
			e.Run("docker", 2)
			e.Run("github", 2)
		case <-t3.C:
			e.Run("docker", 3)
			e.Run("github", 3)
		case <-pr.C:
			_ = e.st.Prune(time.Now().AddDate(0, 0, -30).Unix())
		}
	}
}
