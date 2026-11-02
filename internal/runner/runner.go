// Package runner schedules probes, applies digest consensus and maintains
// source status.
//
// 一个源可能同时支持 Docker Hub 和 GitHub：cap 说明支持什么，
// status 说明现在活不活，两者分开记。
package runner

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

var allKinds = []string{"docker", "github"}

// Options 是源站生命周期的三个时间参数，零值走默认。
type Options struct {
	// ArchiveAfter：dead/blocked 维持这么久就归档（不再探测、不参与调度）。
	ArchiveAfter time.Duration
	// CleanupAfter：归档维持这么久就彻底删除（0 = 永不删）。
	CleanupAfter time.Duration
	// RecoveryEvery：dead/blocked 源最多多久探测一次（恢复探测）。
	RecoveryEvery time.Duration
}

func (o Options) withDefaults() Options {
	if o.ArchiveAfter <= 0 {
		o.ArchiveAfter = 14 * 24 * time.Hour
	}
	if o.RecoveryEvery <= 0 {
		o.RecoveryEvery = time.Hour
	}
	return o
}

type Engine struct {
	st  *store.Store
	opt Options

	mu       sync.Mutex
	inflight bool
	queued   int // 探测中又来的请求，记下最高的 tier
	lastRun  map[int]time.Time

	officialDigest string
	officialAt     time.Time

	onChange func()
}

// OnChange 注册状态变更回调，调度器据此丢掉候选缓存，
// 否则刚判死的源还能在 ListTTL 内被选中。
func (e *Engine) OnChange(fn func()) { e.onChange = fn }

// Changed 通知外部状态已变（面板改状态、启用/停用时调用）。
func (e *Engine) Changed() {
	if e.onChange != nil {
		e.onChange()
	}
}

func New(st *store.Store, opts ...Options) *Engine {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return &Engine{st: st, opt: o.withDefaults(), lastRun: map[int]time.Time{}}
}

// Busy 表示当前是否有探测在跑，面板用来显示"检测中"。
func (e *Engine) Busy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inflight
}

func (e *Engine) Run(tier int) {
	e.mu.Lock()
	if e.inflight {
		// 已在跑：记下最高 tier 等这轮结束补跑，丢了新源会一直 pending。
		if tier > e.queued {
			e.queued = tier
		}
		e.mu.Unlock()
		return
	}
	e.inflight = true
	e.lastRun[tier] = time.Now()
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.inflight = false
		q := e.queued
		e.queued = 0
		e.mu.Unlock()
		if q > 0 {
			go e.Run(q)
		}
	}()

	srcs, err := e.st.EnabledSources()
	if err != nil {
		log.Printf("runner: list sources: %v", err)
		return
	}
	// 死源/封禁源降频到每小时一次：不探就永远不恢复，
	// 但按 3 分钟一轮去敲挂掉的站只是白烧带宽。
	if tier < 3 {
		now := time.Now().Unix()
		gap := int64(e.opt.RecoveryEvery.Seconds())
		keep := srcs[:0]
		for _, s := range srcs {
			if !e.probeDue(s, now, gap) {
				continue
			}
			keep = append(keep, s)
		}
		srcs = keep
	}
	// tier3 要下载真实文件，只对活着的源做，省带宽和对方额度。
	if tier >= 3 {
		live := srcs[:0]
		for _, s := range srcs {
			if s.Status == "usable" || s.Status == "degraded" || s.DockerCap < 0 || s.GithubCap < 0 {
				live = append(live, s)
			}
		}
		srcs = live
	}

	type job struct {
		src  store.Source
		kind string
	}
	var jobs []job
	for _, s := range srcs {
		for _, k := range kindsFor(s, tier) {
			// 观察池：能力未知的新源，L1 连续通过才放行 L2。
			if !e.observed(s, k, tier) {
				continue
			}
			jobs = append(jobs, job{s, k})
		}
	}
	if len(jobs) == 0 {
		return
	}

	expected := ""
	if tier >= 2 {
		expected = e.baseline(context.Background())
	}

	type pair struct {
		job job
		res probe.Result
	}
	results := make([]pair, len(jobs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			j := jobs[i]
			var r probe.Result
			if j.kind == "docker" {
				r = probe.Docker(ctx, j.src.BaseURL, tier, expected)
			} else {
				r = probe.GitHub(ctx, j.src.BaseURL, tier, "")
			}
			results[i] = pair{j, r}
		}(i)
	}
	wg.Wait()

	// 没有官方基准时，用多数派的 digest 当基准。
	if tier >= 3 && expected == "" {
		expected = consensus(results, func(p pair) string {
			if p.job.kind != "docker" {
				return ""
			}
			return p.res.Digest
		})
		if expected != "" {
			for i := range results {
				if results[i].job.kind == "docker" && results[i].res.OK && results[i].res.Digest != "" {
					if results[i].res.Digest == expected {
						results[i].res.DigestOK = 1
					} else {
						results[i].res.DigestOK = 0
					}
				}
			}
		}
	}

	// 按源聚合后一次性写回。
	perSource := map[int64]map[string]probe.Result{}
	for _, p := range results {
		c := store.Check{
			SourceID:  p.job.src.ID,
			Kind:      p.job.kind,
			Tier:      tier,
			OK:        p.res.OK,
			LatencyMS: p.res.LatencyMS,
			SpeedKBps: p.res.SpeedKBps,
			Digest:    p.res.Digest,
			DigestOK:  p.res.DigestOK,
			BlobOK:    p.res.BlobOK,
			TTFBMS:    p.res.TTFBMS,
			TLSOK:     p.res.TLSOK,
			CertDays:  p.res.CertDays,
			HTTPCode:  p.res.HTTPCode,
			Error:     p.res.Error,
			TS:        time.Now().Unix(),
		}
		if err := e.st.AddCheck(c); err != nil {
			log.Printf("runner: add check %d: %v", p.job.src.ID, err)
			continue
		}
		if perSource[p.job.src.ID] == nil {
			perSource[p.job.src.ID] = map[string]probe.Result{}
		}
		perSource[p.job.src.ID][p.job.kind] = p.res
	}
	for _, s := range srcs {
		if r, ok := perSource[s.ID]; ok {
			_ = e.st.TouchProbe(s.ID)
			e.apply(s, r, tier)
		}
	}
	log.Printf("runner: tier%d done, %d checks over %d sources", tier, len(results), len(srcs))
}

// probeDue 判断这轮要不要探这个源：死源/封禁源降频到 RecoveryEvery，
// 其余照常，它们值得每 3 分钟看一次。
func (e *Engine) probeDue(s store.Source, now, gap int64) bool {
	if s.Status != "dead" && s.Status != "blocked" {
		return true
	}
	return s.ProbeAt == 0 || now-s.ProbeAt >= gap
}

// RunSource 立刻识别单个源（新增自定义加速时用），两种类型都试一遍。
func (e *Engine) RunSource(id int64) {
	s, err := e.st.GetSource(id)
	if err != nil {
		return
	}
	expected := e.baseline(context.Background())
	res := map[string]probe.Result{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, k := range allKinds {
		wg.Add(1)
		go func(kind string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			var r probe.Result
			if kind == "docker" {
				r = probe.Docker(ctx, s.BaseURL, 2, expected)
			} else {
				r = probe.GitHub(ctx, s.BaseURL, 2, "")
			}
			mu.Lock()
			res[kind] = r
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	for _, k := range allKinds {
		r := res[k]
		_ = e.st.AddCheck(store.Check{
			SourceID: s.ID, Kind: k, Tier: 2, OK: r.OK, LatencyMS: r.LatencyMS,
			SpeedKBps: r.SpeedKBps, Digest: r.Digest, DigestOK: r.DigestOK,
			BlobOK: r.BlobOK, TTFBMS: r.TTFBMS,
			TLSOK: r.TLSOK, CertDays: r.CertDays, HTTPCode: r.HTTPCode,
			Error: r.Error, TS: time.Now().Unix(),
		})
	}
	_ = e.st.TouchProbe(s.ID)
	e.apply(*s, res, 2)
	log.Printf("runner: 识别完成 %s → docker=%v github=%v", s.BaseURL, res["docker"].OK, res["github"].OK)
}

// kindsFor 决定这一轮该测哪些类型：L1 只测已确认支持的，
// L2/L3 再加上没测过的（自动识别双栈站）。
func kindsFor(s store.Source, tier int) []string {
	var out []string
	// 死源且无任何协议确认过时两种都测：误判成 CapNo 的还能自己爬回来。
	reidentify := tier >= 2 && s.Status == "dead" && s.DockerCap != store.CapYes && s.GithubCap != store.CapYes
	for _, k := range allKinds {
		cap := s.DockerCap
		if k == "github" {
			cap = s.GithubCap
		}
		switch {
		case cap == store.CapYes:
			out = append(out, k)
		case cap == store.CapUnknown && tier == 1 && s.Status == "pending":
			// 观察池：刚发现的源先只做轻量存活探测。
			out = append(out, k)
		case cap == store.CapUnknown && tier >= 2:
			out = append(out, k)
		case reidentify:
			out = append(out, k)
		}
	}
	return out
}

// 观察池：新源先只做 L1，连续通过 observePass 次才做完整 L2。
const (
	observePass = 2
	observeFail = 6
)

// observed 判断这个源这一轮能不能进 L2。只有能力未知的 pending 源受限，
// 死源复检、已确认能力的源照旧。
func (e *Engine) observed(s store.Source, kind string, tier int) bool {
	if tier < 2 || s.Status != "pending" {
		return true
	}
	cap := s.DockerCap
	if kind == "github" {
		cap = s.GithubCap
	}
	if cap != store.CapUnknown {
		return true
	}
	pass, _ := e.st.ObserveStreak(s.ID, kind)
	return pass >= observePass
}

// observeFailures 返回两种类型里连续失败次数的较大值。
func (e *Engine) observeFailures(s store.Source) int {
	fail := 0
	for _, k := range allKinds {
		if _, f := e.st.ObserveStreak(s.ID, k); f > fail {
			fail = f
		}
	}
	return fail
}

// apply 写回能力标记和聚合状态，然后让调度器丢掉候选缓存。
func (e *Engine) apply(s store.Source, res map[string]probe.Result, tier int) {
	e.applyInner(s, res, tier)
	// 状态一改，旧候选名单就不该再用（TTL 60s 太长）。
	e.Changed()
}

func (e *Engine) applyInner(s store.Source, res map[string]probe.Result, tier int) {
	// 探测到写回隔几十秒，源可能已被归档或删除：
	// 以库里的当前状态为准，不用探测开始时的快照。
	cur, err := e.st.GetSource(s.ID)
	if err != nil {
		return
	}
	// 归档是人工决定，探测结果不能顶掉：能通了也不代表想让它回调度。
	if cur.Status == "archived" {
		return
	}
	s = *cur
	docker, github := s.DockerCap, s.GithubCap
	for k, r := range res {
		capv := docker
		if k == "github" {
			capv = github
		}
		switch {
		case r.Verdict == probe.VerdictYes && tier >= 2:
			capv = store.CapYes
		case r.Verdict == probe.VerdictNo && tier >= 2 && capv == store.CapUnknown:
			// 只有明确说「没有这个协议」（404/410）才认 CapNo。
			// 超时/DNS/429/5xx 是此刻不通，写成 CapNo 等于永久判死。
			capv = store.CapNo
		}
		if k == "docker" {
			docker = capv
		} else {
			github = capv
		}
	}
	if docker != s.DockerCap || github != s.GithubCap {
		// 条件更新：写之前那一瞬被归档，也不覆盖。
		ok, err := e.st.SetCapsLive(s.ID, docker, github)
		if err != nil {
			log.Printf("runner: set caps %d: %v", s.ID, err)
		} else if !ok {
			return
		}
	}
	s.DockerCap, s.GithubCap = docker, github

	// 协议级健康分开记：docker 挂了不影响 github 的候选资格；
	// 明确「不支持」不算坏，不记。
	for k, r := range res {
		if r.Verdict == probe.VerdictNo {
			continue
		}
		if err := e.st.NoteProbe(s.ID, store.Family(k), r.OK); err != nil {
			log.Printf("runner: note probe %d/%s: %v", s.ID, k, err)
		}
	}

	// 内容被替换：直接封禁，等人看
	for _, r := range res {
		if r.DigestOK == 0 {
			_, _ = e.st.SetStatusLive(s.ID, "blocked", "内容与官方摘要不一致")
			return
		}
	}
	// 封禁源要连续 2 次校验一致才恢复，否则间歇返回正确内容的
	// 污染源会在可用/封禁之间来回跳。
	if s.Status == "blocked" {
		clean := 0
		for _, k := range allKinds {
			if !s.Supports(k) {
				continue
			}
			if n := e.st.ConsecutiveClean(s.ID, k); n > clean {
				clean = n
			}
		}
		if clean < 2 {
			_, _ = e.st.SetStatusLive(s.ID, "blocked", "校验恢复中：需连续 2 次一致")
			return
		}
	}

	// 任一已确认类型此刻可用 → usable
	for _, k := range allKinds {
		if !s.Supports(k) {
			continue
		}
		if r, ok := res[k]; ok {
			if r.OK {
				_, _ = e.st.SetStatusLive(s.ID, "usable", "")
				return
			}
			continue
		}
		if c, err := e.st.LastCheck(s.ID, k); err == nil && c != nil && c.OK {
			_, _ = e.st.SetStatusLive(s.ID, "usable", "")
			return
		}
	}

	if s.DockerCap != store.CapYes && s.GithubCap != store.CapYes {
		// 全没结论（超时/DNS/429）就标 pending 继续识别：判死会降频到
		// 每小时一次，而它可能只是刚才网络抖了一下。
		if s.DockerCap == store.CapUnknown && s.GithubCap == store.CapUnknown {
			if tier == 1 && s.Status == "pending" {
				// 观察池：连续失败才降频，别一次抖动就丢掉新源。
				if e.observeFailures(s) >= observeFail {
					_, _ = e.st.SetStatusLive(s.ID, "dead", "存活探测连续失败")
				}
				return
			}
			_, _ = e.st.SetStatusLive(s.ID, "pending", "探测未拿到结论，等待复测")
			return
		}
		_, _ = e.st.SetStatusLive(s.ID, "dead", "没有任何协议探测通过")
		return
	}
	// 连续 3 次 tier>=2 失败才判死，避免一次抖动就下架。
	if tier < 2 {
		if s.Status == "usable" {
			_, _ = e.st.SetStatusLive(s.ID, "degraded", "存活探测失败")
		}
		return
	}
	worst := 1 << 30
	for _, k := range allKinds {
		if !s.Supports(k) {
			continue
		}
		if n := e.st.ConsecutiveFailures(s.ID, k); n < worst {
			worst = n
		}
	}
	if worst >= 3 {
		_, _ = e.st.SetStatusLive(s.ID, "dead", "连续 3 次探测失败")
	} else {
		_, _ = e.st.SetStatusLive(s.ID, "degraded", "探测失败，未达判死阈值")
	}
}

func consensus[T any](items []T, digest func(T) string) string {
	counts := map[string]int{}
	best, bestN := "", 0
	for _, it := range items {
		d := digest(it)
		if d == "" {
			continue
		}
		counts[d]++
		if counts[d] > bestN {
			best, bestN = d, counts[d]
		}
	}
	if bestN < 2 {
		return ""
	}
	return best
}

// baseline 取官方 registry 的 digest 作为权威基准；6 小时缓存，失败回退空。
func (e *Engine) baseline(ctx context.Context) string {
	e.mu.Lock()
	if time.Since(e.officialAt) < 6*time.Hour && e.officialDigest != "" {
		d := e.officialDigest
		e.mu.Unlock()
		return d
	}
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r := probe.Docker(ctx, "https://registry-1.docker.io", 2, "")
	if r.OK && r.Digest != "" {
		e.mu.Lock()
		e.officialDigest, e.officialAt = r.Digest, time.Now()
		e.mu.Unlock()
		return r.Digest
	}
	return ""
}

// Loop 按设置里的间隔调度；每 30 秒比一次时间，改设置不用重启。
func (e *Engine) Loop(ctx context.Context) {
	go func() {
		// 启动后先跑一轮，面板立刻有数据
		time.Sleep(2 * time.Second)
		e.maintain(time.Now())
		e.Run(2)
		e.Run(3)
	}()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	lastPrune := time.Now()
	lastMaint := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.mu.Lock()
			l1, l2, l3 := e.lastRun[1], e.lastRun[2], e.lastRun[3]
			e.mu.Unlock()
			now := time.Now()
			if now.Sub(l1) >= minutes(e.st.GetSetting("l1_minutes", "3")) {
				go e.Run(1)
			}
			if now.Sub(l2) >= minutes(e.st.GetSetting("l2_minutes", "15")) {
				go e.Run(2)
			}
			if now.Sub(l3) >= minutes(e.st.GetSetting("l3_minutes", "360")) {
				go e.Run(3)
			}
			if now.Sub(lastMaint) >= time.Hour {
				lastMaint = now
				e.maintain(now)
			}
			if now.Sub(lastPrune) > 24*time.Hour {
				lastPrune = now
				_ = e.st.Prune(now.AddDate(0, 0, -30).Unix())
				e.st.PruneSessions()
			}
		}
	}
}

// maintain 做生命周期收尾：长期不可用的归档，归档太久的删除。
// 每小时一次，纯 SQL，代价可忽略。
func (e *Engine) maintain(now time.Time) {
	if n, err := e.st.ArchiveStale(now.Add(-e.opt.ArchiveAfter).Unix()); err != nil {
		log.Printf("runner: 归档失败: %v", err)
	} else if n > 0 {
		log.Printf("runner: 归档 %d 个长期不可用的源", n)
	}
	if e.opt.CleanupAfter > 0 {
		if n, err := e.st.CleanupArchived(now.Add(-e.opt.CleanupAfter).Unix()); err != nil {
			log.Printf("runner: 清理归档源失败: %v", err)
		} else if n > 0 {
			log.Printf("runner: 清理 %d 个归档超期的源", n)
		}
	}
}

func minutes(v string) time.Duration {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		n = 1
	}
	return time.Duration(n) * time.Minute
}
