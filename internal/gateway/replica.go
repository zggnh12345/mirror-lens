package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/unified"
)

// 3.5.17 B 层网关入口：跨 Registry 可信副本。
//
// 分层（冻结）：
//   - trusted_registry_replicas 先解析「权威仓库 → 副本仓库」；
//   - 每个副本 Registry 再经 trusted_source_mappings 找可用 Source；
//   - 映射只授权尝试，不证明内容一致，最终仍以正文 Digest 为准。
//
// 两种模式（registry.replica_mode）：
//   - sequential：按 priority 固定顺序逐站尝试，首个可信正文交付（权威站先执行，
//     失败后按顺序访问 M 个可信副本——11.1 乙案对默认顺序模式的冻结口径）；
//   - race：首轮 C=2 个**副本**并发、200ms 投票窗口；两路一致直接交付；不一致先
//     顺序补位剩余副本，仍不能解决才用唯一一次 Authority。Authority 正文可作最终交付。
//
// 权威站与副本的角色必须分开（11.1 乙案）：权威 Registry 不属于可信副本集合，
// 不占 M 名额、不进入首轮 C、不占补位名额。它的 AuthorityAttempt 每请求每
// Identity 最多一次真实派发，软比对与最终兜底复用同一次结果。
//
// 本文件不改缓存身份与认证边界：副本候选共享权威 Identity 的**同一份提交权**
// （一个请求只有一个胜者、只写一个 CacheKey），其 Attempt 以权威仓库登记 Claim
// 身份，因此同一请求内只有一条 Claim Dispatched；但这不是「把不同 Registry 的
// Claim 合并成一条」——每个逻辑 Registry 仍有自己的 Identity Claim，见 1.6 §11.8。
// Auth 策略按候选自己的逻辑 Registry 解析。

// 冻结常量（1.6 §11.1）：口径来自设计冻结，不在调用方覆盖。
const (
	// crossReplicaM 是可信副本候选总数上限（权威站不占 M 名额）。
	crossReplicaM = 4
	// crossReplicaC 是跨站竞速首轮并发候选数，含于 M。
	crossReplicaC = 2
	// crossReplicaA 是单个 Registry 内部的 Source 并发上限（冻结稿 §4.2）。
	// 嵌套 A+B 双竞速时首轮物理并发峰值 C×A=4。
	crossReplicaA = 2
)

// replicaArtifact 是一次已完整读取并校验过的候选正文（临时文件）。
type replicaArtifact struct {
	tmp      string
	sha      string
	ctype    string
	etag     string
	lm       string
	upstream string
	size     int64
}

// recordReplicaSample 把一次已分类的副本回源样本原子写入持久化状态。
// 它绝不影响当前下载请求：写失败只记日志，保留最近一次成功状态。
// SourceID<=0（官方权威）或空映射身份时忽略，避免污染统计。
func (g *Gateway) recordReplicaSample(in store.DynamicSampleInput) {
	if g == nil || g.st == nil || in.SourceID <= 0 || in.RegistryHost == "" {
		return
	}
	active := g.st.ActiveMappingIdentities(g.now())
	// 双重确认：该映射当前确实有效，避免已撤销映射的迟到样本重新建状态。
	ok := false
	for _, id := range active {
		if id.Host == in.RegistryHost && id.SID == in.SourceID {
			ok = true
			break
		}
	}
	if !ok {
		return
	}
	if _, err := g.st.ApplyDynamicSample(in, active); err != nil {
		// 持久化失败不阻断下载，也不发布未持久化排名（内存状态由 Store 层
		// 保证不因写失败而对外发布；这里只记可诊断日志）。
		slog.Warn("动态排序样本持久化失败",
			"registry", in.RegistryHost, "source_id", in.SourceID, "err", err)
	}
}

// replicaTarget 是一个跨 Registry 候选。
type replicaTarget struct {
	u        *url.URL
	registry string // 该候选的逻辑 Registry host
	sourceID int64
	name     string
	// star 标记该 Source 已被星标（A 站内竞速资格）。
	star bool
	// alts 是该 Registry 内部可参与 A 站内竞速的其他星标合格 Source（最多
	// crossReplicaA-1 个）。只要该 Registry 的 A 开关打开即填充，与 B 的
	// race/sequential 模式无关（§4.2）。每个 Source 仍是同一 B 候选，不增加
	// M/投票名额。B 路径不新建 ChainA，避免与 ChainB 争用同一权威 Claim 被抑制。
	alts []replicaSourceAlt
}

// replicaSourceAlt 是同一副本 Registry 内的备选 Source。
type replicaSourceAlt struct {
	u        *url.URL
	sourceID int64
	name     string
}

// replicaModeTTL 是 UI 模式快照的保鲜期；切换由 API 立即失效，不靠 TTL。
const replicaModeTTL = 10 * time.Second

type replicaModeSnapshot struct {
	mode string
	at   time.Time
}

// replicaMode 解析当前生效的跨站模式：
//   - UI 设置（settings.cross_registry_mode）优先；
//   - 否则用部署期配置 registry.replica_mode；
//   - 两者都缺省时按 B17「默认顺序回退」（sequential）。空勾选使它在默认
//     安装上依然没有候选，因此不会改变既有行为。
func (g *Gateway) replicaMode() string {
	if g == nil {
		return ""
	}
	if v := g.replicaModeLive(); v != "" {
		if v == "off" {
			return "" // UI 显式关闭优先于部署期配置
		}
		return v
	}
	switch strings.ToLower(strings.TrimSpace(g.replica)) {
	case "race":
		return "race"
	case "sequential", "":
		return "sequential"
	}
	return "" // "off" 或无法识别 → 关闭
}

// replicaModeLive 读 UI 设置（带 10s 快照，避免每个 Manifest 请求查库）。
func (g *Gateway) replicaModeLive() string {
	if g == nil || g.st == nil {
		return ""
	}
	if v := g.replicaCfg.Load(); v != nil {
		if sn, ok := v.(*replicaModeSnapshot); ok && sn != nil && sn.mode != "" && g.now().Sub(sn.at) < replicaModeTTL {
			return sn.mode
		}
	}
	// 仅当 settings 有 B 字段时才缓存合成模式；三键全缺省时返回空，
	// 由 replicaMode() 走部署期 g.replica 分支（保证部署期原值变化即时生效）。
	if !g.st.HasBConfig() {
		return ""
	}
	cfg := g.st.ResolveBConfig(g.replica)
	// 新模型：enabled=false → "off"；enabled=true → mode（sequential/race）。
	// replicaMode() 主体据此保持旧 off 语义（返回空=关闭）不变。
	m := cfg.Mode
	if !cfg.Enabled {
		m = "off"
	}
	g.replicaCfg.Store(&replicaModeSnapshot{mode: m, at: g.now()})
	return m
}

// InvalidateReplicaMode 让模式切换即时生效（下一请求重新读设置）。
func (g *Gateway) InvalidateReplicaMode() {
	if g == nil {
		return
	}
	g.replicaCfg.Store((*replicaModeSnapshot)(nil))
}

// EffectiveReplicaMode 返回当前真正生效的跨站模式，供面板展示（只读）。
func (g *Gateway) EffectiveReplicaMode() string {
	return g.replicaMode()
}

// DeployReplicaMode 返回部署期 registry.replica_mode 基线原值，供 18a
// ResolveBConfig 在 settings 三键全缺省时作底层基线。不读 UI 设置、无副作用。
func (g *Gateway) DeployReplicaMode() string {
	if g == nil {
		return ""
	}
	return g.replica
}

// replicaEligible 判断本次 Manifest 请求是否进入跨 Registry 副本路径。
func (g *Gateway) replicaEligible(plan *adapter.Plan, prev *store.CacheEntry) bool {
	if g.replicaMode() == "" || plan == nil {
		return false
	}
	if plan.ResourceKind != adapter.ResourceManifest || !plan.Registry || !plan.Cacheable || plan.NoStore {
		return false
	}
	if prev != nil || plan.OfficialOnly || plan.FallbackChain || plan.Sequential {
		return false
	}
	return true
}

// replicaTargets 按「先副本映射、再 Source 映射」的顺序构造候选。
// 返回的 targets 第一个固定是权威官方源。
func (g *Gateway) replicaTargets(ctx context.Context, plan *adapter.Plan, now time.Time) ([]replicaTarget, uint64, error) {
	authority := rangeHostOf(plan)
	if authority == "" || plan.Repo == "" || plan.Ref == "" {
		return nil, 0, nil
	}
	// B19（3.5.15 对齐）：私有认证 Host 不进跨站候选，也不写共享缓存。
	// 私有站按缓存前分流走 OfficialOnly 路径，这里再挡一次，避免任何入口绕开。
	if g.privateRegistryHost(authority) {
		return nil, 0, nil
	}
	// 3.5.18 18b：候选来源按派发模式分离（冻结稿 §1.3/§1.6、反向测试 1/2/3/4）。
	//   - sequential：候选来自可信映射，不受参选集合限制（顺序回退不受参选勾选约束）；
	//   - race：候选还须满足 Source 星标 ∩ 跨站参选（方案二 Source 级求交）；
	//   - race + 空参选：无合格竞速候选，退化为 sequential 按可信映射顺序回退。
	// 无论哪种模式，候选都必须来自可信映射（trusted_registry_replicas +
	// trusted_source_mappings）且经调度器选中，仓库身份不靠同名/404 猜测。
	selected := g.st.EffectiveCrossSelected()
	mode := g.replicaMode()
	raceEligible := mode == "race" && len(selected) > 0
	reps, err := g.st.ActiveRegistryReplicas(authority, plan.Repo, now)
	if err != nil {
		return nil, 0, err
	}
	if len(reps) == 0 {
		return nil, 0, nil
	}
	// Generation 与候选同时取：代次只用于观测与 Scope 快照，不参与内容身份。
	gen, err := g.st.ReplicaGeneration(authority)
	if err != nil {
		return nil, 0, err
	}
	out := []replicaTarget{{u: plan.Upstream, registry: authority, sourceID: 0, name: "official"}}
	// M 只数副本：权威站在下标 0，不占 M 名额。
	replicas := 0
	for _, rep := range reps {
		if replicas >= crossReplicaM {
			break
		}
		// B19：副本侧若是私有认证 Host，既不进候选也不写共享缓存
		// （凭据只发给明确授权的官方 Registry/Realm）。
		if g.privateRegistryHost(rep.ReplicaRegistry) {
			continue
		}
		base := &url.URL{
			Scheme: "https",
			Host:   rep.ReplicaRegistry,
			Path:   "/v2/" + rep.ReplicaRepository + "/manifests/" + plan.Ref,
		}
		// 副本仍须经 trusted_source_mappings：没有显式可信 Source 的副本不尝试。
		maps, err := g.st.ActiveTrustedMappings(rep.ReplicaRegistry, now)
		if err != nil {
			return nil, gen, err
		}
		if len(maps) == 0 {
			continue
		}
		allowed := make(map[int64]bool, len(maps))
		for _, m := range maps {
			allowed[m.SourceID] = true
		}
		var cands []scheduler.Candidate
		if g.sched != nil && g.sched.Enabled() {
			cands = g.sched.Candidates("registry:" + rep.ReplicaRegistry)
		}
		rawN, rawErr := -1, error(nil)
		if g.st != nil {
			if raw, err := g.st.RegistryCandidatesRaw(rep.ReplicaRegistry); err != nil {
				rawN, rawErr = -2, err
			} else {
				rawN = len(raw)
			}
		}
		loggw(slog.LevelDebug, "副本候选原始查询", ctx, plan, "", rawErr,
			"replica", rep.ReplicaRegistry, "raw", rawN, "sched", len(cands),
			"sched_enabled", g.sched != nil && g.sched.Enabled())
		var picked []replicaTarget
		var skipped []string
		for _, c := range cands {
			if !allowed[c.ID] {
				skipped = append(skipped, fmt.Sprintf("%d:no-mapping", c.ID))
				continue
			}
			// 3.5.18 18b：仅 race 模式要求方案二 Source 级求交（星标 ∩ 参选）。
			// sequential 模式的候选来自可信映射，不受参选勾选限制；但两类模式
			// 都要求该 Source 已被调度器选中并经 Rewrite 成功（见下）。
			// A 站内竞速资格按星标记（§4.2 S(B-race)：调度器选中 ∩ 星标 ∩ 参选）。
			// race 模式还要求跨站参选勾选；sequential 模式不受参选限制。
			aEligible := c.Preferred && (!raceEligible || selected[c.ID])
			if raceEligible && !aEligible {
				skipped = append(skipped, fmt.Sprintf("%d:not-race-eligible(star=%v,sel=%v)", c.ID, c.Preferred, selected[c.ID]))
				continue
			}
			cc := c
			u, ok := g.sched.Rewrite("registry:"+rep.ReplicaRegistry, base, cc)
			if !ok || u == nil {
				skipped = append(skipped, fmt.Sprintf("%d:rewrite-failed(path=%q)", c.ID, cc.RegistryPath))
				continue
			}
			picked = append(picked, replicaTarget{u: u, registry: rep.ReplicaRegistry, sourceID: cc.ID, name: cc.Name, star: aEligible})
		}
		// 候选诊断：只在 debug 级别输出，逐副本给出每道门的结果。
		// 它不改变任何选择结果，只把「为什么没进候选」变成可复核的事实。
		loggw(slog.LevelDebug, "副本候选诊断", ctx, plan, "", nil,
			"replica", rep.ReplicaRegistry, "mappings", len(maps), "sched_candidates", len(cands),
			"preferred_selected", len(picked), "skipped", strings.Join(skipped, ","))
		// 同一副本 Registry 可能有多个可信 Source。该 Registry 仍只占一个 B
		// 候选/一个投票主体（避免同 Registry 多 Source 投两票）；但 3.5.18
		// 嵌套调度要求：只要该 Registry 的 A 站内竞速开关打开（与 B 的
		// race/sequential 模式无关，§4.2 两种模式内部都由 A 调度），就把
		// 这个 Registry 内部最多 crossReplicaA 个星标合格 Source 带进 alts，
		// 由内层竞速决定哪个 Source 交付。alts 必须是星标合格的 Source，
		// 且不能把未勾选 Source 带入 B race 请求（§4.2 S(B-race)）。
		if len(picked) > 0 {
			primary := picked[0]
			aOn := g.starRaceOn(rep.ReplicaRegistry)
			// 顺序模式下主 Source 可能不是星标；若 A 开且存在星标备选，
			// 优先把第一个星标 Source 提为主候选，保证 A 只在星标集合内竞速。
			if aOn && !primary.star {
				for i := 1; i < len(picked); i++ {
					if picked[i].star {
						primary = picked[i]
						break
					}
				}
			}
			if aOn {
				for i := 0; i < len(picked) && len(primary.alts) < crossReplicaA-1; i++ {
					cand := picked[i]
					if cand.sourceID == primary.sourceID || !cand.star {
						continue
					}
					primary.alts = append(primary.alts, replicaSourceAlt{
						u: cand.u, sourceID: cand.sourceID, name: cand.name,
					})
				}
			}
			out = append(out, primary)
			replicas++
		}
	}
	// 3.5.18 18c：动态排序只重排已进入 M 的副本执行顺序，不改成员/M/预算。
	// 条件：sort_mode=dynamic 且每条有效映射都达 50 样本门槛；否则固定顺序。
	if g.dynamicSortReady(now) {
		out = g.applyDynamicOrder(out, now)
	}
	return out, gen, nil
}

// dynamicSortReady 判断当前是否可以启用动态排序：
// sort_mode 必须为 dynamic，持久化状态有效，且每条有效映射都达到 50 样本门槛。
// 任一条件不满足（含状态损坏/版本不符/映射不足样本）都退回固定顺序。
func (g *Gateway) dynamicSortReady(now time.Time) bool {
	if g == nil || g.st == nil {
		return false
	}
	cfg := g.st.ResolveBConfig(g.replica)
	if cfg.SortMode != "dynamic" {
		return false
	}
	st, err := g.st.LoadDynamicState()
	if err != nil || len(st.Snapshot) == 0 {
		return false
	}
	active := g.st.ActiveMappingIdentities(now)
	if len(active) == 0 || len(st.Snapshot) < len(active) {
		return false
	}
	// 每条有效映射都必须达到门槛（valid_samples==50 且存在于快照）。
	ready := map[store.MappingIdentity]bool{}
	for _, e := range st.Snapshot {
		if e.ValidSamples >= store.DynamicSampleWindow() {
			ready[store.MappingIdentity{Host: e.RegistryHost, SID: e.SourceID}] = true
		}
	}
	for _, id := range active {
		if !ready[id] {
			return false
		}
	}
	return true
}

// applyDynamicOrder 按排名快照重排副本（下标 0 权威站固定不动）。
// 快照是 ComputeScores 按 Score 降序、并列固定顺序的结果；这里只按映射身份
// 映射到候选并重排副本段，无法映射的候选保持原相对位置。
func (g *Gateway) applyDynamicOrder(targets []replicaTarget, now time.Time) []replicaTarget {
	if len(targets) <= 1 {
		return targets
	}
	st, err := g.st.LoadDynamicState()
	if err != nil {
		return targets
	}
	rank := make(map[store.MappingIdentity]int, len(st.Snapshot))
	for i, e := range st.Snapshot {
		rank[store.MappingIdentity{Host: e.RegistryHost, SID: e.SourceID}] = i
	}
	official := targets[0]
	replicas := append([]replicaTarget(nil), targets[1:]...)
	sort.SliceStable(replicas, func(i, j int) bool {
		ki := store.MappingIdentity{Host: replicas[i].registry, SID: replicas[i].sourceID}
		kj := store.MappingIdentity{Host: replicas[j].registry, SID: replicas[j].sourceID}
		ri, oki := rank[ki]
		rj, okj := rank[kj]
		if oki && okj {
			return ri < rj
		}
		if oki != okj {
			return oki // 有快照的排在前面
		}
		return false // 都无快照：保持稳定（固定顺序兜底）
	})
	out := make([]replicaTarget, 0, len(targets))
	out = append(out, official)
	out = append(out, replicas...)
	return out
}

// fetchReplicaArtifact 完整下载一个候选正文到临时文件，计算 SHA-256 并做
// 与官方一致的内容完整性检查。返回的临时文件由调用方负责提交或删除。
// replicaSampleGuard 让嵌套竞速能在「某 Source 因同 Registry 另一 Source
// 胜出而被取消」时抑制其样本（F03 排除项），不影响单 Source 路径。
type replicaSampleGuard struct {
	mu       sync.Mutex
	suppress bool
}

func (g *replicaSampleGuard) suppressed() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.suppress
}

func (g *replicaSampleGuard) setSuppress() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.suppress = true
	g.mu.Unlock()
}

func (g *Gateway) fetchReplicaArtifact(ctx context.Context, plan *adapter.Plan, t replicaTarget, identityHost string) (replicaArtifact, int, error) {
	return g.fetchReplicaArtifactCore(ctx, plan, t, identityHost, nil)
}

func (g *Gateway) fetchReplicaArtifactCore(ctx context.Context, plan *adapter.Plan, t replicaTarget, identityHost string, guard *replicaSampleGuard) (replicaArtifact, int, error) {
	var art replicaArtifact
	// 3.5.18 18c：每个真实回源尝试最多贡献一个样本。deferred 统一在返回时
	// 按分类写入；成功须过 Manifest 校验（走到 art 提交才算 OK）。
	sample := store.DynamicSampleInput{
		RegistryHost: t.registry, SourceID: t.sourceID, TS: g.now().UnixNano(),
	}
	defer func() {
		// 嵌套竞速中被同 Registry 胜者取消的尝试不计样本（F03）。
		if guard.suppressed() {
			return
		}
		g.recordReplicaSample(sample)
	}()

	var dispatched bool
	up, err := g.openUpstreamAs(ctx, plan, nil, t.u, t.sourceID, t.registry, identityHost, &dispatched)
	if err != nil {
		// 只有真实派发后的失败（超时/无响应头）才计失败样本；派发前的
		// 策略/预算/认证拒绝不计（F03）。TTFB 不可测，不虚构。
		if t.sourceID > 0 && dispatched {
			sample.OK = false
			sample.TTFBMeasurable = false
		} else {
			sample = store.DynamicSampleInput{} // 空 SourceID → 采集器忽略
		}
		// 候选派发诊断（debug）：把「谁真的发了、结果是什么」写成可复核的行。
		// 官方权威（sourceID=0）会以 target=official 出现，因此「同意组没有
		// 派发官方」可以由日志逐请求验证，而不是靠推理。
		loggw(slog.LevelDebug, "副本候选失败", ctx, plan, "", err,
			"target", t.name, "registry", t.registry, "source_id", t.sourceID)
		return art, 0, err
	}
	resp := up.resp
	// 收到响应头 → TTFB 可测（冻结口径：终点=收到响应头）。
	sample.TTFBMeasurable = true
	sample.TTFBMS = up.ttfbMS
	defer func() {
		resp.Body.Close()
		up.cancel()
	}()
	if resp.StatusCode != http.StatusOK {
		// 有响应头的失败（404/401/429/5xx）：TTFB 参与 P50；成功率为 0。
		loggw(slog.LevelDebug, "副本候选非 200", ctx, plan, "", nil,
			"target", t.name, "registry", t.registry, "status", resp.StatusCode)
		io.CopyN(io.Discard, resp.Body, 4096)
		err := &upstreamError{
			Code:       resp.StatusCode,
			RetryAfter: retryAfterHeader(resp.Header),
			Challenge:  resp.Header.Get("WWW-Authenticate"),
		}
		up.finishAttempt(resp.StatusCode, err)
		return art, resp.StatusCode, err
	}
	if plan.Validate != nil {
		if verr := plan.Validate(resp); verr != nil {
			up.finishAttempt(0, verr)
			return art, 0, verr
		}
	}
	if resp.ContentLength > g.maxSize {
		up.finishAttempt(0, &tooLargeError{Max: g.maxSize})
		return art, 0, &tooLargeError{Max: g.maxSize}
	}
	tmp, err := g.cache.CreateTemp()
	if err != nil {
		up.finishAttempt(0, err)
		return art, 0, err
	}
	name := tmp.Name()
	fail := func(e error) (replicaArtifact, int, error) {
		tmp.Close()
		removeFile(name)
		g.cache.ForgetTemp(name)
		up.finishAttempt(0, e)
		return replicaArtifact{}, 0, e
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, g.maxSize+1))
	if err != nil {
		return fail(fmt.Errorf("读取候选正文中断: %w", err))
	}
	if n > g.maxSize {
		return fail(&tooLargeError{Max: g.maxSize})
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return fail(fmt.Errorf("长度不符：期望 %d，实际 %d", resp.ContentLength, n))
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		removeFile(name)
		g.cache.ForgetTemp(name)
		up.finishAttempt(0, err)
		return replicaArtifact{}, 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	// 内容寻址请求（/manifests/sha256:…）必须逐字节等于客户端指定的摘要。
	// 副本只被授权「尝试」，绝不因为来自可信副本就跳过 Digest 校验；
	// 这条一旦漏掉，错误映射会被 Immutable 缓存永久命中。
	if plan.ExpectSHA != "" && !strings.EqualFold(sum, plan.ExpectSHA) {
		removeFile(name)
		g.cache.ForgetTemp(name)
		err := fmt.Errorf("内容与请求摘要不符：期望 %s 实得 %s (%s)", plan.ExpectSHA, sum, t.u)
		g.verifyFail.Add(1)
		up.finishAttempt(0, err)
		return replicaArtifact{}, 0, err
	}
	if checked, ok := checkDigest(resp.Header, sum); checked && !ok {
		removeFile(name)
		g.cache.ForgetTemp(name)
		err := fmt.Errorf("内容与上游声明的摘要不符：%s", t.u)
		g.verifyFail.Add(1)
		up.finishAttempt(0, err)
		return replicaArtifact{}, 0, err
	}
	art = replicaArtifact{
		tmp:      name,
		sha:      sum,
		ctype:    resp.Header.Get("Content-Type"),
		etag:     resp.Header.Get("ETag"),
		lm:       resp.Header.Get("Last-Modified"),
		upstream: t.u.String(),
		size:     n,
	}
	up.finishAttempt(http.StatusOK, nil)
	// 走到这里：正文完整读取、长度/Digest/身份全部校验通过 → 成功样本。
	sample.OK = true
	return art, http.StatusOK, nil
}

// fetchRegistryArtifact 在一个副本 Registry 内部执行嵌套 A 站内竞速。
//
// 冻结语义（§4.2/§4.3/§18d）：
//   - 该 Registry 仍是 B 的一个候选、一个投票主体；alts 只决定内部谁交付；
//   - 所有 Source Attempt 都在调用方已绑定的 ChainB 上，共享权威 Claim、
//     进同一 RequestScope 的 R/P 预算，不新建 ChainA（否则会与 ChainB 争用
//     同一 Claim 而被 errUnifiedSuppressed 抑制）；
//   - Source 失败立即补位（这里并发启动，等价于失败即由其他 Source 顶上）；
//   - 第一个通过完整校验的正文胜出；胜者确定后取消其余 Source，被取消者
//     不计样本（F03），并清理临时文件与在途资源；
//   - 全部 Source 失败 → 该 Registry 整体失败，交回外层 B 继续补位。
func (g *Gateway) fetchRegistryArtifact(ctx context.Context, plan *adapter.Plan, t replicaTarget, identityHost string) (replicaArtifact, int, error) {
	if len(t.alts) == 0 {
		return g.fetchReplicaArtifact(ctx, plan, t, identityHost)
	}

	sources := make([]replicaTarget, 0, 1+len(t.alts))
	sources = append(sources, t)
	for _, a := range t.alts {
		sources = append(sources, replicaTarget{
			u: a.u, registry: t.registry, sourceID: a.sourceID, name: a.name,
		})
	}
	if len(sources) > crossReplicaA {
		sources = sources[:crossReplicaA]
	}

	type innerResult struct {
		idx  int
		art  replicaArtifact
		code int
		err  error
	}
	n := len(sources)
	ch := make(chan innerResult, n)
	guards := make([]*replicaSampleGuard, n)
	cancels := make([]context.CancelFunc, n)

	for i, src := range sources {
		guards[i] = &replicaSampleGuard{}
		sctx, cancel := context.WithCancel(ctx)
		cancels[i] = cancel
		idx, s, gd := i, src, guards[i]
		go func() {
			// 每个内层 Source 用独立的 plan 浅拷贝：fetchReplicaArtifactCore
			// 内 loggw 会读 plan.Source，而外层胜者返回后会写 plan.Source。
			// 共享同一 *Plan 会与仍在收尾的输家 goroutine 产生 DATA RACE
			// （实测 replica.go:968 写 vs loggw 读）。内层只读 plan，浅拷贝
			// 即可隔离该字段；不改任何被共享的引用型字段内容。
			pp := *plan
			art, code, err := g.fetchReplicaArtifactCore(sctx, &pp, s, identityHost, gd)
			ch <- innerResult{idx: idx, art: art, code: code, err: err}
		}()
	}

	// 胜者判定：第一个「完整校验通过（code==200 且无 err）」的结果。
	// 其余先到的失败结果不提前结束整场——等在途 Source 或全部失败。
	var winner *innerResult
	var lastErr error
	remaining := n
	for remaining > 0 && winner == nil {
		r := <-ch
		remaining--
		if r.err == nil && r.code == http.StatusOK {
			w := r
			winner = &w
			continue
		}
		lastErr = r.err
	}

	cleanupLosers := func(keep int) {
		// 先给所有输家打抑制标记，再取消 context：顺序反了会让被取消者的
		// deferred 样本采集抢在抑制之前发生，违反 F03「因其他候选胜出取消
		// 不计样本」。已完成的真实失败样本在消费结果时已经记录，不受影响。
		for i := range sources {
			if i == keep {
				continue
			}
			guards[i].setSuppress()
		}
		for i := range sources {
			cancels[i]()
		}
	}

	if winner == nil {
		cleanupLosers(-1)
		if lastErr == nil {
			lastErr = fmt.Errorf("Registry %s 内所有 Source 失败", t.registry)
		}
		return replicaArtifact{}, 0, lastErr
	}

	cleanupLosers(winner.idx)
	// 收集迟到结果并清理其临时文件（迟到成功也可能落了 temp）。
	go func() {
		for i := 0; i < remaining; i++ {
			r := <-ch
			if r.art.tmp != "" {
				removeFile(r.art.tmp)
				g.cache.ForgetTemp(r.art.tmp)
			}
		}
	}()
	return winner.art, winner.code, nil
}

// commitReplicaArtifact 把已校验的候选正文提交为权威缓存条目。
func (g *Gateway) commitReplicaArtifact(ctx context.Context, plan *adapter.Plan, art replicaArtifact) (*store.CacheEntry, error) {
	e := &store.CacheEntry{
		Key:          plan.CacheKey,
		SHA256:       art.sha,
		Size:         art.size,
		ContentType:  art.ctype,
		ETag:         art.etag,
		LastModified: art.lm,
		Upstream:     art.upstream,
		CreatedAt:    g.now().Unix(),
		TTL:          entryTTL(plan),
		Verify:       verifyState(plan),
	}
	unlock := g.cache.LockBlob(art.sha)
	if _, err := g.cache.Commit(art.tmp, art.sha); err != nil {
		unlock()
		removeFile(art.tmp)
		g.cache.ForgetTemp(art.tmp)
		return nil, fmt.Errorf("提交缓存失败: %w", err)
	}
	err := g.st.CachePut(*e)
	unlock()
	if err != nil {
		loggw(slog.LevelError, "写缓存索引失败", ctx, plan, logging.CacheCommitFailed, err)
	}
	g.st.RecordTraffic(store.TrafficFill, store.KindOfKey(e.Key), art.size)
	return e, nil
}

// mutableTagRef 判断这次 Manifest 引用是可变 Tag（不是内容寻址 Digest）。
// 可变 Tag 在没有版本一致性证明前不能共享逻辑身份（B.3/B.6），也不能把副本
// 正文直接写进权威 Tag 的缓存键。
func mutableTagRef(plan *adapter.Plan) bool {
	if plan == nil {
		return false
	}
	if plan.ExpectSHA != "" {
		return false
	}
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(plan.Ref)), "sha256:")
}

// crossFlightKey 把副本执行模式并进 singleflight 键。
//
// A 的 registry-race、B 的顺序与竞速是三个互不复用的执行态（与 FlightPolicy
// 命名空间同理）：共用同一个 Flight 会让不同策略的结果串味（顺序模式的结果
// 被竞速请求复用，反之亦然）。CacheKey 本身不改，缓存身份仍由权威 plan 决定。
func crossFlightKey(mode string, plan *adapter.Plan) string {
	if plan == nil {
		return ""
	}
	if mode != "race" && mode != "sequential" {
		mode = "off"
	}
	return "cross-registry-" + mode + "|" + plan.CacheKey
}

// fetchWithReplicas 是 serveManifest 的唯一入口：副本路径没有形成结论时
// 无条件回到 fetchOnce，保证旧行为不被削弱。
// replicaFlight 让同一 CacheKey 的并发请求共享一次跨 Registry 竞速。
// 没有它，冷 tag 被 K 个客户端同时拉会放大成 K 轮竞速（每轮 2~M 次回源）。
type replicaFlight struct {
	done   chan struct{}
	entry  *store.CacheEntry
	notMod bool
	err    error
}

func (g *Gateway) fetchWithReplicas(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry) (*store.CacheEntry, bool, error) {
	if !g.replicaEligible(plan, prev) {
		return g.fetchOnce(ctx, plan, prev)
	}
	// 副本路径与它的顺序回退必须落在同一条 B 链：竞速阶段一旦派发过，
	// F19 的 Claim 就是 dispatched，换链顺序回退会被直接抑制（502）。
	ctx = withUnifiedChain(ctx, unified.ChainB)
	key := crossFlightKey(g.replicaMode(), plan)
	nf := &replicaFlight{done: make(chan struct{})}
	actual, loaded := g.replicaFlights.LoadOrStore(key, nf)
	f := actual.(*replicaFlight)
	if loaded {
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		if f.err != nil || f.entry == nil {
			// 领跑者的失败（含客户端取消/panic）不能传染给其他请求：各自走旧路径。
			return g.fetchOnce(ctx, plan, prev)
		}
		return f.entry, f.notMod, nil
	}
	defer func() {
		g.replicaFlights.Delete(key)
		close(f.done)
	}()
	if e, handled, err := g.crossRegistryManifest(ctx, plan, prev); handled {
		f.entry, f.err = e, err
		return e, false, err
	}
	e, notMod, err := g.fetchOnce(ctx, plan, prev)
	f.entry, f.notMod, f.err = e, notMod, err
	return e, notMod, err
}

// crossRegistryManifest 执行跨 Registry 副本路径。
// handled=false 表示没有形成结论，调用方必须继续原有顺序路径。
func (g *Gateway) crossRegistryManifest(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry) (*store.CacheEntry, bool, error) {
	if !g.replicaEligible(plan, prev) {
		return nil, false, nil
	}
	idHost := rangeHostOf(plan)

	// 18f：本次跨站调度的运行时状态；用 defer 统一落环形缓冲，覆盖所有返回路径。
	rec := CrossRunState{
		At:          g.now(),
		Mode:        g.replicaMode(),
		RequestHint: sanitizeHint(plan.CacheKey),
		Outcome:     "fail",
		CachePut:    "skipped",
	}
	defer func() { g.recordCrossRun(rec) }()

	targets, gen, err := g.replicaTargets(ctx, plan, g.now())
	rec.Generation = gen
	if err != nil {
		// 配置读取失败不阻断主路径：副本是加速收益，不是可用性前提。
		loggw(slog.LevelWarn, "副本候选解析失败，回到顺序路径", ctx, plan, logging.CacheCommitFailed, err)
		rec.Outcome = "fail"
		return nil, false, nil
	}
	if len(targets) < 2 {
		return nil, false, nil
	}
	rec.Candidates = len(targets) - 1 // 权威站 targets[0] 不占候选名额
	for _, t := range targets {
		if len(t.alts) > 0 {
			rec.Nested = true
			rec.Sources += 1 + len(t.alts)
		}
	}
	_ = gen

	// 3.5.18 18b：race + 空参选 → 无合格竞速候选，退化为按可信映射的
	// sequential 顺序回退（冻结稿 §1.6、反向测试 2/4）。退化只改变派发
	// 形态，不降低任何安全门禁：候选仍来自可信映射、仍经 fetchReplicaArtifact
	// 执行派发前仓库身份检查与响应后 B.3 三方 Digest 校验。
	mode := g.replicaMode()
	if mode == "race" && len(g.st.EffectiveCrossSelected()) == 0 {
		mode = "sequential"
	}
	rec.Mode = mode // 记录实际生效模式（race 空参选已退化为 sequential），保证派发状态准确

	if mode == "sequential" {
		for _, t := range targets {
			// A ON + B sequential：Registry 内部仍由 A 调度（§4.2 主链并发=A=2）。
			art, code, err := g.fetchRegistryArtifact(ctx, plan, t, idHost)
			if err != nil {
				_ = code
				continue
			}
			e, cerr := g.commitReplicaArtifact(ctx, plan, art)
			if cerr != nil {
				rec.CachePut = "rejected"
				rec.Outcome = "fail"
				return nil, true, cerr
			}
			rec.CachePut = "committed"
			rec.Outcome = "ok"
			rec.Registry = t.registry
			rec.WinnerID = t.sourceID
			rec.Digest = art.sha
			plan.Source = t.name
			return e, true, nil
		}
		return nil, false, nil // 全部副本失败 → 交回顺序路径
	}

	// race 模式：每个候选只下载一次，产物保留到结论确定。
	type slot struct {
		once      sync.Once
		done      chan struct{}
		art       replicaArtifact
		sha       string
		err       error
		abandoned bool
	}
	slots := make([]*slot, len(targets))
	for i := range slots {
		slots[i] = &slot{done: make(chan struct{})}
	}
	var mu sync.Mutex
	consumed := map[int]bool{}
	run := func(i int) (string, error) {
		slots[i].once.Do(func() {
			// A ON + B race：每个 Registry 内部再跑 A 站内竞速（C×A=4），
			// 但该 Registry 仍是一个 B 候选/一个投票主体。内层所有 Source
			// Attempt 在同一 ChainB/Claim 上，共享 RequestScope 预算。
			art, _, err := g.fetchRegistryArtifact(ctx, plan, targets[i], idHost)
			mu.Lock()
			if err == nil {
				slots[i].art = art
				slots[i].sha = art.sha
			}
			slots[i].err = err
			late := slots[i].abandoned
			// done 在锁内关闭：与 dropAll 的「取产物 / 标记放弃」二选一
			// 在同一把锁下互斥，避免交接窗口里两头都不清理。
			close(slots[i].done)
			mu.Unlock()
			if late && art.tmp != "" {
				// 竞速已在窗口外放弃该候选：迟到产物自己清理，不留孤儿文件。
				removeFile(art.tmp)
				g.cache.ForgetTemp(art.tmp)
			}
		})
		<-slots[i].done
		if slots[i].err != nil {
			return "", slots[i].err
		}
		return slots[i].sha, nil
	}
	dropAll := func(keep int) {
		for i := range slots {
			if i == keep {
				continue
			}
			mu.Lock()
			select {
			case <-slots[i].done:
				// 已完成的候选：产物稳定（close(done) 在锁内，建立 HB）。
				art := slots[i].art
				mu.Unlock()
				if art.tmp != "" {
					removeFile(art.tmp)
					g.cache.ForgetTemp(art.tmp)
				}
			default:
				// 还在跑的候选（窗口外丢弃）：标记后由它自己清理。
				slots[i].abandoned = true
				mu.Unlock()
			}
		}
	}

	cands := make([]unified.CrossCandidate, 0, len(targets)-1)
	// 只有副本进首轮 C 与补位：权威站独立于 M，不占 M 名额、不进 C、
	// 不占补位名额（11.1 乙案）。targets[0] 是权威站，副本从下标 1 开始。
	for j := 1; j < len(targets); j++ {
		slotIdx := j
		cands = append(cands, unified.CrossCandidate{
			Registry: targets[j].registry,
			SourceID: targets[j].sourceID,
			Run: func(rc context.Context) (string, error) {
				return run(slotIdx)
			},
			Abort: func() {},
		})
	}
	// Authority 唯一一次真实派发：复用权威站的槽位（run 由 sync.Once 记忆化，
	// 同一请求最多一次 Dispatched；软比对与最终兜底共用这次结果）。
	cfg := unified.CrossRaceConfig{
		MaxCandidates: crossReplicaM,
		FirstRound:    crossReplicaC,
		Authority: func(rc context.Context) (string, error) {
			return run(0)
		},
	}
	out, rerr := unified.RunCrossRace(ctx, cfg, cands)
	rec.Authority = out.AuthorityUsed
	rec.VoteState = out.State.String()
	rec.Backfill = out.Truncated // 首轮外候选（由补位机制处理）的近似计数
	// 窗口外仍未完成的候选：标记放弃，让它带着自己的产物退出。
	for i := range slots {
		select {
		case <-slots[i].done:
		default:
			rec.Canceled++
			mu.Lock()
			slots[i].abandoned = true
			mu.Unlock()
		}
	}
	if out.Winner >= 0 {
		// out.Winner 是副本下标（cands 不含权威站），槽位要 +1 映射回去。
		wi := out.Winner + 1
		if wi >= len(slots) {
			dropAll(-1)
			return nil, false, nil
		}
		// B03：可变 Tag 没有内容寻址身份，必须取得版本一致性才能把副本正文
		// 写进权威 Tag 的缓存键。一致性由本请求唯一一次 AuthorityAttempt
		// 对照（run(0) 已记忆化，不会二次派发）；不一致就交回顺序路径。
		if mutableTagRef(plan) {
			ad, aerr := run(0)
			if aerr != nil || ad == "" || !strings.EqualFold(ad, slots[wi].sha) {
				dropAll(-1)
				return nil, false, nil
			}
		}
		art := slots[wi].art
		consumed[wi] = true
		if targets[wi].sourceID > 0 {
			plan.Source = targets[wi].name
		}
		dropAll(wi)
		rec.Registry = targets[wi].registry
		rec.WinnerID = targets[wi].sourceID
		rec.Digest = art.sha
		rec.Outcome = "ok"
		e, cerr := g.commitReplicaArtifact(ctx, plan, art)
		if cerr != nil {
			rec.CachePut = "rejected"
			return nil, true, cerr
		}
		rec.CachePut = "committed"
		return e, true, nil
	}
	// 不一致/无结论：权威正文若已校验，可以作最终交付（design §4.4）。
	mu.Lock()
	var authArt replicaArtifact
	select {
	case <-slots[0].done:
		authArt = slots[0].art
	default:
		// 权威那条还没回：没有可用于兜底的已验证正文。
	}
	mu.Unlock()
	if authArt.tmp != "" && authArt.sha != "" {
		art := authArt
		consumed[0] = true
		dropAll(0)
		rec.Registry = targets[0].registry
		rec.WinnerID = 0 // 权威兜底
		rec.Digest = art.sha
		rec.Outcome = "fallback"
		rec.Authority = true
		e, cerr := g.commitReplicaArtifact(ctx, plan, art)
		if cerr != nil {
			rec.CachePut = "rejected"
			return nil, true, cerr
		}
		rec.CachePut = "committed"
		return e, true, nil
	}
	dropAll(-1)
	_ = rerr
	return nil, false, nil
}
