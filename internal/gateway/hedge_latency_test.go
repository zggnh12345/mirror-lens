package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 常态下对冲不该带来任何可见延迟；慢源场景才应该把长尾压下来。
// 默认跳过（一遍要跑一分多钟），手动：HEDGE_LATENCY=1 go test -run TestHedgeLatencyMatrix -v
func TestHedgeLatencyMatrix(t *testing.T) {
	if os.Getenv("HEDGE_LATENCY") == "" {
		t.Skip("设置 HEDGE_LATENCY=1 才跑延迟矩阵")
	}
	const rounds = 15

	pcts := func(ds []time.Duration) (mean, p50, p95 time.Duration) {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		var sum time.Duration
		for _, d := range ds {
			sum += d
		}
		return sum / time.Duration(len(ds)), ds[len(ds)/2], ds[len(ds)*95/100]
	}

	type scenario struct {
		name    string
		first   time.Duration
		firstSt int
		second  time.Duration
	}

	scenarios := []scenario{
		{"首选源即时", 0, http.StatusOK, 0},
		{"首选源 300ms", 300 * time.Millisecond, http.StatusOK, 0},
		{"首选源 1.2s", 1200 * time.Millisecond, http.StatusOK, 0},
		{"首选源 3s", 3 * time.Second, http.StatusOK, 0},
		{"首选源 404", 0, http.StatusNotFound, 0},
	}

	for _, sc := range scenarios {
		// 每次起一对新 server，避免连接复用把第一次的建连成本摊薄。
		base := make([]time.Duration, 0, rounds)
		hedged := make([]time.Duration, 0, rounds)

		for i := 0; i < rounds; i++ {
			g, _, _, _, _ := newGW(t, true)
			g.hedge = HedgeOptions{Enabled: true, After: 100 * time.Millisecond, Grace: 50 * time.Millisecond}
			s1 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`, delay: sc.first, status: sc.firstSt})
			s2 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`, delay: sc.second})

			// 基线：只有首选源，等价于 hedge 关闭。
			t0 := time.Now()
			up, err := g.openUpstream(context.Background(), hedgePlan(), nil, hedgeTargets(s1)[0].u)
			if err == nil && up != nil {
				up.resp.Body.Close()
				up.cancel()
			}
			base = append(base, time.Since(t0))

			// 对冲开启。
			t0 = time.Now()
			hup, _, _, _ := g.pickUpstream(context.Background(), hedgePlan(), nil, hedgeTargets(s1, s2))
			if hup != nil {
				hup.resp.Body.Close()
				hup.cancel()
			}
			hedged = append(hedged, time.Since(t0))

			s1.Close()
			s2.Close()
		}
		bm, bp50, bp95 := pcts(base)
		hm, hp50, hp95 := pcts(hedged)
		t.Logf("%-14s 串行 mean=%-9v p50=%-9v p95=%-9v | 对冲 mean=%-9v p50=%-9v p95=%-9v",
			sc.name, bm.Round(time.Millisecond), bp50.Round(time.Millisecond), bp95.Round(time.Millisecond),
			hm.Round(time.Millisecond), hp50.Round(time.Millisecond), hp95.Round(time.Millisecond))
	}

	// 混合流量：7 成快、2 成 1.2s、1 成 3s。看平均被拖成什么样。
	{
		g, _, _, _, _ := newGW(t, true)
		g.hedge = HedgeOptions{Enabled: true, After: 1 * time.Second, Grace: 50 * time.Millisecond}
		slow1 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`, delay: 1200 * time.Millisecond})
		slow2 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`, delay: 3 * time.Second})
		fast := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`})
		defer slow1.Close()
		defer slow2.Close()
		defer fast.Close()

		var base, hedged []time.Duration
		for i := 0; i < 60; i++ {
			srv := fast
			if i%10 == 7 {
				srv = slow1
			} else if i%10 == 9 {
				srv = slow2
			}
			tg := hedgeTargets(srv, fast)

			t0 := time.Now()
			up, err := g.openUpstream(context.Background(), hedgePlan(), nil, tg[0].u)
			if err == nil && up != nil {
				up.resp.Body.Close()
				up.cancel()
			}
			base = append(base, time.Since(t0))

			t0 = time.Now()
			hup, _, _, _ := g.pickUpstream(context.Background(), hedgePlan(), nil, tg)
			if hup != nil {
				hup.resp.Body.Close()
				hup.cancel()
			}
			hedged = append(hedged, time.Since(t0))
		}
		bm, _, bp95 := pcts(base)
		hm, _, hp95 := pcts(hedged)
		t.Logf("%-14s 串行 mean=%-9v p95=%-9v | 对冲 mean=%-9v p95=%-9v",
			"混合 7:2:1", bm.Round(time.Millisecond), bp95.Round(time.Millisecond),
			hm.Round(time.Millisecond), hp95.Round(time.Millisecond))
	}

	// 并发：全快场景 20 并发 × 10 轮，确认没有 goroutine 堆积。
	{
		g, _, _, _, _ := newGW(t, true)
		g.hedge = HedgeOptions{Enabled: true, After: 100 * time.Millisecond, Grace: 50 * time.Millisecond}
		s1 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`})
		s2 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`})
		defer s1.Close()
		defer s2.Close()
		tg := hedgeTargets(s1, s2)

		before := runtime.NumGoroutine()
		var wg sync.WaitGroup
		t0 := time.Now()
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 10; j++ {
					hup, _, _, _ := g.pickUpstream(context.Background(), hedgePlan(), nil, tg)
					if hup != nil {
						hup.resp.Body.Close()
						hup.cancel()
					}
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(t0)
		time.Sleep(300 * time.Millisecond)
		after := runtime.NumGoroutine()
		t.Logf("并发 20×10（全快）总耗时=%v goroutine 前=%d 后=%d", elapsed.Round(time.Millisecond), before, after)
		if after > before+10 {
			t.Errorf("疑似 goroutine 泄漏：前 %d 后 %d", before, after)
		}
	}
}

// 条件请求场景：304 宽限会不会让常见的「内容没变」变慢。
func TestHedgeConditionalLatency(t *testing.T) {
	if os.Getenv("HEDGE_LATENCY") == "" {
		t.Skip("设置 HEDGE_LATENCY=1 才跑")
	}
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 1 * time.Second, Grace: 300 * time.Millisecond}
	s1 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`})
	s2 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`})
	defer s1.Close()
	defer s2.Close()

	prev := &store.CacheEntry{ETag: `"v1"`}
	var ds []time.Duration
	for i := 0; i < 20; i++ {
		t0 := time.Now()
		up, _, _, err := g.pickUpstream(context.Background(), hedgePlan(), prev, hedgeTargets(s1, s2))
		if err != nil {
			t.Fatalf("pickUpstream: %v", err)
		}
		code := up.resp.StatusCode
		up.resp.Body.Close()
		up.cancel()
		if code != http.StatusNotModified {
			t.Fatalf("应命中 304，得到 %d", code)
		}
		ds = append(ds, time.Since(t0))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	t.Logf("条件请求 304 mean=%v p95=%v（不应有 200 宽限等待）", (sum / 20).Round(time.Microsecond), ds[19].Round(time.Microsecond))
}

// 阈值扫描：对冲阈值 vs 各类慢源的收益。常态（< 阈值）必须零差异。
func TestHedgeThresholdSweep(t *testing.T) {
	if os.Getenv("HEDGE_LATENCY") == "" {
		t.Skip("设置 HEDGE_LATENCY=1 才跑")
	}
	const rounds = 6
	for _, after := range []time.Duration{500 * time.Millisecond, 1000 * time.Millisecond} {
		for _, sc := range []struct {
			name string
			d    time.Duration
		}{
			{"300ms", 300 * time.Millisecond},
			{"700ms", 700 * time.Millisecond},
			{"1.2s", 1200 * time.Millisecond},
			{"3s", 3 * time.Second},
		} {
			var base, hedged []time.Duration
			for i := 0; i < rounds; i++ {
				g, _, _, _, _ := newGW(t, true)
				g.hedge = HedgeOptions{Enabled: true, After: after, Grace: 300 * time.Millisecond}
				s1 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`, delay: sc.d})
				s2 := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`})

				t0 := time.Now()
				up, err := g.openUpstream(context.Background(), hedgePlan(), nil, hedgeTargets(s1)[0].u)
				if err == nil && up != nil {
					up.resp.Body.Close()
					up.cancel()
				}
				base = append(base, time.Since(t0))

				t0 = time.Now()
				hup, _, _, _ := g.pickUpstream(context.Background(), hedgePlan(), nil, hedgeTargets(s1, s2))
				if hup != nil {
					hup.resp.Body.Close()
					hup.cancel()
				}
				hedged = append(hedged, time.Since(t0))
				s1.Close()
				s2.Close()
			}
			var bs, hs time.Duration
			for i := range base {
				bs += base[i]
				hs += hedged[i]
			}
			bm := bs / rounds
			hm := hs / rounds
			t.Logf("阈值 %-5v 源 %-6s 串行 mean=%-8v 对冲 mean=%-8v 变化 %+.0f%%",
				after, sc.name, bm.Round(time.Millisecond), hm.Round(time.Millisecond),
				float64(hm-bm)/float64(bm)*100)
		}
	}
}
