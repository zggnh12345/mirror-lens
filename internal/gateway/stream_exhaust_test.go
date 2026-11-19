package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	wrote := make(chan struct{})
	go func() {
		_, _ = pw.Write(make([]byte, 32))
		close(wrote)
	}()
	go func() {
		<-wrote
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
	partPath, _ := cs.PartialPaths(plan.CacheKey)
	if _, err := os.Stat(partPath); err == nil {
		t.Fatalf(".part 未清理: %s", partPath)
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
	g := &Gateway{}
	if srcs, total := g.parPreflightShared(ctx, racePlan("quay.io", "a/b", "latest"), nil, time.Second); srcs != nil || total != -1 {
		t.Fatalf("耗尽后不得新增 Preflight，实得 %d/%d", len(srcs), total)
	}
}

// RangeWork 是 RequestScope 生命周期级：tryParallel → midFill 不得重置，
// 每 Range 最多 4 个目标，Segment 上限 256，总量 1024。
func TestRangeLedgerIsScopeLifetime(t *testing.T) {
	sc, ctx := testScope(t)
	keep, ok := rangeAdmit(ctx, 9, 256)
	if !ok || keep != unified.MaxRangeTargets {
		t.Fatalf("每 Range 必须截到 %d 个目标，实得 %d/%v", unified.MaxRangeTargets, keep, ok)
	}
	if got := sc.Ranges().Used(); got != unified.RangeWorkCap {
		t.Fatalf("首轮应占满 RangeWorkCap，实得 %d", got)
	}
	// 第二阶段（midFill）不得重置预算。
	if _, ok := rangeAdmit(ctx, 4, 1); ok {
		t.Fatal("tryParallel→midFill 不得重置 Range 预算")
	}
	if got := sc.Ranges().Used(); got != unified.RangeWorkCap {
		t.Fatalf("预算泄漏/重置：Used=%d", got)
	}
}
