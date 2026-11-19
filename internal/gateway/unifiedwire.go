package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/registryhost"
	"github.com/zggnh12345/mirror-lens/internal/unified"
)

// 3.5.17 统一执行框架的网关侧接线。
//
// 本文件只做三件事：把 plan 翻译成 Identity、把请求绑定到某类 Chain、
// 在发送提交边界（g.cli.Do 之前）完成 Claim Dispatch 与 R 提交。
// 预算、状态机与仲裁都在 internal/unified 里，这里不重新实现一遍。
//
// 部署期全局回退（T62）：是否启用统一框架由 Gateway.unifiedOn 一次性决定，
// 关闭时 newRequestScope 不建 Scope，所有接线点都因「没有 Scope」自然跳过，
// 不存在同一个请求里一半走新框架、一半走旧路径的混合态。

var (
	// errUnifiedSuppressed 表示同一 Identity 已被另一条链占住，本链不发送。
	// 它是中性事件：不罚源、不进冷却、也不算 Miss。
	errUnifiedSuppressed = errors.New("unified: identity suppressed by another chain")
)

// closeUnifiedChain 结束某类链（释放它的 Claim 与在途计数）。
// 用于「A 竞速整场结束后顺序回退」：只有链真的结束，同 Identity 的
// 主链才能重新预占，否则会把自己误判成重复派发。
func closeUnifiedChain(ctx context.Context, kind unified.ChainKind) {
	sc := unified.FromContext(ctx)
	if sc == nil {
		return
	}
	if ch, err := sc.ChainFor(kind); err == nil {
		ch.Close()
	}
}

// planIdentity 计算 Claim 身份。Registry 用规范化逻辑 Host（不是镜像站
// host），Repository/Ref 由适配器显式给出；任一为空就不参与 Claim。
func planIdentity(plan *adapter.Plan, logicalHost string) unified.Identity {
	if plan == nil {
		return unified.Identity{}
	}
	host, err := registryhost.Normalize(logicalHost)
	if err != nil {
		return unified.Identity{}
	}
	return unified.Identity{Registry: host, Repository: plan.Repo, Manifest: plan.Ref}
}

// unifiedManifest 判断这次回源是否属于「逻辑 Attempt」语义。
// Blob/Range 走 P 与 RangeWorkCap，不经过 Claim/R，不能套同一套账。
func unifiedManifest(plan *adapter.Plan) bool {
	return plan != nil && plan.Registry && plan.ResourceKind == adapter.ResourceManifest
}

// ---------------------------------------------------------------------------
// Chain 绑定
// ---------------------------------------------------------------------------

type unifiedChainKey struct{}

// withUnifiedChain 把当前执行路径绑定到某类链（main/A/B/fb）。
// 没有 Scope 或 Scope 已关闭时原样返回，调用方随后会走无框架路径。
func withUnifiedChain(ctx context.Context, kind unified.ChainKind) context.Context {
	sc := unified.FromContext(ctx)
	if sc == nil {
		return ctx
	}
	ch, err := sc.ChainFor(kind)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, unifiedChainKey{}, ch)
}

func chainFrom(ctx context.Context) *unified.Chain {
	if ctx == nil {
		return nil
	}
	ch, _ := ctx.Value(unifiedChainKey{}).(*unified.Chain)
	return ch
}

// ---------------------------------------------------------------------------
// Attempt 门控
// ---------------------------------------------------------------------------

// unifiedAttempt 是一次 Manifest 上游请求在统一框架里的句柄。
type unifiedAttempt struct {
	sc    *unified.Scope
	chain *unified.Chain
	att   *unified.Attempt
	plan  *adapter.Plan
	id    unified.Identity
	src   int64
}

// beginUnifiedAttempt 在发送前登记 Attempt 并预占 Claim。
//
// 返回 (nil, nil) 表示本次请求不参与统一框架（非 Manifest、无 Scope、
// 身份不完整）；返回 errUnifiedSuppressed 表示同一 Identity 已被另一条链
// 占住，本链不得发送（静态抑制/跨链去重）。
func (g *Gateway) beginUnifiedAttempt(ctx context.Context, plan *adapter.Plan, logicalHost string, sourceID int64) (*unifiedAttempt, error) {
	if !unifiedManifest(plan) {
		return nil, nil
	}
	sc := unified.FromContext(ctx)
	if sc == nil {
		return nil, nil
	}
	ch := chainFrom(ctx)
	if ch == nil {
		return nil, nil
	}
	id := planIdentity(plan, logicalHost)
	if id.Empty() {
		// 身份不完整时不能假装自己占住了什么：跳过 Claim，但仍记 R，
		// 让预算在异常路径上依然是硬上限。
		att, err := ch.RegisterAttempt(nil)
		if err != nil {
			return nil, err
		}
		return &unifiedAttempt{sc: sc, chain: ch, att: att, plan: plan, src: sourceID}, nil
	}
	claim, err := sc.Claim(ch.ID(), id)
	if err != nil {
		if errors.Is(err, unified.ErrClaimConflict) {
			return nil, errUnifiedSuppressed
		}
		return nil, err
	}
	att, err := ch.RegisterAttempt(claim)
	if err != nil {
		return nil, err
	}
	return &unifiedAttempt{sc: sc, chain: ch, att: att, plan: plan, id: id, src: sourceID}, nil
}

// dispatch 越过发送提交边界。失败时必须 Abort 释放 R 预留。
func (a *unifiedAttempt) dispatch() error {
	if a == nil || a.att == nil {
		return nil
	}
	if err := a.att.Dispatch(); err != nil {
		a.att.Abort()
		if errors.Is(err, unified.ErrClaimReleased) {
			return errUnifiedSuppressed
		}
		return err
	}
	return nil
}

// abort 在发送前失败时归还 R 预留。
func (a *unifiedAttempt) abort() {
	if a == nil || a.att == nil {
		return
	}
	a.att.Abort()
}

// finish 按结果分类写结果账本并结束 Attempt。
// Attempt 四态里 Completed 表示「执行路径结束」，不代表上游成功；
// 成败由 Outcome 表达，两者不能混用。
func (a *unifiedAttempt) finish(code int, err error) {
	if a == nil || a.att == nil {
		return
	}
	out := classifyOutcome(code, err)
	if a.sc != nil {
		a.sc.Ledger().Record(unified.AttemptResult{
			ChainID:   a.chain.ID(),
			ChainKind: a.chain.Kind(),
			AttemptID: a.att.ID(),
			SourceID:  a.src,
			Registry:  a.id.Registry,
			Outcome:   out,
			Code:      code,
			Err:       err,
		})
	}
	a.att.Complete()
}

// classifyOutcome 把上游结果映射成账本分类。
// 预算耗尽必须走独立通道，不能被折叠成 Auth/Miss/Fail。
func classifyOutcome(code int, err error) unified.Outcome {
	if err != nil {
		if unified.IsBudgetExhausted(err) {
			return unified.OutcomeBudget
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return unified.OutcomeCancelled
		}
		var ue *upstreamError
		if errors.As(err, &ue) {
			switch {
			case isMiss(ue.Code):
				return unified.OutcomeMiss
			case isAuthDenied(ue.Code):
				return unified.OutcomeAuth
			}
		}
		if errors.Is(err, errUnifiedSuppressed) || errors.Is(err, errHedgeCanceled) || errors.Is(err, errRaceLost) {
			return unified.OutcomeCancelled
		}
		return unified.OutcomeFail
	}
	if code == http.StatusOK || code == http.StatusNotModified {
		return unified.OutcomeOK
	}
	return unified.OutcomeOK
}

// ---------------------------------------------------------------------------
// fb 静态抑制快照
// ---------------------------------------------------------------------------

// suppressFallbacks 在请求开始时固定「已由主链覆盖的 Identity」集合，
// 把同 Identity 的 fb 目标整条剔除。
//
// 这是静态快照：同一请求内不再根据运行结果增删（design §5 / F18/F19-14）；
// 被抑制链因此不会参与 Claim 竞争，也不消耗 R。
func suppressFallbacks(plan *adapter.Plan, targets []fetchTarget) ([]fetchTarget, int) {
	if plan == nil || len(targets) == 0 {
		return targets, 0
	}
	seen := map[unified.Identity]bool{}
	for _, t := range targets {
		if t.fb {
			continue
		}
		if id := planIdentity(plan, logicalHostFor(t, plan)); !id.Empty() {
			seen[id] = true
		}
	}
	if len(seen) == 0 {
		return targets, 0
	}
	out := make([]fetchTarget, 0, len(targets))
	suppressed := 0
	for _, t := range targets {
		if t.fb {
			if id := planIdentity(plan, logicalHostFor(t, plan)); !id.Empty() && seen[id] {
				suppressed++
				continue
			}
		}
		out = append(out, t)
	}
	return out, suppressed
}

// ---------------------------------------------------------------------------
// RegistryRetryToken（B16）
// ---------------------------------------------------------------------------

// takeRetryToken 为顺序路径上的「本站一次额外 Attempt」取令牌。
// 没有 Scope（旧路径/回退态）时返回 false：不新增重试行为。
func takeRetryToken(ctx context.Context, logicalHost string) bool {
	sc := unified.FromContext(ctx)
	if sc == nil {
		return false
	}
	host, err := registryhost.Normalize(logicalHost)
	if err != nil || host == "" {
		return false
	}
	return sc.Tokens().Take(host) == nil
}

// isNetworkFault 判断是否属于「可重试的网络类失败」：没有 HTTP 响应。
// 明确响应（404/401/403/429/5xx/无效正文）不重试，直接换下一站。
func isNetworkFault(err error) bool {
	if err == nil || !isSourceFault(err) {
		return false
	}
	var ue *upstreamError
	if errors.As(err, &ue) {
		return false
	}
	var tl *tooLargeError
	if errors.As(err, &tl) {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Hedge：三指标分离 + P 门控
// ---------------------------------------------------------------------------

// hedgeAllowed 在启动额外分支前检查 P 账本：预算不足就不触发对冲，
// 避免「先触发再等 503」。没有 Scope 时保持旧行为。
func hedgeAllowed(ctx context.Context) bool {
	sc := unified.FromContext(ctx)
	if sc == nil {
		return true
	}
	return sc.P().Remaining() > 0
}

// noteHedgeTrigger 记录一次对冲决策；额外请求由调用点在真正发送前单独记。
func noteHedgeTrigger(extra bool) {
	metrics.HedgeTrigger()
	if extra {
		metrics.HedgeExtraRequest()
	}
}

// ---------------------------------------------------------------------------
// 共享 Preflight 与 Range 限额
// ---------------------------------------------------------------------------

// parPreflightShared 让 tryParallel 与 midFill 共享同一 RequestScope 的一轮
// Preflight（D-02）。第二次调用只复用已有结果，不再发探测请求。
func (g *Gateway) parPreflightShared(ctx context.Context, plan *adapter.Plan, targets []fetchTarget, timeout time.Duration) ([]parSource, int64) {
	sc := unified.FromContext(ctx)
	if sc == nil {
		return g.parPreflight(ctx, plan, targets, timeout)
	}
	res, total, err := sc.Preflight(ctx, func(pctx context.Context) ([]unified.PreflightResult, int64, error) {
		srcs, tot := g.parPreflight(pctx, plan, targets, timeout)
		out := make([]unified.PreflightResult, 0, len(srcs))
		for _, s := range srcs {
			out = append(out, unified.PreflightResult{
				Target:      s.t.u.String(),
				SourceID:    sourceIDFor(s.t),
				OK:          true,
				Total:       tot,
				ContentType: s.ctype,
			})
		}
		return out, tot, nil
	})
	if err != nil {
		return nil, -1
	}
	// 只保留本次调用真正指向的目标：共享快照不等于把所有源都塞给调用方。
	want := map[string]bool{}
	for _, t := range targets {
		want[t.u.String()] = true
	}
	byURL := map[string]fetchTarget{}
	for _, t := range targets {
		byURL[t.u.String()] = t
	}
	out := make([]parSource, 0, len(res))
	for _, r := range res {
		if !want[r.Target] {
			continue
		}
		t, ok := byURL[r.Target]
		if !ok {
			continue
		}
		out = append(out, parSource{t: t, name: targetName(t), ctype: r.ContentType})
	}
	return out, total
}

// targetName 与 parSource.name 的既有语义保持一致（候选名或 URL host）。
func targetName(t fetchTarget) string {
	if t.c != nil {
		return t.c.Name
	}
	return t.u.Host
}

// rangeAdmit 在承诺分片之前做 RangeWork 准入：
// 每 Range 最多 4 个目标（含官方源），总工作量不超过 RangeWorkCap。
// 没有 Scope 时保持旧行为，只做目标数截断。
func rangeAdmit(ctx context.Context, targets int, segments int) (int, bool) {
	if targets > unified.MaxRangeTargets {
		targets = unified.MaxRangeTargets
	}
	sc := unified.FromContext(ctx)
	if sc == nil {
		return targets, true
	}
	if segments <= 0 || targets <= 0 {
		return targets, true
	}
	if err := sc.Ranges().Reserve(segments * targets); err != nil {
		return targets, false
	}
	sc.Ranges().AddSegments(segments)
	return targets, true
}

// ---------------------------------------------------------------------------
// 指标/日志辅助
// ---------------------------------------------------------------------------

// rangeHostOf 取 plan 的规范化逻辑 Host，指标层需要低基数标签。
func rangeHostOf(plan *adapter.Plan) string {
	if plan == nil {
		return ""
	}
	h, err := registryhost.Normalize(plan.LogicalHost)
	if err != nil {
		return ""
	}
	return h
}

// suppressedName 给日志用：fb 目标没有候选名时退回 host。
func suppressedName(t fetchTarget) string {
	if t.c != nil {
		return t.c.Name
	}
	if t.fbHost != "" {
		return t.fbHost
	}
	if t.u != nil {
		return t.u.Host
	}
	return ""
}
