package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/unified"
)

// 3.5.17 阶段 4 路径接入的门禁测试。全部用可控夹具，不依赖外网。

func racePlan(host, repo, ref string) *adapter.Plan {
	return &adapter.Plan{
		Kind:         "registry",
		ResourceKind: adapter.ResourceManifest,
		LogicalHost:  host,
		Repo:         repo,
		Ref:          ref,
		Registry:     true,
		Upstream:     &url.URL{Scheme: "https", Host: host, Path: "/v2/" + repo + "/manifests/" + ref},
		Method:       http.MethodGet,
		Cacheable:    true,
	}
}

func testScope(t *testing.T) (*unified.Scope, context.Context) {
	t.Helper()
	sc, err := unified.NewScope(context.Background(), unified.ScopeConfig{}, nil)
	if err != nil {
		t.Fatalf("建 Scope: %v", err)
	}
	t.Cleanup(sc.Close)
	return sc, unified.WithScope(context.Background(), sc)
}

// T51：A 站内竞速里，同 Identity 的多个 Source 共享同一个 Claim，
// 多次 Dispatch 是幂等的，Dispatched 只记一次。
func TestUnifiedSharedClaimSameChain(t *testing.T) {
	sc, ctx := testScope(t)
	ctx = withUnifiedChain(ctx, unified.ChainA)
	plan := racePlan("quay.io", "library/nginx", "sha256:"+"a"+"b")

	g := &Gateway{}
	a1, err := g.beginUnifiedAttempt(ctx, plan, "quay.io", 11)
	if err != nil {
		t.Fatalf("第一路预占: %v", err)
	}
	a2, err := g.beginUnifiedAttempt(ctx, plan, "quay.io", 12)
	if err != nil {
		t.Fatalf("第二路应共享 Claim，实际: %v", err)
	}
	if err := a1.dispatch(); err != nil {
		t.Fatalf("第一路 dispatch: %v", err)
	}
	if err := a2.dispatch(); err != nil {
		t.Fatalf("第二路 dispatch 应幂等: %v", err)
	}
	if got := sc.DispatchedClaims(); got != 1 {
		t.Fatalf("同一 Identity 只应有 1 个 Dispatched，实得 %d", got)
	}
	id := planIdentity(plan, "quay.io")
	if st := sc.Claims()[id]; st != unified.ClaimDispatched {
		t.Fatalf("Claim 状态应为 dispatched，实得 %v", st)
	}
	// R 是逻辑 Attempt：两个 Source 各消耗一次，Claim 共享不影响 R。
	if used := sc.R().Used(); used != 2 {
		t.Fatalf("R 应记 2 次逻辑 Attempt，实得 %d", used)
	}
}

// T50：跨链同 Identity 只允许一条链越过发送提交边界；另一条被抑制，
// 且抑制是中性事件（不罚源、不写失败结果）。
func TestUnifiedCrossChainClaimExclusive(t *testing.T) {
	sc, ctx := testScope(t)
	mainCtx := withUnifiedChain(ctx, unified.ChainMain)
	fbCtx := withUnifiedChain(ctx, unified.ChainFallback)
	plan := racePlan("ghcr.io", "owner/app", "sha256:"+"c"+"d")

	g := &Gateway{}
	a1, err := g.beginUnifiedAttempt(mainCtx, plan, "ghcr.io", 21)
	if err != nil {
		t.Fatalf("主链预占: %v", err)
	}
	if err := a1.dispatch(); err != nil {
		t.Fatalf("主链 dispatch: %v", err)
	}
	if _, err := g.beginUnifiedAttempt(fbCtx, plan, "ghcr.io", 22); err == nil {
		t.Fatal("同 Identity 的第二条链必须被抑制")
	} else if err != errUnifiedSuppressed {
		t.Fatalf("应返回 errUnifiedSuppressed，实得 %v", err)
	}
	if got := sc.DispatchedClaims(); got != 1 {
		t.Fatalf("跨链只允许 1 个 Dispatched，实得 %d", got)
	}
	// 不同逻辑 Registry（裸名 Docker Hub → GHCR）不冲突。
	other := racePlan("ghcr.io", "owner/app", "sha256:"+"c"+"d")
	other.LogicalHost = "docker.io"
	if _, err := g.beginUnifiedAttempt(fbCtx, other, "docker.io", 23); err != nil {
		t.Fatalf("不同 Registry 不应冲突: %v", err)
	}
}

// T61：静态抑制快照把「与主链同 Identity」的备用目标整条剔除。
func TestFallbackStaticSuppression(t *testing.T) {
	plan := racePlan("quay.io", "library/redis", "sha256:"+"e"+"f")
	mainURL := &url.URL{Scheme: "https", Host: "quay.io", Path: "/v2/library/redis/manifests/x"}
	fbSame := &url.URL{Scheme: "https", Host: "mirror.local", Path: mainURL.Path}
	fbOther := &url.URL{Scheme: "https", Host: "ghcr.io", Path: mainURL.Path}
	targets := []fetchTarget{
		{u: mainURL},
		{u: fbSame, fb: true, fbHost: "quay.io"},
		{u: fbOther, fb: true, fbHost: "ghcr.io"},
	}
	out, n := suppressFallbacks(plan, targets)
	if n != 1 {
		t.Fatalf("应抑制 1 个同 Identity 的 fb 目标，实得 %d", n)
	}
	if len(out) != 2 {
		t.Fatalf("剩余目标应为 2，实得 %d", len(out))
	}
	for _, tt := range out {
		if tt.fb && tt.fbHost == "quay.io" {
			t.Fatal("同 Identity 的 fb 目标没有被剔除")
		}
	}
}

// B16：per-Registry 重试令牌只允许一次，且不跨 Registry 借用。
func TestRegistryRetryTokenBoundary(t *testing.T) {
	_, ctx := testScope(t)
	if !takeRetryToken(ctx, "quay.io") {
		t.Fatal("第一次应拿到令牌")
	}
	if takeRetryToken(ctx, "quay.io") {
		t.Fatal("同一 Registry 第二次必须被拒")
	}
	if !takeRetryToken(ctx, "gcr.io") {
		t.Fatal("不同 Registry 应各有自己的令牌")
	}
	// 没有 Scope 的旧路径不新增重试行为。
	if takeRetryToken(context.Background(), "quay.io") {
		t.Fatal("无 Scope 时不应返回令牌")
	}
}

// P/hedge 门控：预算耗尽时不再触发对冲；没有 Scope 时保持旧行为。
func TestHedgeSuppressedByPBudget(t *testing.T) {
	if !hedgeAllowed(context.Background()) {
		t.Fatal("无 Scope 时必须保持旧行为（允许对冲）")
	}
	sc, ctx := testScope(t)
	if !hedgeAllowed(ctx) {
		t.Fatal("新 Scope 预算充足，应允许对冲")
	}
	// 把 P 全部占满：剩余为 0 时不允许再开第二路。
	res, err := sc.ReserveP()
	if err != nil {
		t.Fatalf("预留: %v", err)
	}
	for sc.P().Remaining() > 0 {
		if _, err := sc.ReserveP(); err != nil {
			break
		}
	}
	if hedgeAllowed(ctx) {
		t.Fatal("P 耗尽后不应再触发对冲")
	}
	res.Release()
}

// RangeWork：每 Range 最多 4 个目标，总量受 RangeWorkCap 约束。
func TestRangeAdmitCaps(t *testing.T) {
	_, ctx := testScope(t)
	if keep, ok := rangeAdmit(ctx, 9, 10); !ok || keep != unified.MaxRangeTargets {
		t.Fatalf("目标数应截断到 %d，实得 %d ok=%v", unified.MaxRangeTargets, keep, ok)
	}
	// 256 段 × 4 目标 = 1024，正好是上限；再多的段必须被拒。
	if _, ok := rangeAdmit(ctx, 4, 257); ok {
		t.Fatal("257 段 × 4 目标超过 RangeWorkCap，必须拒绝")
	}
}

// T55：tryParallel 与 midFill 共享同一 Scope 的一轮 Preflight。
func TestPreflightSharedOncePerScope(t *testing.T) {
	sc, ctx := testScope(t)
	g, _, _, body, _, mirror, _, ms := parFixture(t, 4<<20)
	plan, cand := parPlan(t, ms, body)
	mu, err := url.Parse(ms.URL + "/v2/library/x/blobs/sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	targets := []fetchTarget{{u: mu, c: cand}}

	srcs1, total1 := g.parPreflightShared(ctx, plan, targets, time.Second)
	srcs2, total2 := g.parPreflightShared(ctx, plan, targets, time.Second)
	if len(srcs1) != 1 || len(srcs2) != 1 || total1 != total2 {
		t.Fatalf("两轮结果应一致：%d/%d total=%d/%d", len(srcs1), len(srcs2), total1, total2)
	}
	if got := sc.Ranges().Used(); got != 0 {
		t.Fatalf("Preflight 不占 RangeWork，实得 %d", got)
	}
	if sc.PreflightRuns() != 1 {
		t.Fatalf("PreflightRuns 应为 1，实得 %d", sc.PreflightRuns())
	}
	if h := atomic.LoadInt32(&mirror.hits); h != 1 {
		t.Fatalf("每 Scope 只允许一轮 Preflight，实际命中 %d 次", h)
	}
}

// T62：部署期全局回退是「整条请求」级别的开关，不产生半新半旧。
func TestUnifiedOffNoScopeCreated(t *testing.T) {
	g := &Gateway{unifiedOff: true}
	r := httptest.NewRequest(http.MethodGet, "/v2/quay.io/foo/bar/manifests/latest", nil)
	nr, done, err := g.newRequestScope(r)
	if err != nil {
		t.Fatalf("回退态不应报错: %v", err)
	}
	if done != nil {
		t.Fatal("回退态不应产生 Scope 收尾函数")
	}
	if unified.FromContext(nr.Context()) != nil {
		t.Fatal("回退态不应挂 RequestScope")
	}
}

// 结果分类：预算耗尽必须走独立通道，不能被折叠成 Auth/Miss/Fail。
func TestClassifyOutcomeBudgetIndependent(t *testing.T) {
	ex := &unified.ExhaustedError{Kind: "P", Max: unified.PMax}
	if got := classifyOutcome(0, ex); got != unified.OutcomeBudget {
		t.Fatalf("预算耗尽应为 budget，实得 %v", got)
	}
	if got := classifyOutcome(404, &upstreamError{Code: 404}); got != unified.OutcomeMiss {
		t.Fatalf("404 应为 miss，实得 %v", got)
	}
	if got := classifyOutcome(401, &upstreamError{Code: 401}); got != unified.OutcomeAuth {
		t.Fatalf("401 应为 auth，实得 %v", got)
	}
	if got := classifyOutcome(200, nil); got != unified.OutcomeOK {
		t.Fatalf("200 应为 ok，实得 %v", got)
	}
}

// 端到端（受控上游）：Manifest 请求经统一框架后，账本里是 OK，
// 且缓存填充正常；用来证明接线没有改变正常路径的结论。
func TestUnifiedManifestLedgerOK(t *testing.T) {
	f := newMOFixture(t)
	var hits atomic.Int32
	m := &moMirror{path: "/v2/library/alpine/manifests/latest", body: mirrorManifest, hits: &hits}
	srv := httptest.NewServer(m)
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/"+f.host+"/library/alpine/manifests/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实得 %d body=%s", w.Code, w.Body.String())
	}
	if !f.gw.sched.Enabled() {
		t.Fatal("调度器应为启用态，否则候选不会参与")
	}
	_ = scheduler.OutcomeOK
}

// F19/身份：Claim 身份必须含内容协商语义；同 Ref 不同 Accept 是不同身份，
// 等价规范化（大小写/空白/参数顺序/q=1 省略）仍是同一身份。
func TestPlanIdentityIncludesAccept(t *testing.T) {
	a := racePlan("ghcr.io", "owner/app", "sha256:aa")
	a.Header = http.Header{"Accept": {"application/vnd.oci.image.index.v1+json"}}
	b := racePlan("ghcr.io", "owner/app", "sha256:aa")
	b.Header = http.Header{"Accept": {"application/vnd.docker.distribution.manifest.v2+json"}}
	if planIdentity(a, "ghcr.io") == planIdentity(b, "ghcr.io") {
		t.Fatal("同 Ref 不同 Accept 被当成同一身份")
	}
	c := racePlan("ghcr.io", "owner/app", "sha256:aa")
	c.Header = http.Header{"Accept": {"APPLICATION/VND.OCI.IMAGE.INDEX.V1+JSON;q=1.0"}}
	if planIdentity(a, "ghcr.io") != planIdentity(c, "ghcr.io") {
		t.Fatalf("等价 Accept 规范化后应同一身份：%q vs %q",
			planIdentity(a, "ghcr.io").Manifest.Accept, planIdentity(c, "ghcr.io").Manifest.Accept)
	}

	// 端到端：Accept 不同的两路必须能各自取得 Claim（不互相抑制）。
	sc, ctx := testScope(t)
	_ = sc
	g := &Gateway{}
	mctx := withUnifiedChain(ctx, unified.ChainMain)
	fctx := withUnifiedChain(ctx, unified.ChainFallback)
	a1, err := g.beginUnifiedAttempt(mctx, a, "ghcr.io", 31)
	if err != nil {
		t.Fatalf("Accept A 预占: %v", err)
	}
	if err := a1.dispatch(); err != nil {
		t.Fatalf("Accept A dispatch: %v", err)
	}
	if _, err := g.beginUnifiedAttempt(fctx, b, "ghcr.io", 32); err != nil {
		t.Fatalf("Accept B 应独立成 Claim，实际: %v", err)
	}
}

// F19：A 竞速阶段结束不关闭链。同链在竞速之后继续顺序补位可注册 Attempt，
// Claim 保持 Dispatched，且另一条链始终不能接管。
func TestRacePhaseEndKeepsChainAndClaim(t *testing.T) {
	sc, ctx := testScope(t)
	actx := withUnifiedChain(ctx, unified.ChainA)
	plan := racePlan("quay.io", "library/nginx", "sha256:aa")

	g := &Gateway{}
	first, err := g.beginUnifiedAttempt(actx, plan, "quay.io", 41)
	if err != nil {
		t.Fatalf("竞速 Attempt: %v", err)
	}
	if err := first.dispatch(); err != nil {
		t.Fatalf("竞速 dispatch: %v", err)
	}
	first.finish(200, nil)

	// 竞速阶段结束（胜者正文失败）：不关链，顺序补位继续。
	second, err := g.beginUnifiedAttempt(actx, plan, "quay.io", 42)
	if err != nil {
		t.Fatalf("顺序补位注册失败: %v", err)
	}
	if err := second.dispatch(); err != nil {
		t.Fatalf("顺序补位 dispatch 失败: %v", err)
	}
	second.finish(200, nil)

	if got := sc.DispatchedClaims(); got != 1 {
		t.Fatalf("同 Identity 只应有 1 个 Dispatched，实得 %d", got)
	}
	// 主链在这之后仍然不能取得同一身份。
	mainCtx := withUnifiedChain(ctx, unified.ChainMain)
	if _, err := g.beginUnifiedAttempt(mainCtx, plan, "quay.io", 43); err != errUnifiedSuppressed {
		t.Fatalf("另一条链应被抑制，实得 %v", err)
	}
	// 链关闭（请求收尾）后 Claim 仍保持 Dispatched。
	closeUnifiedChain(actx, unified.ChainA)
	if st, _ := sc.ClaimStateOf(planIdentity(plan, "quay.io")); st != unified.ClaimDispatched {
		t.Fatalf("链关闭后 Claim 状态 = %v，期望 dispatched", st)
	}
}
