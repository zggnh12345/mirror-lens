package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// ============================================================================
// 18d 第一阶段：A/B characterization + A+B 嵌套 RED
//
// 本文件只新增测试，不改任何生产调度代码。目的：
//   1. 用外部可观察行为锁定现有 A（站内 Source 竞速）与 B（跨 Registry 竞速）
//      的独立语义，作为后续重构的回归硬门禁；
//   2. 用确定性 RED 证明「B 选中的 Registry 内部没有 A 站内竞速」这一缺口
//      真实存在（在干净基线 56335ab 上编译成功、因行为缺失而断言失败）。
//
// 纪律（用户裁定）：
//   - characterization 不依赖内部函数调用顺序/局部变量，只锁候选资格、并发上限、
//     失败补位、取消、身份验证与缓存提交边界等外部行为。
//   - RED 不用固定 Sleep 赌时序：用并发屏障（channel）让「先失败的 Source」
//     与「可用的 Source」行为确定化。
//   - RED 必须编译成功且因目标行为缺失失败；不得以编译/环境/Mock 错误充当 RED。
// ============================================================================

// ---------------------------------------------------------------------------
// A 层 characterization：锁定 A 站内竞速的独立外部行为
// ---------------------------------------------------------------------------

// A-char-1：A 竞速资格 —— 只有内容寻址、双星标源、Host 开关打开、调度器启用时
// 才形成两路竞速；单星标源只走单路，不回官方。锁定「资格门」外部行为。
func TestCharA_RaceEligibilitySingleSource(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srv, hits := f.mirror(t, body, 30*time.Millisecond)
	id := f.addRegistrySource(t, srv, "native")
	f.star(t, id, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	w := f.getDigest(t, digest)
	if w.Code != http.StatusOK {
		t.Fatalf("A 单源应成功: status=%d body=%q", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("A 单源只应打一次，实得 %d", hits.Load())
	}
	if f.officialHits.Load() != 0 {
		t.Fatalf("A 单源成功不应回官方，official=%d", f.officialHits.Load())
	}
}

// A-char-2：A 并发上限 A<=2 —— 三个合格源最多只有两个被启动。锁定并发边界。
func TestCharA_AtMostTwoSources(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 120*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 120*time.Millisecond)
	srvC, hitsC := f.mirror(t, body, 120*time.Millisecond)
	ids := []int64{
		f.addRegistrySource(t, srvA, "native"),
		f.addRegistrySource(t, srvB, "native"),
		f.addRegistrySource(t, srvC, "native"),
	}
	for _, id := range ids {
		f.star(t, id, true)
	}
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	if w := f.getDigest(t, digest); w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if total := hitsA.Load() + hitsB.Load() + hitsC.Load(); total > 2 {
		t.Fatalf("A 层并发上限为 2，实际启动 %d 个 Source", total)
	}
}

// A-char-3：A 失败立即补位 —— 第一路 500，第二路必须启动并交付。锁定失败补位。
func TestCharA_FailureBackfillsWithinHost(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	var failHits atomic.Int32
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	srvB, hitsB := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, failSrv, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	w := f.getDigest(t, digest)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != body {
		t.Fatalf("第一路 500 后应由第二路交付: status=%d body=%q", w.Code, w.Body.String())
	}
	if failHits.Load() == 0 || hitsB.Load() == 0 {
		t.Fatalf("失败补位未发生: fail=%d ok=%d", failHits.Load(), hitsB.Load())
	}
}

// A-char-4：A 胜者坏正文不污染缓存 —— 坏源先回 200 但正文 digest 不符，
// 应顺序回退到可用源，且缓存写入的是回退后的正确内容。锁定缓存提交边界。
func TestCharA_BadWinnerNotCached(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	good := mirrorManifest
	digest := digestOf(good)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", manifestsAccept)
		w.Write([]byte(`{"schemaVersion":2,"tampered":true}`))
	}))
	defer bad.Close()
	idBad := f.addRegistrySource(t, bad, "native")
	// 可用源稍慢，确保坏源先交响应头成为赢家。
	srvGood, _ := f.mirror(t, good, 300*time.Millisecond)
	idGood := f.addRegistrySource(t, srvGood, "native")
	f.star(t, idBad, true)
	f.star(t, idGood, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(good)

	w := f.getDigest(t, digest)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != good {
		t.Fatalf("坏赢家应回退到可用源: status=%d body=%q", w.Code, w.Body.String())
	}
	e, _ := f.st.CacheGet(f.cacheKey(t, digest))
	if e == nil {
		t.Fatal("回退成功后应提交缓存")
	}
	if e.SHA256 != shaOf([]byte(good)) {
		t.Fatalf("缓存 digest=%s，期望 %s（坏内容被提交？）", e.SHA256, shaOf([]byte(good)))
	}
}

// ---------------------------------------------------------------------------
// B 层 characterization：锁定 B 跨 Registry 竞速的独立外部行为
// ---------------------------------------------------------------------------

// B-char-1：两副本一致直接确认，权威站零派发，每候选只下载一次。
func TestCharB_AgreementConfirmsWithoutAuthority(t *testing.T) {
	body := []byte(bodyValidManifest())
	f := newReplicaFixture(t, body, body)
	f.plan.ExpectSHA = shaOf(body)
	f.plan.Immutable = true
	f.addReplica(t, "gcr.io", "mirror/kube-apiserver", body)

	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil || !handled || e == nil {
		t.Fatalf("两副本一致应确认: handled=%v err=%v", handled, err)
	}
	if e.SHA256 != shaOf(body) {
		t.Fatalf("内容不对: %s", e.SHA256)
	}
	if f.official.Load() != 0 {
		t.Fatalf("一致时权威站不应派发，实际 %d", f.official.Load())
	}
}

// B-char-2：副本不一致时唯一 Authority 兜底，只交付权威正文。
func TestCharB_MismatchFallsBackToAuthority(t *testing.T) {
	good := []byte(bodyValidManifest())
	bad := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:bad"}]}`)
	f := newReplicaFixture(t, good, bad)
	f.addReplica(t, "gcr.io", "mirror/kube-apiserver", good)

	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil || !handled || e == nil {
		t.Fatalf("不一致应 Authority 兜底: handled=%v err=%v", handled, err)
	}
	if e.SHA256 != shaOf(good) {
		t.Fatalf("必须交付权威正文，实得 %s", e.SHA256)
	}
	if f.official.Load() != 1 {
		t.Fatalf("权威只允许一次派发，实际 %d", f.official.Load())
	}
}

// B-char-3：M=4，权威站不占名额；5 条副本映射只有 4 个副本候选。
func TestCharB_AuthorityNotCountedInM(t *testing.T) {
	body := []byte(bodyValidManifest())
	f := newReplicaFixture(t, body, body)
	for i, h := range []string{"gcr.io", "public.ecr.aws", "mcr.microsoft.com", "cgr.dev"} {
		f.addReplica(t, h, fmt.Sprintf("mirror/kube-apiserver-%d", i), body)
	}
	ts, _, err := f.gw.replicaTargets(f.ctx(), f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	if len(ts) != crossReplicaM+1 {
		t.Fatalf("候选总数=%d，期望 %d（4 副本 + 1 权威）", len(ts), crossReplicaM+1)
	}
	if ts[0].sourceID != 0 {
		t.Fatalf("下标 0 必须是权威站，实际 sourceID=%d", ts[0].sourceID)
	}
}

// B-char-4：副本候选共享唯一 Claim —— 同一请求只有一条 Dispatched Claim，
// 身份为权威逻辑 Registry。锁定 RequestScope 级单一提交权。
func TestCharB_SingleRequestClaim(t *testing.T) {
	body := []byte(bodyValidManifest())
	f := newReplicaFixture(t, body, body)
	f.addReplica(t, "gcr.io", "mirror/kube-apiserver", body)
	f.plan.ExpectSHA = shaOf(body)
	f.plan.Immutable = true

	if _, _, err := f.gw.fetchWithReplicas(f.ctx(), f.plan, nil); err != nil {
		t.Fatalf("竞速: %v", err)
	}
	claims := f.sc.Claims()
	if len(claims) != 1 {
		t.Fatalf("同一请求只允许一条 Claim，实际 %d", len(claims))
	}
	if st := f.sc.DispatchedClaims(); st != 1 {
		t.Fatalf("Dispatched Claim 数=%d，期望 1", st)
	}
}

// B-char-5：Digest 不符的副本不得成为有效胜者，也不得污染缓存。
func TestCharB_WrongDigestReplicaNotCommitted(t *testing.T) {
	good := []byte(bodyValidManifest())
	bad := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:evil"}]}`)
	f := newReplicaFixture(t, good, bad)
	f.gw.replica = "sequential"
	sum := sha256Of(good)
	f.plan.Ref = "sha256:" + sum
	f.plan.ExpectSHA = sum
	f.plan.Immutable = true
	f.plan.Upstream.Path = "/v2/kube-apiserver/manifests/" + f.plan.Ref

	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil || !handled || e == nil {
		t.Fatalf("错 Digest 副本应被跳过、权威交付: handled=%v err=%v", handled, err)
	}
	if e.SHA256 != shaOf(good) {
		t.Fatalf("必须交付权威正文，实得 %s", e.SHA256)
	}
}

// ---------------------------------------------------------------------------
// A+B 嵌套常驻回归测试（原 RED）：证明「B 选中的 Registry 内部会跑 A 站内竞速」。
// 在基线 56335ab 上本测试因 hits2==0 失败（RED 取证见 18d-phase1-char-red.log）；
// 核心重构后第二个 Source 被内层竞速真实尝试，本测试常驻为绿。
//
// 冻结稿 §0.1/§18d/§4.2/§4.3 要求：A、B 都开启且均为竞速时，每个被 B 选中的
// Registry 内部应再跑 A 的多 Source 竞速（C×A=4 峰值）。
//
// 确定性设计（无 Sleep 赌时序）：
//   - 副本 Registry quay.io 配两个可信 Source：source-1 用屏障阻塞，source-2
//     立即返回正确内容。
//   - 嵌套已实现：两个 Source 都竞速尝试 -> hits1 与 hits2 均 >=1。
//   - 屏障保证 source-1 一定先被派发并卡住，source-2 若会被竞速派发也一定会
//     被打到；不依赖固定时间。
//
// ---------------------------------------------------------------------------
func TestNestedBRegistryRunsASourceRace(t *testing.T) {
	body := []byte(bodyValidManifest())
	f := newReplicaFixture(t, body, body)
	f.plan.ExpectSHA = shaOf(body)
	f.plan.Immutable = true

	// 打开 A 侧条件：该 Host 的星标竞速开关。
	if err := f.st.SetStarRace("quay.io", true); err != nil {
		t.Fatalf("打开 A 星标竞速: %v", err)
	}
	f.gw.InvalidateStarRace()
	f.gw.race.After = 10 * time.Millisecond

	// 给 quay.io 加第二个可信 Source： barrier 阻塞，返回正确内容。
	var hits2 atomic.Int32
	blocked := make(chan struct{})
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits2.Add(1)
		select {
		case <-blocked:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", manifestsAccept)
		w.Write(body)
	}))
	defer func() { close(blocked); srv2.Close() }()

	gen, dig, err := f.st.HostGenState("quay.io")
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if res, err := f.st.CommitHostProbe(store.HostProbeCommit{
		SourceID: f.srcID, Host: "quay.io", Cap: store.RegistryCapYes, PathMode: "native",
		At: time.Now().Unix(), Gen: gen, CredDigest: dig, HasCredential: dig != "",
	}); err != nil || res != store.HostCapsCommitted {
		t.Fatalf("重新提交 quay.io 能力: %v %v", res, err)
	}
	// 第二个 Source：登记、repo_host、星标、能力提交、可信映射、参选勾选。
	id2, err := f.st.AddSource(store.Source{BaseURL: srv2.URL, Name: "nested-source-2", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加第二源: %v", err)
	}
	if err := f.st.SetRepoHost(id2, "quay.io"); err != nil {
		t.Fatalf("repo_host 第二源: %v", err)
	}
	star := true
	if err := f.st.UpdateSource(id2, nil, nil, nil, &star); err != nil {
		t.Fatalf("星标第二源: %v", err)
	}
	gen2, dig2, err := f.st.HostGenState("quay.io")
	if err != nil {
		t.Fatalf("gen2: %v", err)
	}
	if res, err := f.st.CommitHostProbe(store.HostProbeCommit{
		SourceID: id2, Host: "quay.io", Cap: store.RegistryCapYes, PathMode: "native",
		At: time.Now().Unix(), Gen: gen2, CredDigest: dig2, HasCredential: dig2 != "",
	}); err != nil || res != store.HostCapsCommitted {
		t.Fatalf("第二源能力提交: %v %v", res, err)
	}
	if _, err := f.st.PutTrustedMapping(store.TrustedMapping{
		RegistryHost: "quay.io", SourceID: id2, Priority: 5, Enabled: true,
	}, "admin", true); err != nil {
		t.Fatalf("第二源可信映射: %v", err)
	}
	sel := append(f.st.CrossSelectedSources(), id2)
	if err := f.st.SetCrossSelectedSources(sel, "admin", ""); err != nil {
		t.Fatalf("第二源参选勾选: %v", err)
	}
	f.gw.sched.Invalidate()

	// 解析候选：确认 quay.io 确实有两个合格 Source（排除「Mock 配置错误」）。
	cands := f.gw.sched.Candidates("registry:quay.io")
	qualified := 0
	for _, c := range cands {
		if c.Preferred {
			qualified++
		}
	}
	if qualified < 2 {
		t.Fatalf("前置条件失败：quay.io 应有 >=2 个合格星标 Source，实际 %d（cands=%d）", qualified, len(cands))
	}
	t.Logf("RED 前置条件成立：quay.io 合格星标 Source 数=%d（调度器候选 cands=%d）", qualified, len(cands))

	// 触发跨站竞速。source-2 若参与 A 站内竞速必被尝试。
	_, _, err = f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	// 不要求 err==nil：嵌套缺失时结果可能是权威兜底；RED 只看第二个 Source 是否被尝试。
	_ = err

	if hits2.Load() == 0 {
		t.Fatalf("RED：B 选中的 quay.io 未在其内部竞速第二个 Source（hits2=0）。" +
			"当前 replicaTargets 每 Registry 只取 picked[0]，A+B 站内竞速路径不存在（B08 NOT_IMPLEMENTED）")
	}
}

// ---------------------------------------------------------------------------
// 约束验证辅助断言（供重构阶段复用的不变量检查）
// ---------------------------------------------------------------------------

// TestCharHelper_ScopeBudgetObservable 证明 RequestScope 的 R/P 预算是可观测的，
// 后续嵌套重构可用它断言「每个 Source Attempt 进同一预算」而不是各自独立计数。
func TestCharHelper_ScopeBudgetObservable(t *testing.T) {
	f := newReplicaFixture(t, []byte(bodyValidManifest()), []byte(bodyValidManifest()))
	if f.sc.R() == nil || f.sc.P() == nil {
		t.Fatal("RequestScope 应暴露 R/P 预算")
	}
	// 初始未派发：R/P 计数为 0。
	if got := f.sc.R().Used(); got != 0 {
		t.Fatalf("初始 R.Used=%d，期望 0", got)
	}
	if got := f.sc.P().Committed(); got != 0 {
		t.Fatalf("初始 P.Committed=%d，期望 0", got)
	}
	// 单请求唯一 Claim 可观测。
	if got := f.sc.DispatchedClaims(); got != 0 {
		t.Fatalf("初始 DispatchedClaims=%d，期望 0", got)
	}
}

// bodyValidManifest 返回一个能通过 B 校验的合法 OCI Manifest 正文。
func bodyValidManifest() string {
	return `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`
}

// sha256Of 返回 hex 编码的 SHA-256（无 sha256: 前缀），供 ExpectSHA 使用。
func sha256Of(b []byte) string { return shaOf(b) }
