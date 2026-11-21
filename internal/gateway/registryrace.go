// Registry Manifest 竞速（3.5.16 A：同 Registry 星标 Source 竞速）。
//
// 三层职责必须分开，互不替代：
//  1. 人工策略：Source 星标（Preferred）+ Host 级 star_race 总开关；
//  2. 技术能力：source_host_caps 的 effective_status / path_mode + 健康与恢复 TTL；
//  3. 执行态：FlightKey 策略命名空间、每 attempt 独立 Context、并发 Lease。
//
// 执行层只认一个判定结果（adapter.Plan.RaceReady），不自己再推一遍资格：
// 判定点唯一，避免「入口说能竞速、执行层说不能」的两套标准。
package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/registryhost"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/unified"
)

const (
	// flightPolicyRace / flightPolicyNoRace 是 FlightKey 的执行策略命名空间。
	// 空值是原有的默认执行策略（直接用 plan.CacheKey）。
	flightPolicyRace   = "registry-race"
	flightPolicyNoRace = "registry-no-race"

	raceLayerSameHost = "same_host"
)

// errRaceNoCandidate 表示没有两个合格镜像源，竞速不成立。
var errRaceNoCandidate = errors.New("registry race: candidate set incomplete")

// errRaceNoBudget 表示并发预算不足，第二路（或整场）不启动。
var errRaceNoBudget = errors.New("registry race: no concurrency budget")

// flightKeyFor 把执行策略并进 singleflight 键。
//
// 隔离边界是「竞速执行态」而不是 HTTP 方法：竞速关闭、或开启但合格源不足
// 两个时，plan.FlightPolicy 为空，GET 与 HEAD 继续共享 plan.CacheKey 的
// 顺序 Flight（与基线完全一致）；只有真正形成竞速时 GET 才用独立 Race
// Flight，同资源的 HEAD 走 registry-no-race 与之隔离。
func flightKeyFor(plan *adapter.Plan) string {
	if plan == nil {
		return ""
	}
	if plan.FlightPolicy == "" {
		return plan.CacheKey
	}
	return plan.CacheKey + "\x00policy=" + plan.FlightPolicy
}

// ---------------------------------------------------------------------------
// Host 级总开关（settings 表）的内存快照
// ---------------------------------------------------------------------------

type starRaceSnapshot struct {
	hosts map[string]bool
	at    time.Time
}

// starRaceTTL 是快照的最长保鲜期。热路径不读库，改开关后由 API 主动
// 调 InvalidateStarRace 立即生效；TTL 只是「别处改了库」的兜底。
const starRaceTTL = 10 * time.Second

func (g *Gateway) starRaceSnapshot() *starRaceSnapshot {
	if g == nil || g.st == nil {
		return &starRaceSnapshot{hosts: map[string]bool{}}
	}
	if v := g.raceCfg.Load(); v != nil {
		if sn, ok := v.(*starRaceSnapshot); ok && sn.hosts != nil && g.now().Sub(sn.at) < starRaceTTL {
			return sn
		}
	}
	g.raceMu.Lock()
	defer g.raceMu.Unlock()
	if v := g.raceCfg.Load(); v != nil {
		if sn, ok := v.(*starRaceSnapshot); ok && sn.hosts != nil && g.now().Sub(sn.at) < starRaceTTL {
			return sn
		}
	}
	sn := &starRaceSnapshot{hosts: g.st.StarRaceHosts(), at: g.now()}
	g.raceCfg.Store(sn)
	return sn
}

// InvalidateStarRace 在 Host 开关变化后清掉快照，下一次请求立刻读新值。
func (g *Gateway) InvalidateStarRace() {
	if g == nil {
		return
	}
	g.raceCfg.Store(&starRaceSnapshot{at: time.Time{}})
}

func (g *Gateway) starRaceOn(rawHost string) bool {
	h, err := registryhost.Normalize(rawHost)
	if err != nil || h == "" {
		return false
	}
	return g.starRaceSnapshot().hosts[h]
}

// StarRaceHosts 供管理 API 展示当前开关状态。
func (g *Gateway) StarRaceHosts() map[string]bool {
	if g == nil || g.st == nil {
		return map[string]bool{}
	}
	return g.st.StarRaceHosts()
}

// ---------------------------------------------------------------------------
// 准入判定
// ---------------------------------------------------------------------------

// raceHost 返回 plan 归属的规范化 Registry Host。
// docker.io / registry-1.docker.io / index.docker.io 收敛成同一个键。
func raceHost(plan *adapter.Plan) string {
	if plan == nil {
		return ""
	}
	h, err := registryhost.Normalize(plan.LogicalHost)
	if err != nil {
		return ""
	}
	return h
}

// raceEligibleTargets 从 Scheduler 候选里筛出「合格镜像 Source」。
//
// 顺序即 Scheduler 排序，最多两个；官方源（c == nil）永远不进这个集合，
// 备用站点（fb）也不进——它们不是同 Host 的镜像 Source。
// 候选查询本身已经按 source_host_caps（enabled / Supported / path_mode /
// realm policy）和 registry_host_recovery（state=ok、gen、TTL、
// source_health_scope 无失败）过滤，这里只补人工策略（星标）与路径校验。
func (g *Gateway) raceEligibleTargets(plan *adapter.Plan) []fetchTarget {
	if g == nil || g.sched == nil || plan == nil || plan.Upstream == nil {
		return nil
	}
	if !g.sched.Enabled() {
		return nil
	}
	cands := g.sched.CandidatesN(plan.Sched(), 8)
	out := make([]fetchTarget, 0, 2)
	for _, c := range cands {
		if !c.Preferred {
			continue
		}
		cc := c
		if cc.RegistryPath != "" && cc.RegistryPath != "native" && cc.RegistryPath != "host_prefix" {
			continue
		}
		u, ok := g.sched.Rewrite(plan.Sched(), plan.Upstream, cc)
		if !ok || strings.EqualFold(u.Host, plan.Upstream.Host) {
			continue
		}
		out = append(out, fetchTarget{u: u, c: &cc})
		if len(out) == 2 {
			break
		}
	}
	return out
}

// raceFormable 只回答「这个请求此刻是否具备竞速条件」，并把未获资格的原因
// 记进指标（reason 是封闭枚举，避免高基数）。
func (g *Gateway) raceFormable(plan *adapter.Plan, hasPrev bool) bool {
	if plan == nil || !plan.Registry || plan.ResourceKind != adapter.ResourceManifest {
		metrics.RegistryRaceIneligible("other")
		return false
	}
	switch {
	case hasPrev:
		metrics.RegistryRaceIneligible("revalidate")
		return false
	case plan.OfficialOnly:
		metrics.RegistryRaceIneligible("official_only")
		return false
	case !plan.Immutable || plan.ExpectSHA == "":
		// T43：可变 Tag 一律不进竞速。竞速按先到取胜，第三方镜像站的缓存
		// 命中会把 tag 钉在旧版本，而 A 没有 B 的跨站 Digest 三方校验。
		metrics.RegistryRaceIneligible("not_digest")
		return false
	case g.sched == nil || !g.sched.Enabled():
		metrics.RegistryRaceIneligible("sched_off")
		return false
	case !g.starRaceOn(raceHost(plan)):
		metrics.RegistryRaceIneligible("host_off")
		return false
	}
	elig := g.raceEligibleTargets(plan)
	if len(elig) == 0 {
		metrics.RegistryRaceIneligible("no_star")
		return false
	}
	if len(elig) < 2 {
		metrics.RegistryRaceIneligible("one_candidate")
		return false
	}
	return true
}

// evaluateRegistryRace 是唯一的竞速准入判定点，由 serveManifest 在回源前调用。
//
// HEAD 一律强制顺序执行（plan.Sequential），且不得加入 GET 的竞速 Flight；
// 若该资源本身具备竞速条件，HEAD 走 registry-no-race 命名空间与 Race Flight
// 隔离。GET 只有在具备条件时才把 plan.RaceReady 置真。
func (g *Gateway) evaluateRegistryRace(plan *adapter.Plan, method string, hasPrev bool) {
	if plan == nil || !plan.Registry || plan.ResourceKind != adapter.ResourceManifest {
		return
	}
	plan.RaceReady = false
	plan.FlightPolicy = ""
	if method == http.MethodHead {
		// 客户端 HEAD 仍走上游 GET 取完整 Manifest、照常校验与缓存，
		// 只是禁止一切并行（通用 Hedge 与 Registry Race）。
		plan.Sequential = true
		metrics.RegistryRaceIneligible("head")
		if g.raceFormable(plan, hasPrev) {
			plan.FlightPolicy = flightPolicyNoRace
		}
		return
	}
	if !g.raceFormable(plan, hasPrev) {
		return
	}
	plan.RaceReady = true
	plan.FlightPolicy = flightPolicyRace
}

// ---------------------------------------------------------------------------
// 竞速执行
// ---------------------------------------------------------------------------

// raceAfter 是「第一候选还没交响应头就启动第二候选」的耐心预算。
// 默认复用通用 Hedge 的 after，可用 Registry 专属配置覆盖；两者语义不同
// （一个是任意请求的响应头对冲，一个是 Manifest 竞速），所以不共用同一个值。
func (g *Gateway) raceAfter() time.Duration {
	if g.race.After > 0 {
		return g.race.After
	}
	if g.hedge.After > 0 {
		return g.hedge.After
	}
	return time.Second
}

type raceResult struct {
	slot   int
	up     *upstream
	start  time.Time
	err    error
	dialed bool // 该 slot 是否真正进入了 openUpstream 调用边界（区别于仅启动 goroutine）
}

// raceManifest 在同 Host 的两个合格镜像 Source 之间竞速，返回胜者的响应头。
//
// dialedOut（调用方传入长度 n 的切片）在返回时按 slot 填好「该路是否真正
// 进入 openUpstream」。它让调用方只回滚未拨号候选的 tried：预算跳过时
// started=true 但 dialed=false，不能混为一谈。填充只发生在主 goroutine
// 收到该 slot 的 raceResult 之后（channel 建立 happens-before），不读并发写。
//
// 约束（与 7.1/7.2/3.8 对应）：
//   - 每个 attempt 独立 Context，取消一个不会掐断另一个的 Body；
//   - 两路都要先拿到并发 Lease，拿不到就不形成双路（T31）；
//   - 只交出响应头胜者，正文由调用方 saveBody 校验，校验通过才提交缓存；
//   - 返回时保证未胜出的 attempt 已被取消并关闭 Body，不留 goroutine。
func (g *Gateway) raceManifest(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry, pair []fetchTarget, dialedOut []bool) (*upstream, int, time.Time, error) {
	n := len(pair)
	if n < 2 {
		return nil, -1, time.Time{}, errRaceNoCandidate
	}
	for i := range dialedOut {
		dialedOut[i] = false
	}
	// 3.5.17：A 站内竞速的所有 Attempt 属于同一条 A 链，因此共享同一个
	// Claim（同 Identity 多 Source 可并发，不重复 Dispatched）。
	ctx = withUnifiedChain(ctx, unified.ChainA)
	ch := make(chan raceResult, n)
	done := make(chan struct{})
	taken := make([]chan struct{}, n)
	takenOnce := make([]sync.Once, n)
	cancels := make([]context.CancelFunc, n)
	started := make([]bool, n)

	// 每路一个只执行一次的收尾：Body 关闭后归还 Lease。
	var releaseOnce []sync.Once
	releaseOnce = make([]sync.Once, n)
	releases := make([]func(), n)
	doRelease := func(slot int) {
		releaseOnce[slot].Do(func() {
			if releases[slot] != nil {
				releases[slot]()
			}
		})
	}
	markTaken := func(slot int) {
		if taken[slot] != nil {
			takenOnce[slot].Do(func() { close(taken[slot]) })
		}
	}

	launch := func(slot int) bool {
		if started[slot] {
			return false
		}
		started[slot] = true
		actx, acancel := context.WithCancel(ctx)
		cancels[slot] = acancel
		taken[slot] = make(chan struct{})

		t := pair[slot]
		// 第二路必须在拨号之前拿到并发许可；第一路同理，拿不到就整场不竞速，
		// 让调用方回到基线的顺序路径（基线 Manifest 回源不占 Lease）。
		var lease *scheduler.Lease
		if t.c != nil && g.sched != nil && g.sched.Enabled() {
			l, err := g.sched.TryAcquireLease(actx, t.c.ID, scheduler.KindNormal)
			if err != nil {
				acancel()
				ch <- raceResult{slot: slot, err: err, dialed: false}
				return true
			}
			lease = l
			if lease == nil {
				metrics.RegistryRaceBudgetSkip()
				acancel()
				ch <- raceResult{slot: slot, err: errRaceNoBudget, dialed: false}
				return true
			}
			releases[slot] = func() { lease.Release(lease.Token()) }
		}
		if slot > 0 {
			// 第二路才是竞速带来的额外上游请求；第一路本来就要发。
			metrics.RegistryRaceExtra()
		}
		go func() {
			st := time.Now()
			// dialed 表示该 slot 真正进入 openUpstream 调用边界；必须在调用前
			// 置位。它与 started（仅表示 goroutine 已启动）严格区分。
			dialed := true
			up, err := g.openUpstream(actx, plan, prev, t.u, sourceIDFor(t), logicalHostFor(t, plan))
			if up != nil {
				// Body 关闭后再还名额：名额要覆盖整个 attempt 的生命周期。
				// attempt 自己的 ctx 也在这里释放，胜者的 Body 才不会被误取消。
				oc := up.cancel
				up.cancel = func() { oc(); acancel(); doRelease(slot) }
			} else {
				acancel()
				doRelease(slot)
			}
			if actx.Err() != nil {
				if up != nil {
					up.resp.Body.Close()
					up.cancel()
					if up.att != nil {
						up.att.abort()
					}
				}
				ch <- raceResult{slot: slot, start: st, err: errHedgeCanceled, dialed: dialed}
				return
			}
			ch <- raceResult{slot: slot, up: up, start: st, err: err, dialed: dialed}
			if up == nil {
				return
			}
			// 已经交出去就由调用方负责；没交出去（另一路先胜、请求被取消）
			// 就自己把 Body 关掉，绝不留悬空响应体。
			select {
			case <-taken[slot]:
			case <-done:
				up.resp.Body.Close()
				up.cancel()
				if up.att != nil {
					up.att.abort()
				}
			}
		}()
		return true
	}

	defer func() {
		close(done)
		// 只取消没交出去的 attempt：胜者的 Body 仍要读完，它由 saveBody 负责
		// 关闭并触发 up.cancel 释放 ctx 与名额。这里没有共享父 ctx 可取消——
		// 每个 attempt 的 ctx 都直接挂在请求 ctx 上，取消父级会连胜者一起掐断。
		for i := 0; i < n; i++ {
			if cancels[i] == nil {
				continue
			}
			select {
			case <-taken[i]:
			default:
				cancels[i]()
			}
		}
	}()

	if !launch(0) {
		return nil, -1, time.Time{}, errRaceNoCandidate
	}
	// 只有第一路真的启动成功才算「形成了一次竞速」：预算不足整场退回顺序
	// 请求不计入样本（7.1）。
	metrics.RegistryRaceStarted(raceLayerSameHost, raceHost(plan))
	pending := 1
	var hedgeC <-chan time.Time
	if n > 1 {
		tm := time.NewTimer(g.raceAfter())
		defer tm.Stop()
		hedgeC = tm.C
	}
	var lastErr error
	budgetOut := false

	for pending > 0 {
		select {
		case r := <-ch:
			pending--
			// 只在主 goroutine 收到该 slot 的结果后回填拨号状态：channel 接收
			// 与 goroutine 内的发送建立 happens-before，不读并发写入的状态。
			if r.slot >= 0 && r.slot < len(dialedOut) {
				dialedOut[r.slot] = r.dialed
			}
			if r.err != nil {
				if errors.Is(r.err, errRaceNoBudget) {
					budgetOut = true
					if r.slot == 0 {
						// 第一路就没预算：整场不竞速，交给顺序路径。
						return nil, -1, time.Time{}, errRaceNoBudget
					}
					continue
				}
				if !errors.Is(r.err, errHedgeCanceled) && !errors.Is(r.err, context.Canceled) {
					lastErr = r.err
				}
				// 快速失败：不必等计时器，立刻补第二路。
				if launched := countStarted(started); launched < n && !budgetOut {
					if launch(launched) {
						pending++
					}
				}
				continue
			}
			code := r.up.resp.StatusCode
			if code == http.StatusOK || code == http.StatusNotModified {
				markTaken(r.slot)
				// 只统计真的被取消的已启动 attempt。
				for i := 0; i < n; i++ {
					if !started[i] {
						continue
					}
					select {
					case <-taken[i]:
					default:
						metrics.RegistryRaceCancel()
					}
				}
				return r.up, r.slot, r.start, nil
			}
			// 这一路答得正常只是没文件/没资格：记中性结果，不罚源。
			if t := pair[r.slot]; t.c != nil && g.sched != nil {
				o := scheduler.OutcomeFail
				switch {
				case isMiss(code):
					o = scheduler.OutcomeMiss
				case isAuthDenied(code):
					o = scheduler.OutcomeAuth
				}
				g.sched.ReportOutcome(plan.Sched(), *t.c, o, time.Since(r.start), scheduler.Hint{Code: code})
				if o == scheduler.OutcomeFail {
					g.sched.Observe(t.c.ID, 0, 0, false, code)
				}
			}
			lastErr = &upstreamError{
				Code:       code,
				RetryAfter: retryAfterHeader(r.up.resp.Header),
				Challenge:  r.up.resp.Header.Get("WWW-Authenticate"),
			}
			markTaken(r.slot)
			r.up.resp.Body.Close()
			r.up.cancel()
			if launched := countStarted(started); launched < n && !budgetOut {
				if launch(launched) {
					pending++
				}
			}
		case <-hedgeC:
			hedgeC = nil
			if launched := countStarted(started); launched < n {
				if launch(launched) {
					pending++
				}
			}
		case <-ctx.Done():
			return nil, -1, time.Time{}, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = &upstreamError{Code: 0}
	}
	return nil, -1, time.Time{}, lastErr
}

func countStarted(started []bool) int {
	n := 0
	for _, s := range started {
		if s {
			n++
		}
	}
	return n
}

// raceTargets 返回本次请求要竞速的两个候选在 targets 里的下标：调用方据此
// 把两者都计入 tried 集合，正文失败后顺序回退时跳过它们。
func raceTargets(targets []fetchTarget, pair []fetchTarget) (int, int, bool) {
	if len(pair) < 2 || pair[0].c == nil || pair[1].c == nil {
		return 0, 0, false
	}
	ia, ib := -1, -1
	for i := range targets {
		if targets[i].c == nil {
			continue
		}
		if ia < 0 && targets[i].c.ID == pair[0].c.ID {
			ia = i
			continue
		}
		if ib < 0 && ib != ia && targets[i].c.ID == pair[1].c.ID {
			ib = i
		}
	}
	if ia < 0 || ib < 0 {
		return 0, 0, false
	}
	return ia, ib, true
}
