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
//   - sequential：按 priority 固定顺序逐站尝试，首个可信正文交付；
//   - race：首轮 C=2 并发、200ms 投票窗口；两路一致直接交付；不一致先顺序
//     补位，仍不能解决才用唯一一次 Authority；Authority 正文可作最终交付。
//
// 本文件不改缓存身份、Claims 身份与认证边界：所有候选共享权威 Identity，
// 缓存键仍是权威 plan 的 CacheKey，Auth 策略按候选自己的逻辑 Registry 解析。

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

// replicaTarget 是一个跨 Registry 候选。
type replicaTarget struct {
	u        *url.URL
	registry string // 该候选的逻辑 Registry host
	sourceID int64
	name     string
}

func (g *Gateway) replicaMode() string {
	switch strings.ToLower(strings.TrimSpace(g.replica)) {
	case "race":
		return "race"
	case "sequential":
		return "sequential"
	}
	return ""
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
func (g *Gateway) replicaTargets(plan *adapter.Plan, now time.Time) ([]replicaTarget, uint64, error) {
	authority := rangeHostOf(plan)
	if authority == "" || plan.Repo == "" || plan.Ref == "" {
		return nil, 0, nil
	}
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
	for _, rep := range reps {
		if len(out) >= unified.RMax {
			break
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
		var picked []replicaTarget
		for _, c := range cands {
			if !allowed[c.ID] {
				continue
			}
			cc := c
			u, ok := g.sched.Rewrite("registry:"+rep.ReplicaRegistry, base, cc)
			if !ok || u == nil {
				continue
			}
			picked = append(picked, replicaTarget{u: u, registry: rep.ReplicaRegistry, sourceID: cc.ID, name: cc.Name})
		}
		// 同一副本 Registry 可能有多个可信 Source；按候选顺序取第一个可用即可，
		// 避免同一副本在投票里贡献两票（投票层仍按 source_id 去重）。
		if len(picked) > 0 {
			out = append(out, picked[0])
		}
		if len(out) >= unified.RMax {
			break
		}
	}
	return out, gen, nil
}

// fetchReplicaArtifact 完整下载一个候选正文到临时文件，计算 SHA-256 并做
// 与官方一致的内容完整性检查。返回的临时文件由调用方负责提交或删除。
func (g *Gateway) fetchReplicaArtifact(ctx context.Context, plan *adapter.Plan, t replicaTarget, identityHost string) (replicaArtifact, int, error) {
	var art replicaArtifact
	up, err := g.openUpstreamAs(ctx, plan, nil, t.u, t.sourceID, t.registry, identityHost)
	if err != nil {
		return art, 0, err
	}
	resp := up.resp
	defer func() {
		resp.Body.Close()
		up.cancel()
	}()
	if resp.StatusCode != http.StatusOK {
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
	return art, http.StatusOK, nil
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
	key := "replica|" + plan.CacheKey
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
	targets, gen, err := g.replicaTargets(plan, g.now())
	if err != nil {
		// 配置读取失败不阻断主路径：副本是加速收益，不是可用性前提。
		loggw(slog.LevelWarn, "副本候选解析失败，回到顺序路径", ctx, plan, logging.CacheCommitFailed, err)
		return nil, false, nil
	}
	if len(targets) < 2 {
		return nil, false, nil
	}
	_ = gen

	if g.replicaMode() == "sequential" {
		for _, t := range targets {
			art, code, err := g.fetchReplicaArtifact(ctx, plan, t, idHost)
			if err != nil {
				_ = code
				continue
			}
			e, cerr := g.commitReplicaArtifact(ctx, plan, art)
			if cerr != nil {
				return nil, true, cerr
			}
			plan.Source = t.name
			return e, true, nil
		}
		return nil, false, nil
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
			art, _, err := g.fetchReplicaArtifact(ctx, plan, targets[i], idHost)
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

	cands := make([]unified.CrossCandidate, 0, len(targets))
	for i := range targets {
		i := i
		cands = append(cands, unified.CrossCandidate{
			Registry: targets[i].registry,
			SourceID: targets[i].sourceID,
			Run: func(rc context.Context) (string, error) {
				return run(i)
			},
			Abort: func() {},
		})
	}
	// Authority 唯一一次真实派发：候选 0 就是权威官方，复用它的下载产物。
	cfg := unified.CrossRaceConfig{
		Authority: func(rc context.Context) (string, error) {
			return run(0)
		},
	}
	out, rerr := unified.RunCrossRace(ctx, cfg, cands)
	// 窗口外仍未完成的候选：标记放弃，让它带着自己的产物退出。
	for i := range slots {
		select {
		case <-slots[i].done:
		default:
			mu.Lock()
			slots[i].abandoned = true
			mu.Unlock()
		}
	}
	if out.Winner >= 0 {
		art := slots[out.Winner].art
		consumed[out.Winner] = true
		if targets[out.Winner].sourceID > 0 {
			plan.Source = targets[out.Winner].name
		}
		dropAll(out.Winner)
		e, cerr := g.commitReplicaArtifact(ctx, plan, art)
		if cerr != nil {
			return nil, true, cerr
		}
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
		e, cerr := g.commitReplicaArtifact(ctx, plan, art)
		if cerr != nil {
			return nil, true, cerr
		}
		return e, true, nil
	}
	dropAll(-1)
	_ = rerr
	return nil, false, nil
}
