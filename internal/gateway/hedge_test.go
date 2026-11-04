package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// hedgeTargets 把若干 httptest server 包成按顺序排列的候选。
func hedgeTargets(srvs ...*httptest.Server) []fetchTarget {
	out := make([]fetchTarget, 0, len(srvs))
	for _, s := range srvs {
		u, _ := url.Parse(s.URL + "/file")
		out = append(out, fetchTarget{u: u})
	}
	return out
}

func hedgePlan() *adapter.Plan { return &adapter.Plan{} }

// 首选源很快就给出响应头：第二个源不该被碰。
func TestHedgeFastFirstDoesNotStartSecond(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	first := &fakeUpstream{body: "hello", etag: `"v1"`}
	s1 := httptest.NewServer(first)
	defer s1.Close()
	second := &fakeUpstream{body: "hello", etag: `"v1"`, delay: 3 * time.Second}
	s2 := httptest.NewServer(second)
	defer s2.Close()

	up, idx, tried, err := g.pickUpstream(context.Background(), hedgePlan(), nil, hedgeTargets(s1, s2))
	if err != nil {
		t.Fatalf("pickUpstream: %v", err)
	}
	defer func() { up.resp.Body.Close(); up.cancel() }()
	if idx != 0 || tried != 1 {
		t.Fatalf("应只用首选源，得到 idx=%d tried=%d", idx, tried)
	}
	if n := atomic.LoadInt32(&second.hits); n != 0 {
		t.Fatalf("备用源不该被请求，实际 %d 次", n)
	}
}

// 首选源卡住：到点后启动第二个，谁快用谁。
func TestHedgeSlowFirstSecondWins(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 100 * time.Millisecond, Grace: 100 * time.Millisecond}
	first := &fakeUpstream{body: "hello", etag: `"v1"`, delay: 3 * time.Second}
	s1 := httptest.NewServer(first)
	defer s1.Close()
	second := &fakeUpstream{body: "hello", etag: `"v1"`}
	s2 := httptest.NewServer(second)
	defer s2.Close()

	start := time.Now()
	up, idx, tried, err := g.pickUpstream(context.Background(), hedgePlan(), nil, hedgeTargets(s1, s2))
	if err != nil {
		t.Fatalf("pickUpstream: %v", err)
	}
	defer func() { up.resp.Body.Close(); up.cancel() }()
	if idx != 1 || tried != 2 {
		t.Fatalf("应由第二个源胜出，得到 idx=%d tried=%d", idx, tried)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("不该等首选源，实际耗时 %v", d)
	}
}

// 首选源快速失败：不等对冲计时器，立刻试下一个。
func TestHedgeFastFailFallsThroughImmediately(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 5 * time.Second, Grace: 100 * time.Millisecond}
	first := &fakeUpstream{body: "x", status: http.StatusNotFound}
	s1 := httptest.NewServer(first)
	defer s1.Close()
	second := &fakeUpstream{body: "hello", etag: `"v1"`}
	s2 := httptest.NewServer(second)
	defer s2.Close()

	start := time.Now()
	up, idx, _, err := g.pickUpstream(context.Background(), hedgePlan(), nil, hedgeTargets(s1, s2))
	if err != nil {
		t.Fatalf("pickUpstream: %v", err)
	}
	defer func() { up.resp.Body.Close(); up.cancel() }()
	if idx != 1 {
		t.Fatalf("404 后应立刻换源，得到 idx=%d", idx)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("不该等满 5s 的对冲阈值，实际 %v", d)
	}
}

// 条件请求：首选源回 304 即胜，不该被备用源的 200 抢走。
func TestHedge304WinsOverLater200(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 50 * time.Millisecond, Grace: 500 * time.Millisecond}
	first := &fakeUpstream{body: "hello", etag: `"v1"`}
	s1 := httptest.NewServer(first)
	defer s1.Close()
	second := &fakeUpstream{body: "hello", etag: `"v2"`}
	s2 := httptest.NewServer(second)
	defer s2.Close()

	prev := &store.CacheEntry{ETag: `"v1"`}
	up, idx, _, err := g.pickUpstream(context.Background(), hedgePlan(), prev, hedgeTargets(s1, s2))
	if err != nil {
		t.Fatalf("pickUpstream: %v", err)
	}
	defer func() { up.resp.Body.Close(); up.cancel() }()
	if idx != 0 || up.resp.StatusCode != http.StatusNotModified {
		t.Fatalf("应保留首选源的 304，得到 idx=%d status=%d", idx, up.resp.StatusCode)
	}
}

// 条件请求：备用源的 200 先到，但首选源在宽限期内回了 304 —— 仍然选 304，
// 省掉一次整文件下载。
func TestHedgeGraceKeepsLate304(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 20 * time.Millisecond, Grace: 800 * time.Millisecond}
	first := &fakeUpstream{body: "hello", etag: `"v1"`, delay: 200 * time.Millisecond}
	s1 := httptest.NewServer(first)
	defer s1.Close()
	second := &fakeUpstream{body: "hello", etag: `"v2"`}
	s2 := httptest.NewServer(second)
	defer s2.Close()

	prev := &store.CacheEntry{ETag: `"v1"`}
	up, idx, _, err := g.pickUpstream(context.Background(), hedgePlan(), prev, hedgeTargets(s1, s2))
	if err != nil {
		t.Fatalf("pickUpstream: %v", err)
	}
	defer func() { up.resp.Body.Close(); up.cancel() }()
	if idx != 0 || up.resp.StatusCode != http.StatusNotModified {
		t.Fatalf("宽限期内到达的 304 应胜出，得到 idx=%d status=%d", idx, up.resp.StatusCode)
	}
}

// 宽限期到点仍没有 304：采用手上的 200。
func TestHedgeGraceExpiresWith200(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 20 * time.Millisecond, Grace: 100 * time.Millisecond}
	first := &fakeUpstream{body: "hello", etag: `"v1"`, delay: 2 * time.Second}
	s1 := httptest.NewServer(first)
	defer s1.Close()
	second := &fakeUpstream{body: "hello", etag: `"v2"`}
	s2 := httptest.NewServer(second)
	defer s2.Close()

	prev := &store.CacheEntry{ETag: `"v1"`}
	start := time.Now()
	up, idx, _, err := g.pickUpstream(context.Background(), hedgePlan(), prev, hedgeTargets(s1, s2))
	if err != nil {
		t.Fatalf("pickUpstream: %v", err)
	}
	defer func() { up.resp.Body.Close(); up.cancel() }()
	if idx != 1 || up.resp.StatusCode != http.StatusOK {
		t.Fatalf("宽限超时应采用 200，得到 idx=%d status=%d", idx, up.resp.StatusCode)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("宽限不该拖长到首选源的 2s，实际 %v", d)
	}
}

// 两个候选都失败：错误语义保持不变（带上游状态码）。
func TestHedgeAllFailKeepsUpstreamError(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 20 * time.Millisecond, Grace: 100 * time.Millisecond}
	s1 := httptest.NewServer(&fakeUpstream{status: http.StatusNotFound})
	defer s1.Close()
	s2 := httptest.NewServer(&fakeUpstream{status: http.StatusInternalServerError})
	defer s2.Close()

	up, _, tried, err := g.pickUpstream(context.Background(), hedgePlan(), nil, hedgeTargets(s1, s2))
	if up != nil {
		up.resp.Body.Close()
		t.Fatal("不该有胜者")
	}
	if tried != 2 {
		t.Fatalf("两个候选都该试过，得到 tried=%d", tried)
	}
	ue, ok := err.(*upstreamError)
	if !ok || ue.Code != http.StatusInternalServerError {
		t.Fatalf("应保留最后一个上游状态码，得到 %v", err)
	}
}

// 客户端取消：立刻返回，不留下悬挂的请求。
func TestHedgeClientCancel(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 20 * time.Millisecond, Grace: 100 * time.Millisecond}
	s1 := httptest.NewServer(&fakeUpstream{body: "hello", delay: 3 * time.Second})
	defer s1.Close()
	s2 := httptest.NewServer(&fakeUpstream{body: "hello", delay: 3 * time.Second})
	defer s2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	up, _, _, err := g.pickUpstream(ctx, hedgePlan(), nil, hedgeTargets(s1, s2))
	if up != nil {
		up.resp.Body.Close()
	}
	if err == nil {
		t.Fatal("客户端取消应返回错误")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("取消后应立刻返回，实际 %v", d)
	}
}

// 主链赢的时候不能把镜像记到备用站点上。fetchRace 是并行起两条链的，
// 旧代码在备链进入 fetchSeq 的瞬间就调 OnFallback，于是第一次请求
// （主链成功、备链随后被取消）就把 library/golang 永久记成 ghcr.io，
// 之后每次直接回 ghcr，全部 404/401 —— 线上实测的 502 就是这么来的。
func TestRaceMainWinDoesNotRememberFallback(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: false}
	fast := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`})
	defer fast.Close()
	slow := httptest.NewServer(&fakeUpstream{body: "hello", etag: `"v1"`, delay: 400 * time.Millisecond})
	defer slow.Close()

	var called atomic.Int32
	plan := &adapter.Plan{
		CacheKey:  "docker:manifest:registry-1.docker.io/library/golang:1.23-alpine",
		Cacheable: true,
		OnFallback: func(h string) { called.Add(1) },
	}
	su, _ := url.Parse(slow.URL + "/file")
	fb := []fetchTarget{{u: su, fb: true, fbHost: "ghcr.io"}}
	if _, _, err := g.fetchRace(context.Background(), plan, hedgeTargets(fast), fb); err != nil {
		t.Fatalf("fetchRace: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if n := called.Load(); n != 0 {
		t.Fatalf("主链成功，却记了 %d 次 fallback 站点", n)
	}
}

// 胜者的 Body 必须在 pickUpstream 返回之后还能完整读出来。
// 旧实现用共享 ctx + defer cancel，函数一返回就把 Body 掐了，
// 小文件靠内核缓冲侥幸读完，十几 MB 的 release 必然读到一半报
// "context canceled" —— 线上 /gh/ 502 就是这么来的。
func TestHedgeWinnerBodySurvivesReturn(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.hedge = HedgeOptions{Enabled: true, After: 50 * time.Millisecond, Grace: 50 * time.Millisecond}
	body := strings.Repeat("x", 4<<20)
	fast := httptest.NewServer(&fakeUpstream{body: body, etag: `"v1"`})
	defer fast.Close()
	slow := httptest.NewServer(&fakeUpstream{body: body, etag: `"v1"`, delay: 2 * time.Second})
	defer slow.Close()

	targets := append(hedgeTargets(fast), hedgeTargets(slow)...)
	plan := &adapter.Plan{CacheKey: "docker:blob:test-body", Cacheable: true}
	up, idx, started, err := g.pickUpstream(context.Background(), plan, nil, targets)
	if err != nil {
		t.Fatalf("pickUpstream: %v", err)
	}
	if idx != 0 {
		t.Fatalf("快的源应该赢，实际 idx=%d", idx)
	}
	n, cerr := io.Copy(io.Discard, up.resp.Body)
	if cerr != nil {
		t.Fatalf("读胜者 Body 失败（%d/%d 字节）: %v", n, len(body), cerr)
	}
	if n != int64(len(body)) {
		t.Fatalf("只读到 %d 字节，期望 %d", n, len(body))
	}
	up.cancel()
	_ = started
}
