package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/unified"
)

// P 耗尽发生在「已发正文」之后：必须中断传输、清理 .part、禁止缓存提交，
// 且不得追加一个伪造的 503，也不得把这次中断记成上游失败。
func TestStreamExhaustMidBodyDropsPartialAndCache(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "e.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}
	gw := New(st, Options{Cache: cs, Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})
	gw.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))

	srcID, err := st.AddSource(store.Source{BaseURL: "https://mirror.example.org", Name: "m", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}

	sc, err := unified.NewScope(context.Background(), unified.ScopeConfig{}, nil)
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	defer sc.Close()

	plan := &adapter.Plan{
		Kind:        "registry",
		LogicalHost: "quay.io",
		CacheKey:    "registry|blob|test-exhaust",
		Cacheable:   true,
		TTL:         time.Minute,
		Header:      http.Header{},
	}

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: 4096,
		Header:        http.Header{"Content-Type": {"application/octet-stream"}, "ETag": {`"v1"`}},
		Body:          pr,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/quay.io/x/blobs/sha256:aa", nil)
	req = req.WithContext(unified.WithScope(req.Context(), sc))

	partPath, partMeta := cs.PartialPaths(plan.CacheKey)
	wrote := make(chan struct{})
	go func() {
		_, _ = pw.Write(make([]byte, 32))
		close(wrote)
	}()
	go func() {
		<-wrote
		// 先证明半成品真的落了一半：否则「没有 .part」是空断言。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if st, err := os.Stat(partPath); err == nil && st.Size() == 32 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if st, err := os.Stat(partPath); err != nil || st.Size() != 32 {
			t.Errorf("耗尽前应有 32 字节的 .part，实际 %v/%v", st, err)
		}
		// 正文已经输出了一部分：此刻耗尽 P。
		sc.Exhaust("P")
	}()

	cand := &scheduler.Candidate{ID: srcID, Name: "m"}
	gw.streamBody(rec, req, plan, resp, nil, &fetchTarget{c: cand}, 0)

	if rec.Code != http.StatusOK {
		t.Fatalf("已发正文后不得改写状态码，实得 %d", rec.Code)
	}
	if got := rec.Body.Len(); got != 32 {
		t.Fatalf("响应体应停在中途（32 字节），实得 %d：可能追加了伪造错误页", got)
	}
	if e, _ := st.CacheGet(plan.CacheKey); e != nil {
		t.Fatalf("耗尽后不得提交缓存索引: %+v", e)
	}
	for _, p := range []string{partPath, partMeta} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("半成品未清理: %s", p)
		}
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM checks WHERE source_id=?`, srcID).Scan(&n); err != nil {
		t.Fatalf("查 checks: %v", err)
	}
	if n != 0 {
		t.Fatalf("预算耗尽不是上游故障，不得写失败样本（checks=%d）", n)
	}
}

// 耗尽后禁止新增 Range/Preflight/Hedge：三个准入必须一致 fail closed。
func TestExhaustBlocksRangePreflightHedge(t *testing.T) {
	sc, ctx := testScope(t)
	if _, ok := rangeAdmit(ctx, 4, 10); !ok {
		t.Fatal("未耗尽时应允许 Range 准入")
	}
	if !hedgeAllowed(ctx) {
		t.Fatal("未耗尽时应允许 Hedge 准入")
	}
	sc.Exhaust("P")
	if _, ok := rangeAdmit(ctx, 4, 10); ok {
		t.Fatal("耗尽后不得新增 Range 工作")
	}
	if hedgeAllowed(ctx) {
		t.Fatal("耗尽后不得新增 Hedge")
	}
	// 用真实夹具证明「零上游请求」：只断言返回值无法区分「被阻断」和「没依赖」。
	g, _, _, body, _, mirror, _, ms := parFixture(t, 1<<20)
	plan, cand := parPlan(t, ms, body)
	mu, err := url.Parse(ms.URL + "/v2/library/x/blobs/sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	targets := []fetchTarget{{u: mu, c: cand}}
	if srcs, total := g.parPreflightShared(ctx, plan, targets, time.Second); srcs != nil || total != -1 {
		t.Fatalf("耗尽后不得新增 Preflight，实得 %d/%d", len(srcs), total)
	}
	if h := atomic.LoadInt32(&mirror.hits); h != 0 {
		t.Fatalf("耗尽后不得再打上游，Preflight 命中 %d", h)
	}
	if n := sc.PreflightRuns(); n != 0 {
		t.Fatalf("耗尽后不应启动 Preflight，实得 %d", n)
	}
}

// RangeWork 是 RequestScope 生命周期级：tryParallel → midFill 不得重置，
// 每 Range 最多 4 个目标，Segment 上限 256，总量 1024。
func TestRangeLedgerIsScopeLifetime(t *testing.T) {
	sc, ctx := testScope(t)
	keep, ok := rangeAdmit(ctx, 9, 10)
	if !ok || keep != unified.MaxRangeTargets {
		t.Fatalf("每 Range 必须截到 %d 个目标，实得 %d/%v", unified.MaxRangeTargets, keep, ok)
	}
	if got := sc.Ranges().Used(); got != 40 {
		t.Fatalf("首轮应记 4 目标 × 10 段 = 40，实得 %d", got)
	}
	// 第二阶段（midFill）结转而不是重置。
	if _, ok := rangeAdmit(ctx, 4, 246); !ok {
		t.Fatal("剩余额度内应允许 midFill 分片")
	}
	if got := sc.Ranges().Used(); got != unified.RangeWorkCap {
		t.Fatalf("跨阶段应累计到上限：Used=%d", got)
	}
	if _, ok := rangeAdmit(ctx, 4, 1); ok {
		t.Fatal("超出 1024 后不得再准入")
	}
	if got := sc.Ranges().Used(); got != unified.RangeWorkCap {
		t.Fatalf("超额准入不得写账：Used=%d", got)
	}
}
