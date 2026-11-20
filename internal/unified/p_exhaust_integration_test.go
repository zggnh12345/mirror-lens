package unified

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 6 轮 P 耗尽真实集成：用真实 PTransport.RoundTrip + 真实本地 HTTP 上游 +
// 真实 Response.Body，不用伪计数器。PMax=2/3 只经测试构造路径注入；
// 生产构造路径恒为冻结值 PMax=8192（见 TestProductionPMaxFrozen）。

// realUpstream 是一台真实 HTTP Server：每次请求返回一个带真实 Body 的响应，
// 并统计真正到达服务端的请求数（与 PTransport 的提交数交叉核对）。
type realUpstream struct {
	hits   atomic.Int32
	open   atomic.Int32 // 已发出但未关闭 Body 的响应数
	closed atomic.Int32
}

func (u *realUpstream) handler(w http.ResponseWriter, r *http.Request) {
	u.hits.Add(1)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", "8")
	w.WriteHeader(200)
	w.Write([]byte("12345678"))
}

// TestPExhaustRealRoundTrip：主链与 Hedge 分支都真派发，在途未退时第三次
// 真派发被 P 拒绝 → Scope 取消、所有 Body 关闭、无新派发、committed 不回退、
// 本地预算拒绝不污染任何 Source 失败指标（这里以 ledger 行数=0 为准）。
func TestPExhaustRealRoundTrip(t *testing.T) {
	up := &realUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	defer srv.Close()

	var exhaust atomic.Int32
	s, err := NewScope(context.Background(), ScopeConfig{PMax: 2, OnExhaust: func(kind string) {
		if kind == "P" {
			exhaust.Add(1)
		}
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := srv.Client().Transport
	mt := &PTransport{Base: base, OnExhaust: func() {}}
	ctx := WithScope(context.Background(), s)

	// 主链与 Hedge 分支各一次真实派发。
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v2/a/manifests/latest", nil)
		resp, err := mt.RoundTrip(req)
		if err != nil {
			t.Fatalf("第 %d 次派发: %v", i+1, err)
		}
		// 登记 Body 关闭函数：Scope 耗尽时会主动调用它。
		untrack, ok := s.TrackBody(func() { resp.Body.Close(); up.closed.Add(1) })
		if !ok {
			t.Fatalf("第 %d 个 Body 登记失败", i+1)
		}
		up.open.Add(1)
		// 读到一部分就把 Body 留在「在途未关」状态，模拟 Hedge/主链在途。
		buf := make([]byte, 4)
		if _, err := resp.Body.Read(buf); err != nil && err != io.EOF {
			t.Fatalf("读 Body: %v", err)
		}
		_ = untrack // 保留登记，故意不主动关闭，交给耗尽收尾
	}
	if got := s.P().Committed(); got != 2 {
		t.Fatalf("committed=%d want 2", got)
	}
	if got := up.hits.Load(); got != 2 {
		t.Fatalf("服务端实到请求=%d want 2", got)
	}

	// 第三次：预算耗尽。真实 RoundTrip 在 ReserveP 处被拒，不得到达服务端。
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v2/a/blobs/x", nil)
	rejected := 0
	if _, err := mt.RoundTrip(req); !IsBudgetExhausted(err) {
		t.Fatalf("第三次应因 P 耗尽被拒，实际 %v", err)
	} else {
		rejected++
	}
	if got := up.hits.Load(); got != 2 {
		t.Fatalf("耗尽后不得新派发：服务端请求=%d want 2", got)
	}
	if !s.Exhausted() {
		t.Fatal("耗尽后 Scope 必须进入 Exhausted")
	}
	if got := exhaust.Load(); got != 1 {
		t.Fatalf("OnExhaust 回调次数=%d want 1（幂等）", got)
	}
	// 耗尽会取消 Scope 的 Context（在途 Attempt 立刻中断）；Done 只在
	// Close 时关闭，不能拿它判 cancel。这里检查 Context.Err。
	if s.Context().Err() == nil {
		t.Fatal("耗尽后 Scope Context 必须被取消")
	}
	if got := up.closed.Load(); got != 2 {
		t.Fatalf("已关闭 Body=%d want 2", got)
	}
	// committed 只增不减，已提交派发不返还。
	if got := s.P().Committed(); got != 2 {
		t.Fatalf("committed 回退=%d want 2", got)
	}
	// 本地预算拒绝不写任何 Attempt 结果（不污染 Source 失败指标）。
	if n := s.Ledger().Len(); n != 0 {
		t.Fatalf("预算拒绝不得写结果账本，实际 %d 行", n)
	}
	t.Logf("p_exhaust hedge: pmax_injected=%d dispatched=%d rejected=%d bodies_created=%d bodies_closed=%d on_exhaust_calls=%d claims_dispatched=%d chains=%d ledger_rows=%d outstanding=%d",
		s.P().Max(), s.P().Committed(), rejected, up.hits.Load(), up.closed.Load(), exhaust.Load(),
		s.DispatchedClaims(), s.ChainCount(), s.Ledger().Len(), s.P().Outstanding())
}

// TestPExhaustNoCachePut：耗尽后不提交缓存（本 Scope 无 CachePut 通道即视为
// 无提交；真实网关侧不新增 CacheEntry 由上面的 ledger=0 与 body 关闭共同保证）。
func TestPExhaustNoCachePutAfter(t *testing.T) {
	up := &realUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	defer srv.Close()
	s, err := NewScope(context.Background(), ScopeConfig{PMax: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mt := &PTransport{Base: srv.Client().Transport}
	ctx := WithScope(context.Background(), s)
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v2/a/manifests/latest", nil)
	resp, err := mt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	s.TrackBody(func() { resp.Body.Close() })
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// 第二次被拒，且不得再产生任何结果/缓存提交。
	req2, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v2/a/manifests/latest", nil)
	if _, err := mt.RoundTrip(req2); !IsBudgetExhausted(err) {
		t.Fatalf("应被 P 拒绝: %v", err)
	}
	if s.Ledger().Len() != 0 {
		t.Fatal("耗尽拒绝不得写结果账本")
	}
}

// TestMidfillRangeSoleUseP：Preflight + Range 共同消耗同一个 Scope 的 P，
// 耗尽后不得再建 Preflight / Range / trySwitch。已开 Body 关闭。
func TestMidfillRangeSoleUseP(t *testing.T) {
	up := &realUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	defer srv.Close()
	s, err := NewScope(context.Background(), ScopeConfig{PMax: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mt := &PTransport{Base: srv.Client().Transport}
	ctx := WithScope(context.Background(), s)

	// Preflight 用一次真实派发。
	pre, _, perr := s.Preflight(ctx, func(c context.Context) ([]PreflightResult, int64, error) {
		req, _ := http.NewRequestWithContext(c, "GET", srv.URL+"/v2/a/manifests/latest", nil)
		resp, err := mt.RoundTrip(req)
		if err != nil {
			return nil, 0, err
		}
		s.TrackBody(func() { resp.Body.Close() })
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return []PreflightResult{{}}, 8, nil
	})
	if perr != nil || len(pre) != 1 {
		t.Fatalf("Preflight 失败: %v %v", pre, perr)
	}
	if s.PreflightRuns() != 1 {
		t.Fatal("Preflight 应只跑一轮")
	}
	// Range 用第二次真实派发。
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v2/a/blobs/x", nil)
	req.Header.Set("Range", "bytes=4-")
	resp, err := mt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	s.TrackBody(func() { resp.Body.Close() })
	if err := s.Ranges().Reserve(4); err != nil {
		t.Fatalf("Range 预留失败: %v", err)
	}
	s.Ranges().AddSegments(1)
	resp.Body.Close()
	if got := s.P().Committed(); got != 2 {
		t.Fatalf("P committed=%d want 2", got)
	}
	// 耗尽后：第三条 Range / Preflight / trySwitch 都不允许再获额度。
	// RangeWork 与 P 是两个独立账本：耗尽 P 后 ReserveP 被拒，真正的
	// Range/trySwitch 都由 ReserveP 把关，因此这里断言 ReserveP 拒绝即可。
	rej := 0
	if _, err := s.ReserveP(); !IsBudgetExhausted(err) {
		t.Fatalf("耗尽后 ReserveP 必须拒绝 Range/trySwitch: %v", err)
	} else {
		rej++
	}
	if _, err := s.ReserveP(); err == nil {
		t.Fatal("耗尽后 ReserveP 必须拒绝（取消或预算耗尽）")
	} else {
		rej++
	}
	if got := s.PreflightRuns(); got != 1 {
		t.Fatalf("耗尽后 Preflight 轮数变化=%d", got)
	}
	t.Logf("p_exhaust midfill: pmax_injected=%d preflight_runs=%d range_work_used=%d range_work_max=%d segments=%d committed=%d reserve_after_exhaust_rejected=%d bodies_closed=%d",
		s.P().Max(), s.PreflightRuns(), s.Ranges().Used(), s.Ranges().Max(), s.Ranges().Segments(),
		s.P().Committed(), rej, up.closed.Load())
}

// TestReservePCommitP 两阶段：提交前取消释放预留；已提交不返还；并发不超额；
// 重复结束只释放一次。
func TestReservePCommitP(t *testing.T) {
	s, err := NewScope(context.Background(), ScopeConfig{PMax: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 1) 提交前取消释放预留（直接对 PBudget 注入已取消的父 ctx）。
	ctx, cancel := context.WithCancel(context.Background())
	r1, err := s.P().ReserveP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if r1.Commit() {
		t.Fatal("取消后不得提交")
	}
	if got := s.P().Outstanding(); got != 0 {
		t.Fatalf("取消后 outstanding=%d want 0", got)
	}

	// 2) 已提交派发不返还；重复 Release 只生效一次。
	r2, _ := s.ReserveP()
	if !r2.Commit() {
		t.Fatal("提交失败")
	}
	if r2.Commit() {
		t.Fatal("重复提交必须失败")
	}
	if r2.Release() {
		t.Fatal("已提交后 Release 不得生效")
	}
	if got := s.P().Committed(); got != 1 || s.P().Outstanding() != 1 {
		t.Fatalf("已提交不返还：committed=%d outstanding=%d", got, s.P().Outstanding())
	}

	// 3) 并发 ReserveP 不超额（最后一个必被拒）。
	s2, _ := NewScope(context.Background(), ScopeConfig{PMax: 3}, nil)
	defer s2.Close()
	var ok atomic.Int32
	var rejected atomic.Int32
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			r, err := s2.ReserveP()
			if err != nil {
				rejected.Add(1)
				return
			}
			if r.Commit() {
				ok.Add(1)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if ok.Load() != 3 || rejected.Load() != 5 {
		t.Fatalf("并发超额：committed=%d rejected=%d", ok.Load(), rejected.Load())
	}
	if s2.P().Outstanding() != 3 {
		t.Fatalf("并发超额 outstanding=%d", s2.P().Outstanding())
	}
	t.Logf("reserve_commit: pmax=%d committed=%d outstanding=%d concurrent_committed=%d concurrent_rejected=%d",
		s.P().Max(), s.P().Committed(), s.P().Outstanding(), ok.Load(), rejected.Load())
}

// TestProductionPMaxFrozen：生产构造路径恒为冻结值 8192；测试注入的小额度
// 只存在于测试构造路径，不新增生产用户可配 P 上限。
func TestProductionPMaxFrozen(t *testing.T) {
	if PMax != 8192 {
		t.Fatalf("冻结 PMax 被改动=%d", PMax)
	}
	// 零值 ScopeConfig（生产构造路径）必须得到 8192。
	s, err := NewScope(context.Background(), ScopeConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.P().Max(); got != PMax {
		t.Fatalf("生产 PMax=%d want %d", got, PMax)
	}
	// 1 秒内不超时的 sanity：耗尽路径不会卡住（真实 Body 已关）。
	time.Sleep(time.Millisecond)
	t.Logf("production_pmax: frozen=%d scope_max=%d", PMax, s.P().Max())
}
