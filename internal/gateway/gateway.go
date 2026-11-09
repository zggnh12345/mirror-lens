// Package gateway 是统一加速入口：认证 → 适配器解析 → 缓存 → 回源。
// 阶段 0/1 只挂载 /raw/，接口按后续 /http/ /git/ /v2/ 的形状写。
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/adapter/ghadapter"
	"github.com/zggnh12345/mirror-lens/internal/adapter/gitadapter"
	"github.com/zggnh12345/mirror-lens/internal/adapter/httpadapter"
	"github.com/zggnh12345/mirror-lens/internal/adapter/raw"
	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/gc"
	"github.com/zggnh12345/mirror-lens/internal/ghapi"
	"github.com/zggnh12345/mirror-lens/internal/keymutex"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

const Version = "3.2.4"

type Gateway struct {
	st      *store.Store
	cache   *cache.Store
	cli     *http.Client
	guard   *safehttp.Guard
	raw     *raw.Adapter
	ghAd    *ghadapter.Adapter
	httpAd  *httpadapter.Adapter
	gitAd   *gitadapter.Adapter
	regAd   *registryadapter.Adapter
	ghMeta  *ghapi.Client
	maxSize int64

	cacheMaxBytes int64
	cacheMinFree  int64 // 磁盘剩余低于此值暂停落盘，0 = 不设底线
	diskLowFlag   atomic.Bool
	quotaMu       sync.Mutex
	gc            *gc.GC
	now           func() time.Time
	sched         *scheduler.Scheduler
	flights       sync.Map      // cacheKey -> *flight（缓冲型适配器用）
	locks         *keymutex.Map // cacheKey -> 锁（流式填充用，空闲即回收）
	fillMu        sync.Mutex
	fillFlights   map[string]chan struct{} // cacheKey -> 正在回源的流式填充
	verifyFail    atomic.Int64             // 校验不过、因此没落盘的次数（进程内计数）
	par           ParOptions
	preSem        chan struct{}
	gitSem        chan struct{}
	relayLimit    int
	semOnce       sync.Once
	hedge         HedgeOptions
	parTargets    func(*adapter.Plan) []fetchTarget          // 测试注入用，nil 时走 targets()
	speedProbe    func(id int64, kind string, n int) []int64 // 测试注入用，nil 时查 store
	requireToken  bool                                       // 代理路径是否强制 Access Token（false = 匿名放行）
	cdn           CDNOptions                                 // 边缘缓存策略（阶段 8）
	dockerOff     bool                                       // /v2/ 的 Docker Hub 通道关闭
	ghcrOff       bool                                       // /v2/ 的 GHCR 通道关闭
	githubOff     bool                                       // /gh/ /raw/ /git/ 关闭
	priv          PrivateOptions                             // 私有请求的中转与回退策略
	trusted       atomic.Value                               // []TrustedRelay，面板热更新
	privMu        sync.Mutex
	privFails     map[string]int
	privUntil     map[string]time.Time
	pinClients    sync.Map // "id|v4|v6" -> *http.Client（只拨钉住地址）
	pinMu         sync.Mutex
	pinWarnAt     map[int64]time.Time
	privClient    *http.Client
	privSem       chan struct{} // 回退官方时限制同时下载的文件数
	parFills      atomic.Int64  // 真正走了并行分片的次数
	parBytes      atomic.Int64  // 并行分片下下来的字节
	parFalls      atomic.Int64  // 试过但退回单流（不支持 Range、源不够等）
	tasks         *taskReg
	degradeMu     sync.Mutex
	degradeAt     map[int64]time.Time
	probeAt       map[int64]time.Time // 面板活跃任务表（内存）
}

type ParOptions struct {
	Enabled   bool
	MinSize   int64
	ChunkSize int64
	Workers   int
	// TailVerify 为 true 时，最后一段字节要等整份哈希通过才放给客户端。
	TailVerify bool
	// RangeTTL 是「源不支持 Range」结论的缓存时长，0 表示每次预检都探。
	RangeTTL time.Duration
	// DegradeMBps / DegradeHold 控制星标源降级：最近 3 次真实单流都低于
	// DegradeMBps（MB/s）才改走并行，并行 DegradeHold 后放一次单流重探。
	DegradeMBps int64
	DegradeHold time.Duration

	// 单流实时判速与换源。窗口内平均低于 WatchMBps 判慢；
	// 备选源要有 SwitchMinMBps 的历史样本才值得换；已发超过
	// SwitchMaxSentMiB 不再换；SwitchMax/SwitchGap 限制次数与间隔。
	WatchMBps        int64
	WatchWindow      time.Duration
	SwitchMinMBps    int64
	SwitchMinFileMiB int64
	SwitchMaxSentMiB int64
	SwitchMax        int
	SwitchGap        time.Duration

	// B 档与预检的独立配额（3.0）。0 表示用默认值 4。
	PreflightConcurrency int
	RelayConcurrency     int
	GitConcurrency       int
}

// CDNOptions 控制边缘缓存语义：Enabled=false 只发保守短 TTL，
// 打开后才把条目 TTL 让给边缘并加可清理的缓存标签。
type CDNOptions struct {
	Enabled bool
	MaxAge  int // 秒；0 = 沿用条目 TTL
}

// HedgeOptions 是响应头对冲的运行时参数。
type HedgeOptions struct {
	Enabled bool
	After   time.Duration // 首选源的响应头耐心预算
	Grace   time.Duration // 条件请求里等 304 的宽限
}

type Options struct {
	Cache         *cache.Store
	RawBase       string
	MaxBytes      int64
	GitTTL        time.Duration
	RawTTL        time.Duration
	Registry      *registryadapter.Adapter
	CacheMaxBytes int64 // 缓存总量上限（字节），0 = 不限
	// CacheMinFreeBytes 是磁盘剩余底线：低于它就不落盘、改直通。
	// 配额管"缓存能用多少"，这条管"别把同一块盘上的别人挤死"。
	CacheMinFreeBytes int64
	Guard             *safehttp.Guard
	Parallel          ParOptions
	Hedge             HedgeOptions
	// 默认 false 匿名可读（适合公共加速站）；true 时客户端必须带 token。
	RequireToken bool
	CDN          CDNOptions
	Private      PrivateOptions
	// 三类加速的独立开关（默认全开，零值即开启）。
	// Off 而不是 On：老调用方不传这几个字段时行为不变。
	DockerOff bool
	GHCROff   bool
	GitHubOff bool
	// GHMetadata 为 nil 或未配置 Token 时，Release 下载完全不碰 GitHub API。
	GHMetadata *ghapi.Client
}

func New(st *store.Store, o Options) *Gateway {
	if o.MaxBytes <= 0 {
		o.MaxBytes = 256 << 20
	}
	if o.Hedge.After <= 0 {
		o.Hedge.After = time.Second
	}
	if o.Hedge.Grace <= 0 {
		o.Hedge.Grace = 300 * time.Millisecond
	}
	guard := o.Guard
	if guard == nil {
		guard = safehttp.Default()
	}
	cli := guard.Client()
	if o.Registry != nil {
		// 401 + Bearer 挑战时自动换 token 重发，只对配置的那个上游生效
		cli = &http.Client{
			Transport: &bearerTransport{
				base:   cli.Transport,
				host:   o.Registry.Upstream,
				tokens: newTokenCache(),
			},
			CheckRedirect: cli.CheckRedirect,
		}
	}
	gw := &Gateway{
		ghMeta:        o.GHMetadata,
		dockerOff:     o.DockerOff,
		ghcrOff:       o.GHCROff,
		githubOff:     o.GitHubOff,
		locks:         keymutex.New(),
		st:            st,
		cache:         o.Cache,
		cli:           cli,
		guard:         guard,
		raw:           raw.New(o.RawBase),
		ghAd:          ghadapter.New(),
		httpAd:        httpadapter.New(o.MaxBytes),
		gitAd:         gitadapter.New(o.GitTTL),
		regAd:         o.Registry,
		maxSize:       o.MaxBytes,
		cacheMaxBytes: o.CacheMaxBytes,
		cacheMinFree:  o.CacheMinFreeBytes,
		gc:            gc.New(st, o.Cache),
		now:           time.Now,
		par:           o.Parallel,
		hedge:         o.Hedge,
		tasks:         newTaskReg(),
		degradeAt:     map[int64]time.Time{},
		probeAt:       map[int64]time.Time{},
		requireToken:  o.RequireToken,
		cdn:           o.CDN,
		priv:          o.Private,
		privClient:    newPrivClient(),
	}
	if n := o.Private.FallbackSem; n > 0 {
		gw.privSem = make(chan struct{}, n)
	}
	gw.logPrivateBudget()
	return gw
}

// logPrivateBudget 启动时把私有分片的最坏内存窗口算一遍。
// 窗口 = 并发文件数 × worker 数 × 段大小；段大小会随文件变大翻倍
// （段数收敛到 256），所以按配置的最大文件算出上界。这里只告警，
// 不拒绝启动——数值是估算，不含读缓冲和 Transport 开销。
func (g *Gateway) logPrivateBudget() {
	if !g.par.Enabled || g.par.Workers <= 0 || g.par.ChunkSize <= 0 {
		return
	}
	nPar := cap(g.privSem)
	if nPar == 0 {
		nPar = 1
	}
	chunk := g.par.ChunkSize
	if g.maxSize > 0 {
		if c := g.maxSize / 256; c > chunk {
			chunk = c
		}
		if c := g.maxSize / 2; c < chunk {
			chunk = c
		}
	}
	if chunk <= 0 {
		return
	}
	worst := int64(nPar) * int64(g.par.Workers) * chunk
	slog.Info("私有分片内存窗口",
		"并发文件", nPar, "worker", g.par.Workers,
		"段大小MB", chunk>>20, "最坏MB", worst>>20)
	if worst > 1<<30 {
		slog.Warn("私有分片最坏窗口超 1 GB，建议降低 parallel.workers 或 private.fallback_sem",
			"最坏MB", worst>>20)
	}
}

// GC 暴露给 API 层做手动清理，保证和淘汰走同一套删除逻辑。
func (g *Gateway) GC() *gc.GC { return g.gc }

// CacheStore 给统计接口算磁盘实际占用。
func (g *Gateway) CacheStore() *cache.Store { return g.cache }

// VerifyFailed 返回进程内「校验不过、因此没落盘」的次数。
// 坏内容不写索引、没记录可查，只能用计数器留痕。
func (g *Gateway) VerifyFailed() int64 {
	if g == nil {
		return 0
	}
	return g.verifyFail.Load()
}

// CacheLimit 是配置里的容量上限，0 表示不限。
func (g *Gateway) CacheLimit() int64 { return g.cacheMaxBytes }

// CacheMinFree 返回磁盘剩余底线（0 = 未设）。
func (g *Gateway) CacheMinFree() int64 { return g.cacheMinFree }

// SetRawTTL 给测试用：把可变资源的重新验证间隔调小。
func (g *Gateway) SetRawTTL(d time.Duration) { g.raw.TTL = d }

// SetGitTTL 调整 info/refs 的缓存时长；0 表示不缓存。
func (g *Gateway) SetGitTTL(d time.Duration) { g.gitAd.TTL = d }

type flight struct {
	done   chan struct{}
	entry  *store.CacheEntry
	notMod bool
	err    error
}

// setSource 标出供的哪台源，便于排查「为什么变慢」。
func setSource(w http.ResponseWriter, plan *adapter.Plan) {
	if plan != nil && plan.Source != "" {
		w.Header().Set("X-ML-Source", plan.Source)
	}
}

// SetScheduler 注入多源调度器；传 nil 表示只用官方源。
func (g *Gateway) SetScheduler(s *scheduler.Scheduler) { g.sched = s }

// Scheduler 供面板读取当前排序。
func (g *Gateway) Scheduler() *scheduler.Scheduler { return g.sched }

// ServeRaw 处理 /raw/<owner>/<repo>/<ref>/<path...>
func (g *Gateway) ServeRaw(w http.ResponseWriter, r *http.Request) {
	if g.githubOff {
		http.Error(w, "404 GitHub 加速未开启", http.StatusNotFound)
		return
	}
	g.withAuth(func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, g.raw)
	})(w, r)
}

// ServeGH 处理 release 资产与源码包。自带 Bearer/Basic 认证：
// 客户端是 curl/wget/gh，不会带面板 cookie。
func (g *Gateway) ServeGH(w http.ResponseWriter, r *http.Request) {
	if g.githubOff {
		http.Error(w, "404 GitHub 加速未开启", http.StatusNotFound)
		return
	}
	g.withAuth(func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, g.ghAd)
	})(w, r)
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, ad adapter.Adapter) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 只支持 GET/HEAD", http.StatusMethodNotAllowed)
		return
	}
	plan, err := ad.Plan(r)
	if err != nil {
		noStore(w.Header())
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	g.enrichReleaseDigest(r.Context(), plan)
	// 带凭据的私有内容一律直通：既不读索引，也不落盘。
	// 用 NoStore 而不是 Cacheable：git info/refs 之类只是不值得按 TTL
	// 缓存，仍需走原来的临时落盘路径。
	if plan.NoStore {
		g.fillStream(w, r, plan)
		return
	}
	prev, err := g.st.CacheGet(plan.CacheKey)
	if err != nil {
		loggw(slog.LevelWarn, "缓存索引读取失败", r.Context(), plan, logging.CacheReadFailed, err)
	}

	if prev != nil {
		fresh := plan.Immutable || g.now().Sub(time.Unix(prev.CreatedAt, 0)) < plan.TTL
		if fresh {
			g.serveCached(w, r, plan, prev, "HIT", true)
			return
		}
		// 磁盘低于底线就别再往里写了，命中的旧条目照常发，新的走直通。
		if g.lowDisk() || r.Header.Get("Range") != "" || r.Method == http.MethodHead {
			g.passThrough(w, r, plan)
			return
		}
		// 大文件（release 包、镜像层）：边下边发，首字节立刻走，
		// 下完照样入库。以前只有超出上限或写盘失败才走这条，
		// 正常 MISS 要等整份落盘，32MB 的包首字节就要 20 秒。
		if plan.Stream {
			g.fillStream(w, r, plan)
			return
		}
		entry, notMod, err := g.fetchOnce(r.Context(), plan, prev)
		switch {
		case err == nil && notMod:
			g.st.CacheRefresh(plan.CacheKey)
			g.serveCached(w, r, plan, prev, "HIT-REVAL", true)
			return
		case err == nil:
			setSource(w, plan)
			g.serveCached(w, r, plan, entry, "MISS", false)
			return
		default:
			var ue *upstreamError
			if errors.As(err, &ue) && (ue.Code == http.StatusNotFound || ue.Code == http.StatusGone) {
				// 上游说文件没了：不要拿旧缓存糊弄人
				g.st.CacheDelete(plan.CacheKey)
				noStore(w.Header())
				http.Error(w, fmt.Sprintf("%d 上游返回 %d", http.StatusNotFound, ue.Code), http.StatusNotFound)
				return
			}
			// 超过缓存上限就边下边发、不缓存：大文件最需要加速，
			// 早期直接 502 等于把 release 包全推回源站。
			var tl *tooLargeError
			if errors.As(err, &tl) {
				g.fillStream(w, r, plan)
				return
			}
			// 本地写不进去（盘满、权限）：别 502，直接边下边发。
			var le *localError
			if errors.As(err, &le) {
				loggw(slog.LevelWarn, "本地写入失败，转透传", r.Context(), plan, logging.CacheCommitFailed, err)
				g.fillStream(w, r, plan)
				return
			}
			// 其余错误（超时、5xx、连不上）先给缓存里的旧版本，标 STALE
			loggw(slog.LevelWarn, "回源失败，使用旧缓存", r.Context(), plan, codeFor(err), err)
			g.serveCached(w, r, plan, prev, "STALE", true)
			return
		}
	}

	if g.lowDisk() || r.Header.Get("Range") != "" || r.Method == http.MethodHead {
		g.passThrough(w, r, plan)
		return
	}

	// 首次未命中也要边下边发：release 资产十几 MB，
	// 等整份落盘再回包，首字节就等于下载全长。
	if plan.Stream {
		g.fillStream(w, r, plan)
		return
	}
	entry, _, err := g.fetchOnce(r.Context(), plan, nil)
	if err != nil {
		var tl *tooLargeError
		if errors.As(err, &tl) {
			g.fillStream(w, r, plan)
			return
		}
		var le *localError
		if errors.As(err, &le) {
			loggw(slog.LevelWarn, "本地写入失败，转透传", r.Context(), plan, logging.CacheCommitFailed, err)
			g.fillStream(w, r, plan)
			return
		}
		code := statusFor(err)
		loggw(slog.LevelWarn, "回源失败", r.Context(), plan, codeFor(err), err)
		noStore(w.Header())
		setRetryAfter(w, err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}
	setSource(w, plan)
	g.serveCached(w, r, plan, entry, "MISS", false)
}

// FSFree 返回缓存盘的剩余字节，供 /readyz 判断是否还写得下。
func (g *Gateway) FSFree() (int64, error) {
	if g.cache == nil {
		return 0, nil
	}
	return g.cache.FSFree()
}

// CacheRoot 返回缓存目录，供 /readyz 做目录级检查。
func (g *Gateway) CacheRoot() string {
	if g.cache == nil {
		return ""
	}
	return g.cache.Root()
}

// freshFor 按 plan 的 TTL 判断条目是否还新鲜（/gh/ /raw/ 用）。
func (g *Gateway) freshFor(e *store.CacheEntry, plan *adapter.Plan) bool {
	if e == nil {
		return false
	}
	return plan.Immutable || g.now().Sub(time.Unix(e.CreatedAt, 0)) < plan.TTL
}

// fetchOnce 保证同一缓存键同时只有一次回源，其余请求等结果。
func (g *Gateway) fetchOnce(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry) (*store.CacheEntry, bool, error) {
	v, loaded := g.flights.LoadOrStore(plan.CacheKey, &flight{done: make(chan struct{})})
	f := v.(*flight)
	if loaded {
		select {
		case <-f.done:
			return f.entry, f.notMod, f.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	f.entry, f.notMod, f.err = g.fetch(ctx, plan, prev)
	close(f.done)
	g.flights.Delete(plan.CacheKey)
	return f.entry, f.notMod, f.err
}

type fetchTarget struct {
	u      *url.URL
	c      *scheduler.Candidate
	fb     bool   // 属于备用站点（plan.Fallbacks）
	fbHost string // 备用站点的逻辑站点（ghcr.io），不是加速源 host
}

// targets 决定回源顺序。prev != nil（TTL 到期重验证）只回官方源：
// 镜像可能把旧内容当「没变」，让它验证等于固化过期数据。
func (g *Gateway) targets(plan *adapter.Plan, prev *store.CacheEntry) []fetchTarget {
	out := []fetchTarget{{u: plan.Upstream}}
	fbs := g.fallbackTargets(plan, prev)
	if prev != nil || plan.OfficialOnly || !g.sched.Enabled() {
		return append(out, fbs...)
	}
	cands := g.sched.Candidates(plan.Kind)
	metrics.SourceCandidates(len(cands))
	var head []fetchTarget
	for _, c := range cands {
		cc := c
		u, ok := g.sched.Rewrite(plan.Kind, plan.Upstream, cc)
		if !ok || strings.EqualFold(u.Host, plan.Upstream.Host) {
			continue
		}
		head = append(head, fetchTarget{u: u, c: &cc})
	}
	return append(append(head, out...), fbs...)
}

// parallelTargets 给分片并行凑源：普通回退链 3 台就够，
// 分片越多越吃带宽，按 Worker 数取源（最多 8 台），不把
// max_candidates 当成取数上限。
func (g *Gateway) parallelTargets(plan *adapter.Plan) []fetchTarget {
	return g.parallelTargetsN(plan, 0)
}

// parallelTargetsN 与 parallelTargets 相同，但 want>0 时可多要几台候选：
// 中途接入时单流已经慢，多探几台才有机会凑到两台能分片的源。
func (g *Gateway) parallelTargetsN(plan *adapter.Plan, want int) []fetchTarget {
	out := []fetchTarget{{u: plan.Upstream}}
	fbs := g.fallbackTargets(plan, nil)
	if plan.OfficialOnly || !g.sched.Enabled() {
		return append(out, fbs...)
	}
	n := g.par.Workers
	if want > 0 {
		n = want
	}
	if n < 4 {
		n = 4
	}
	if n > 24 {
		n = 24
	}
	var head []fetchTarget
	cands := g.sched.CandidatesN(plan.Kind, n)
	metrics.SourceCandidates(len(cands))
	for _, c := range cands {
		cc := c
		u, ok := g.sched.Rewrite(plan.Kind, plan.Upstream, cc)
		if !ok || strings.EqualFold(u.Host, plan.Upstream.Host) {
			continue
		}
		head = append(head, fetchTarget{u: u, c: &cc})
	}
	return append(append(head, out...), fbs...)
}

// fallbackTargets 把 plan 声明的备用站点排成 target（加速源在前、官方殿后）。
// prev != nil 时只回官方，别让镜像替我们「验证」旧内容。
func (g *Gateway) fallbackTargets(plan *adapter.Plan, prev *store.CacheEntry) []fetchTarget {
	var res []fetchTarget
	for _, fb := range plan.Fallbacks {
		if prev == nil && !plan.OfficialOnly && g.sched != nil && g.sched.Enabled() {
			for _, c := range g.sched.Candidates(fb.Kind) {
				cc := c
				u, ok := g.sched.Rewrite(fb.Kind, fb.URL, cc)
				if !ok || strings.EqualFold(u.Host, fb.URL.Host) {
					continue
				}
				res = append(res, fetchTarget{u: u, c: &cc, fb: true, fbHost: fb.URL.Host})
			}
		}
		res = append(res, fetchTarget{u: fb.URL, fb: true, fbHost: fb.URL.Host})
	}
	return res
}

// fetch 试到哪台源能给出文件为止，客户端看到的是整条链的结论。
// 404/410 记 Miss 不罚源，401/403 换源也不罚，其余才算源故障。
func (g *Gateway) fetch(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry) (*store.CacheEntry, bool, error) {
	if plan.Upstream == nil || plan.Upstream.Host == "" {
		return nil, false, adapter.ErrNoTarget
	}
	targets := g.targets(plan, prev)
	// 裸格式（Docker Hub 名 + ghcr 备用站点）：两条链并行探，谁先给出
	// 文件用谁。串行要等 Docker Hub 那串 401 走完（实测 10 秒以上），
	// 而镜像真在 ghcr 时 ghcr 那条一秒内就有结果。
	if prev == nil && len(plan.Fallbacks) > 0 {
		var main, fb []fetchTarget
		for _, t := range targets {
			if t.fb {
				fb = append(fb, t)
			} else {
				main = append(main, t)
			}
		}
		if len(main) > 0 && len(fb) > 0 {
			return g.fetchRace(ctx, plan, main, fb)
		}
	}
	return g.fetchSeq(ctx, plan, prev, targets)
}

// fetchRace 让两条源链并行探同一份内容，谁先成功用谁，另一条立刻取消。
// 只在裸格式自动识别站点时用：普通请求没有第二条链可并行。
func (g *Gateway) fetchRace(ctx context.Context, plan *adapter.Plan, main, fb []fetchTarget) (*store.CacheEntry, bool, error) {
	type result struct {
		e      *store.CacheEntry
		notMod bool
		source string
		err    error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan result, 2)
	// 两条链并行下载，但只有一条能提交缓存：令牌由先完成者拿走，
	// 另一条内容照常返回、只是不写索引，避免后完成的把缓存覆盖成
	// 另一份 manifest。
	var claimed atomic.Bool
	claim := func() bool { return claimed.CompareAndSwap(false, true) }
	run := func(ts []fetchTarget) {
		// 每条链用独立副本：回源过程会改写 Source 和提交令牌。
		pc := *plan
		pc.Claim = claim
		e, notMod, err := g.fetchSeq(ctx, &pc, nil, ts)
		ch <- result{e, notMod, pc.Source, err}
	}
	go run(main)
	go run(fb)
	var last result
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err == nil {
			cancel() // 另一条不用再试了
			plan.Source = r.source
			return r.e, r.notMod, nil
		}
		if errors.Is(r.err, errRaceLost) {
			continue // 这条只是没抢到提交权，等另一条链的结果
		}
		last = r
	}
	if last.err == nil {
		last.err = errRaceLost
	}
	return nil, false, last.err
}

// settleAction 表示一次上游尝试之后该做什么。
type settleAction int

const (
	settleNext  settleAction = iota // 换下一个源
	settleDone                      // 已经拿到文件
	settleAbort                     // 直接把这个错误交回调用方
)

// errHedgeCanceled 标记「被对冲主动掐掉」的请求。
// 它不是源站故障：不记健康分、不冷却、不进 AIMD。
var errHedgeCanceled = errors.New("upstream canceled by hedge")

// errRaceLost 表示并行竞速里另一条链先拿到提交权：内容本身可用，
// 但不能写缓存，也不能算作源站失败。
var errRaceLost = errors.New("race lost: cache already committed")

// errNotCacheable 是保存路径的兜底：走到这里说明入口分流漏了，
// 绝不能把内容写进公共缓存。
var errNotCacheable = errors.New("plan is not cacheable")

// hedgeRes 是一路对冲请求的结果。
type hedgeRes struct {
	up    *upstream
	idx   int
	start time.Time
	err   error
}

// settleTarget 处理一次上游尝试的结果：记账，并决定结束、换源还是放弃。
func (g *Gateway) settleTarget(ctx context.Context, plan *adapter.Plan, t fetchTarget, start time.Time, e *store.CacheEntry, notMod bool, code int, err error) (settleAction, *store.CacheEntry, bool, error) {
	if errors.Is(err, errHedgeCanceled) {
		// 主动取消是中性事件，不能当成源站失败。
		return settleNext, nil, false, err
	}
	if errors.Is(err, errRaceLost) {
		// 竞速输家：另一条链已经提交了同一份内容，本链直接收工。
		return settleAbort, nil, false, err
	}
	if err == nil {
		if t.c != nil {
			// 成功也要记状态码：429 率的分母要含真实响应，
			// 不记 200 的话分母里只剩失败请求。
			g.sched.Report(plan.Kind, *t.c, true, time.Since(start), scheduler.Hint{Code: code, Mode: "single"})
			if e != nil {
				// 吞吐样本只喂给带宽控制器，不写库：一次回源一条记录就够了。
				g.sched.Observe(t.c.ID, e.Size, time.Since(start), true, code)
				metrics.SourceBytes(t.c.ID, plan.Kind, e.Size)
			}
			plan.Source = t.c.Name
		}
		return settleDone, e, notMod, nil
	}
	// 超上限是策略判断不是源的问题：记失败会误伤冷却，
	// 换源也一样超限，直接交给流式链路。
	var tl *tooLargeError
	if errors.As(err, &tl) {
		return settleAbort, nil, false, err
	}
	// 404/410 是缺文件不是源坏了：按故障处理会误伤冷却，记 Miss 后继续换源。
	var ue *upstreamError
	if errors.As(err, &ue) && isMiss(ue.Code) {
		if t.c != nil {
			// 404/410 也进 429 率分母并按成功计：它答得很正常，只是没这个文件。
			g.sched.Report(plan.Kind, *t.c, true, time.Since(start), scheduler.Hint{Code: ue.Code, Mode: "single"})
		}
		metrics.SourceMiss(plan.Kind, ue.Code)
		name := t.u.Host
		if t.c != nil {
			name = t.c.Name
		}
		loggw(slog.LevelInfo, "源未命中，换下一个", ctx, plan, "", nil, "source", name, "status", ue.Code)
		return settleNext, nil, false, err
	}
	// 401/403 是「没资格问」：换源但不罚源，也不进 Miss 统计。
	if errors.As(err, &ue) && isAuthDenied(ue.Code) {
		if t.c != nil {
			g.sched.Report(plan.Kind, *t.c, true, time.Since(start), scheduler.Hint{Code: ue.Code, Mode: "single"})
		}
		name := t.u.Host
		if t.c != nil {
			name = t.c.Name
		}
		loggw(slog.LevelInfo, "源要求鉴权，换下一个", ctx, plan, logging.AuthTokenInvalid, nil, "source", name, "status", ue.Code)
		return settleNext, nil, false, err
	}
	// 本地故障（磁盘满、缓存提交失败）和客户端取消：换源没用，也不记源站。
	if !isSourceFault(err) {
		return settleAbort, nil, false, err
	}
	if t.c == nil {
		return settleAbort, nil, false, err // 官方源失败就是失败，不再兜圈子
	}
	// 客户端自己取消（关页面、断网）不算这台源的账。
	if ctx.Err() == nil {
		var hint scheduler.Hint
		if errors.As(err, &ue) {
			hint.RetryAfter = ue.RetryAfter
			hint.Code = ue.Code
		}
		g.sched.Report(plan.Kind, *t.c, false, time.Since(start), hint)
		// 带宽控制也要知道它刚失败：429 时照原并发打只会拖长冷却。
		g.sched.Observe(t.c.ID, 0, 0, false, hint.Code)
	}
	loggw(slog.LevelWarn, "源不可用，换下一个", ctx, plan, codeFor(err), err, "source", t.c.Name)
	return settleNext, nil, false, err
}

// fetchSeq 按给定顺序逐个源试，直到有一个交出文件。
// 第一轮会在前两个候选之间做响应头对冲：首选源超过 hedge.After
// 还没吐响应头，就并行发第二个，谁先给出可用响应头用谁。
func (g *Gateway) fetchSeq(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry, targets []fetchTarget) (*store.CacheEntry, bool, error) {
	var lastErr error
	// 回退链（裸格式猜站点）由 fetchRace 做整链并行，里面不再套对冲：
	// 否则每条链各开两路，一个请求会变成四个上游请求。
	hedged := g.hedge.Enabled && len(targets) > 1 && !targets[0].fb && len(plan.Fallbacks) == 0
	for i := 0; i < len(targets); i++ {
		t := targets[i]
		start := time.Now()
		var (
			e      *store.CacheEntry
			notMod bool
			code   int
			err    error
		)
		if hedged && i == 0 {
			var (
				up    *upstream
				idx   int
				tried int
				herr  error
			)
			up, idx, tried, herr = g.pickUpstream(ctx, plan, prev, targets)
			if up == nil {
				if ctx.Err() != nil {
					return nil, false, ctx.Err()
				}
				lastErr = herr
				if tried >= len(targets) {
					return nil, false, lastErr
				}
				i = tried - 1 // 试过的候选不再重来
				continue
			}
			t = targets[idx]
			e, notMod, code, err = g.saveBody(ctx, plan, prev, up, t)
		} else {
			e, notMod, code, err = g.fetchURL(ctx, plan, prev, t.u, t)
		}
		act, entry, nm, lerr := g.settleTarget(ctx, plan, t, start, e, notMod, code, err)
		switch act {
		case settleDone:
			// 只有真从这个站点拿到东西才记住它。之前是「开始试就记」，
			// 主链成功、备链被取消也会写，于是第一次请求就把镜像锁死在
			// ghcr.io，后续全量 404/401（library/golang 实测如此）。
			if t.fb && plan.OnFallback != nil {
				plan.OnFallback(t.fbHost)
			}
			return entry, nm, nil
		case settleAbort:
			return nil, false, lerr
		default:
			lastErr = lerr
		}
	}
	return nil, false, lastErr
}

// pickUpstream 做响应头对冲：先发首选源，hedge.After 内没等到可用响应头，
// 再发第二个源。只把胜者的 Body 交出去，落败请求在这里关掉并取消。
// 返回实际启动过的候选数，调用方据此跳过已经试过的源。
//
// 304 优先的语义：304 一到即胜；条件请求里备用源先回 200 时，再给首选源
// 一个 grace 宽限，看它是否回 304——能省掉一次整文件下载。宽限不是无限等，
// 到点就采用手上这个 200。
func (g *Gateway) pickUpstream(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry, targets []fetchTarget) (*upstream, int, int, error) {
	n := len(targets)
	if n > 2 {
		n = 2
	}
	// 每路请求各自一个 ctx。不能用共享 ctx + defer cancel：那会在函数
	// 返回的瞬间掐断胜者的 Body（大文件必然读到一半报 context canceled）。
	// 落败者由 drop 显式取消，胜者的 ictx 挂到 up.cancel 上，读完再释放。
	ch := make(chan hedgeRes, n)
	cancels := make([]context.CancelFunc, n)
	started := 0
	launch := func(i int) {
		started++
		if i > 0 {
			metrics.HedgeTrigger()
		}
		ictx, icancel := context.WithCancel(ctx)
		cancels[i] = icancel
		go func() {
			st := time.Now()
			up, err := g.openUpstream(ictx, plan, prev, targets[i].u)
			if ictx.Err() != nil {
				if up != nil {
					up.resp.Body.Close()
					up.cancel()
				}
				icancel()
				ch <- hedgeRes{idx: i, start: st, err: errHedgeCanceled}
				return
			}
			ch <- hedgeRes{up: up, idx: i, start: st, err: err}
		}()
	}
	launch(0)
	pending := 1

	var hedgeC <-chan time.Time
	if n > 1 {
		tm := time.NewTimer(g.hedge.After)
		defer tm.Stop()
		hedgeC = tm.C
	}
	var graceC <-chan time.Time
	var held *hedgeRes
	var lastErr error
	win := -1
	defer func() {
		// 只有真发过第二路才算对冲胜出；单路成功不该进这个指标。
		if win >= 0 && started > 1 {
			metrics.HedgeWin(win + 1)
		}
	}()

	drop := func(r hedgeRes) {
		if r.up != nil {
			// 直接断，不 Drain：大文件 Drain 等于白读一遍流量。
			r.up.resp.Body.Close()
			r.up.cancel()
		}
		if c := cancels[r.idx]; c != nil {
			c()
		}
	}

	// take 把这一路的 ictx 释放挂到 up.cancel 上：调用方读完 Body 再调，
	// 既清理落败路径又不会提前掐断胜者的下载。
	take := func(r hedgeRes) *upstream {
		// 先把别的路都掐掉，免得它们跑满超时才退（测试里就是白等 2s）。
		for j, c := range cancels {
			if j != r.idx && c != nil {
				c()
			}
		}
		if r.up != nil && cancels[r.idx] != nil {
			oc, ic := r.up.cancel, cancels[r.idx]
			r.up.cancel = func() { oc(); ic() }
		}
		return r.up
	}

	for pending > 0 || held != nil {
		select {
		case r := <-ch:
			pending--
			if r.err != nil {
				if !errors.Is(r.err, errHedgeCanceled) {
					lastErr = r.err
				}
				// 快速失败（404/429/连不上）：不必等计时器，立刻补下一个。
				if started < n {
					launch(started)
					pending++
				}
				continue
			}
			switch r.up.resp.StatusCode {
			case http.StatusNotModified:
				// 304 是条件请求的最优结果：一到即胜，不产生任何下载。
				if held != nil {
					drop(*held)
				}
				win = r.idx
				return take(r), r.idx, started, nil
			case http.StatusOK:
				if prev != nil && started > 1 && held == nil {
					// 备用源先回了 200：给首选源一个宽限窗口等它的 304。
					rr := r
					held = &rr
					gt := time.NewTimer(g.hedge.Grace)
					defer gt.Stop()
					graceC = gt.C
					continue
				}
				if held != nil {
					// 宽限期内首选源也回了 200：两个都是完整响应，
					// 用排名靠前的那个，后到的丢掉。
					drop(r)
					win = held.idx
					return take(*held), held.idx, started, nil
				}
				win = r.idx
				return take(r), r.idx, started, nil
			default:
				code := r.up.resp.StatusCode
				var hint scheduler.Hint
				hint.Code = code
				hint.RetryAfter = retryAfterHeader(r.up.resp.Header)
				drop(r)
				// 404/410/401/403 答得正常、只是没文件/没资格，按成功记；
				// 429/5xx 才是源站问题，记失败并带上 Retry-After。
				ok := isMiss(code) || isAuthDenied(code)
				if t := targets[r.idx]; t.c != nil {
					g.sched.Report(plan.Kind, *t.c, ok, time.Since(r.start), hint)
					if !ok {
						g.sched.Observe(t.c.ID, 0, 0, false, code)
					}
				}
				lastErr = &upstreamError{Code: code, RetryAfter: hint.RetryAfter}
				if started < n {
					launch(started)
					pending++
				}
			}
		case <-hedgeC:
			hedgeC = nil
			if started < n {
				launch(started)
				pending++
			}
		case <-graceC:
			graceC = nil
			h := held
			held = nil
			win = h.idx
			return take(*h), h.idx, started, nil
		case <-ctx.Done():
			if held != nil {
				drop(*held)
			}
			return nil, 0, started, ctx.Err()
		}
	}
	if held != nil {
		win = held.idx
		return take(*held), held.idx, started, nil
	}
	if lastErr == nil {
		lastErr = &upstreamError{Code: 0}
	}
	return nil, 0, started, lastErr
}

// fetchURL 返回上游状态码；0 表示没拿到响应。
// 调用方据此区分「回了 429」和「没理我们」。
// upstream 是一次已经拿到响应头的上游请求。Body 的所有权在调用方手里：
// 胜者读、落败者 Close；cancel 用来主动掐掉落败请求。
// 拆出这一层是为了做响应头对冲（hedging）：先比 header，再让胜者下 Body。
type upstream struct {
	resp   *http.Response
	u      *url.URL
	cancel context.CancelFunc
}

// openUpstream 只负责到响应头为止，不读 Body。
// 返回的 cancel 必须由调用方在读完/关闭 Body 后调用，否则子 context 泄漏。
func (g *Gateway) openUpstream(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry, u *url.URL) (*upstream, error) {
	uctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(uctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("User-Agent", "mirror-lens/"+Version+" (+https://github.com/zggnh12345/mirror-lens)")
	// 必须拿原始字节：压缩后的内容算不出上游的哈希，也校验不了长度
	req.Header.Set("Accept-Encoding", "identity")
	for k, vs := range plan.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if prev != nil {
		if prev.ETag != "" {
			req.Header.Set("If-None-Match", prev.ETag)
		} else if prev.LastModified != "" {
			req.Header.Set("If-Modified-Since", prev.LastModified)
		}
	}

	resp, err := g.cli.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	return &upstream{resp: resp, u: u, cancel: cancel}, nil
}

// saveBody 接管胜者的响应体：状态判断、落盘、校验、提交。
// 304 直接复用 prev，不产生任何下载。
func (g *Gateway) saveBody(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry, up *upstream, t fetchTarget) (*store.CacheEntry, bool, int, error) {
	resp := up.resp
	defer func() {
		resp.Body.Close()
		up.cancel()
	}()

	if resp.StatusCode == http.StatusNotModified {
		return prev, true, http.StatusNotModified, nil
	}
	if resp.StatusCode != http.StatusOK {
		io.CopyN(io.Discard, resp.Body, 4096)
		return nil, false, resp.StatusCode, &upstreamError{Code: resp.StatusCode, RetryAfter: retryAfterHeader(resp.Header)}
	}
	if plan.Validate != nil {
		if err := plan.Validate(resp); err != nil {
			return nil, false, 0, err
		}
	}

	tmp, err := g.cache.CreateTemp()
	if err != nil {
		return nil, false, 0, local(fmt.Errorf("创建临时文件失败: %w", err))
	}
	tmpName := tmp.Name()
	// 无论走到哪条分支都要注销：否则这个文件名会被 SweepTemp 永久跳过。
	defer g.cache.ForgetTemp(tmpName)
	cleanup := func() {
		tmp.Close()
		removeFile(tmpName)
	}

	h := sha256.New()
	// 声明长度就超上限时一个字节都不下：否则要白下 maxSize 才报错。
	if resp.ContentLength > g.maxSize {
		resp.Body.Close()
		return nil, false, 0, &tooLargeError{Max: g.maxSize}
	}
	// 面板：回源这一段单独记一条任务。GitHub/git 走的是「先整份落盘再下发」，
	// 以前这条链路一个任务都不登记，大文件下载时面板永远空着。
	var ftask *liveTask
	ftaskOK := false
	if resp.ContentLength >= liveMinBytes {
		ftask = g.tasks.start(plan.Kind, planLabel(plan), "fetch", resp.ContentLength, nil)
	}
	defer func() { ftask.finish(ftaskOK) }()

	dw := &diskWriter{w: tmp}
	sink := io.Writer(dw)
	if ftask != nil {
		sink = &taskOnlyWriter{w: dw, t: ftask}
	}
	start := time.Now()
	n, err := io.Copy(io.MultiWriter(sink, h), io.LimitReader(resp.Body, g.maxSize+1))
	if err != nil {
		cleanup()
		// io.Copy 只会停在第一个出错的 writer 上，所以要分开看是盘还是网。
		if dw.err != nil {
			return nil, false, 0, local(fmt.Errorf("写本地缓存失败: %w", dw.err))
		}
		return nil, false, 0, fmt.Errorf("读取上游中断: %w", err)
	}
	if n > g.maxSize {
		cleanup()
		return nil, false, 0, &tooLargeError{Max: g.maxSize}
	}
	// 上游声明了长度就必须一致，否则是截断的响应
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		cleanup()
		return nil, false, 0, fmt.Errorf("长度不符：期望 %d，实际 %d", resp.ContentLength, n)
	}
	// 记真实回源吞吐：这是单流独占带宽下量到的持续速度，
	// 比探测那 4MB 准得多。小文件慢启动占比高，不记。
	if t.c != nil && g.st != nil && n >= minSpeedSample {
		if el := time.Since(start).Milliseconds(); el > 0 {
			_ = g.st.AddProxySpeed(t.c.ID, plan.Kind, n, el)
		}
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, false, 0, local(fmt.Errorf("落盘失败: %w", err))
	}
	if err := tmp.Close(); err != nil {
		removeFile(tmpName)
		return nil, false, 0, local(fmt.Errorf("关闭临时文件失败: %w", err))
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if plan.ExpectSHA != "" && !strings.EqualFold(sum, plan.ExpectSHA) {
		removeFile(tmpName)
		g.verifyFail.Add(1)
		return nil, false, 0, fmt.Errorf("内容与客户端给的摘要不符：期望 %s 实得 %s", plan.ExpectSHA, sum)
	}
	if checked, ok := checkDigest(resp.Header, sum); checked && !ok {
		removeFile(tmpName)
		g.verifyFail.Add(1)
		return nil, false, 0, fmt.Errorf("内容与上游声明的摘要不符：%s", plan.Upstream)
	}
	e := &store.CacheEntry{
		Key:          plan.CacheKey,
		SHA256:       sum,
		Size:         n,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Upstream:     up.u.String(),
		CreatedAt:    g.now().Unix(),
		TTL:          entryTTL(plan),
		Verify:       verifyState(plan),
	}
	// 兜底：入口分流漏了也不能落盘；竞速里没抢到令牌的链直接放弃提交。
	if plan.NoStore {
		cleanup()
		return nil, false, 0, errNotCacheable
	}
	if plan.Claim != nil && !plan.Claim() {
		cleanup()
		return nil, false, 0, errRaceLost
	}
	// 落盘和写索引必须在同一把 blob 锁内：janitor 在锁内查引用再删，
	// 否则「文件已就位、索引未写」的瞬间会被当孤儿清掉。
	unlock := g.cache.LockBlob(sum)
	if _, err := g.cache.Commit(tmpName, sum); err != nil {
		unlock()
		removeFile(tmpName)
		return nil, false, 0, local(fmt.Errorf("提交缓存失败: %w", err))
	}
	err = g.st.CachePut(*e)
	unlock()
	if err != nil {
		loggw(slog.LevelError, "写缓存索引失败", ctx, plan, logging.CacheCommitFailed, err)
	}
	g.st.RecordTraffic(store.TrafficFill, store.KindOfKey(plan.CacheKey), n)
	ftaskOK = true
	return e, false, http.StatusOK, nil
}

// fetchURL 是「单源完整取」的薄包装：非对冲路径与旧行为完全一致。
func (g *Gateway) fetchURL(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry, u *url.URL, t fetchTarget) (*store.CacheEntry, bool, int, error) {
	up, err := g.openUpstream(ctx, plan, prev, u)
	if err != nil {
		return nil, false, 0, err
	}
	return g.saveBody(ctx, plan, prev, up, t)
}

// fromCache=false 表示刚回源填充：字节已记 fill，再记 hit 会抬高命中率。
// hit 字节按实际写出的量算，Range 只要 10 MB 就记 10 MB。
func (g *Gateway) serveCached(w http.ResponseWriter, r *http.Request, plan *adapter.Plan, e *store.CacheEntry, label string, fromCache bool) {
	if e == nil {
		http.Error(w, "502 缓存条目缺失", http.StatusBadGateway)
		return
	}
	f, err := g.cache.Open(e.SHA256)
	if err != nil {
		// 索引有、文件没了：删掉索引，让下次重新回源
		slog.Warn("缓存文件丢失", "cache_key", logging.SanitizeKey(e.Key), "sha256", e.SHA256, "error", err, "error_code", logging.CacheReadFailed)
		_ = g.st.CacheDelete(e.Key)
		http.Error(w, "502 缓存文件丢失，请重试", http.StatusBadGateway)
		return
	}
	defer f.Close()

	if e.ContentType != "" {
		w.Header().Set("Content-Type", e.ContentType)
	}
	// ETag 用内容哈希：强校验、跨源稳定，客户端可以拿它做 If-None-Match
	w.Header().Set("ETag", `"`+e.SHA256+`"`)
	g.cacheHeaders(w.Header(), e, g.now())
	w.Header().Set("X-ML-Cache", label)
	// 面板：从磁盘下发也记一条任务。Docker 命中缓存、GitHub 回源完的下发
	// 都走这条路，以前这里一个任务都不登记，所以面板看着像「没动静」。
	var task *liveTask
	if r.Method == http.MethodGet && e.Size >= liveMinBytes {
		mode := "cache"
		if !fromCache {
			mode = "served"
		}
		var kind string
		if plan != nil {
			kind = plan.Kind
		}
		task = g.tasks.start(kind, planLabel(plan), mode, e.Size, nil)
		defer task.finish(true)
	}
	cw := &countWriter{ResponseWriter: w, t: task, through: fromCache}
	// ServeContent 负责 Range / 206 / 416 / If-None-Match，不用自己写
	http.ServeContent(cw, r, path.Base(e.Key), time.Unix(e.CreatedAt, 0), f)
	if fromCache && task == nil {
		g.tasks.addThrough(cw.n)
		g.st.CacheHit(e.Key)
		if cw.n > 0 {
			g.st.RecordTraffic(store.TrafficHit, store.KindOfKey(e.Key), cw.n)
		}
	}
}

func (g *Gateway) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 匿名放行模式下完全不碰凭据：面板仍然要登录，代理路径直接给。
		if !g.requireToken {
			next(w, r)
			return
		}
		// 默认密码时不认 cookie：admin/admin 是公开的，会话等于绕过
		// Access Token。面板登录照旧，要留一条改密码的路。
		if auth.Authed(r, g.st) && !auth.MustChange(g.st) {
			next(w, r)
			return
		}
		if tok := proxyToken(r); tok != "" {
			if _, ok := g.st.VerifyToken(tok); ok {
				// 这是网关自己的访问令牌，不是上游凭据。必须摘掉：
				// adapter 会把它当 GitHub token 透传出去，请求也因此
				// 被当成「带凭据」而不落缓存。
				r.Header.Del("Authorization")
				next(w, r)
				return
			}
		}
		// Basic 让 git/docker 客户端弹认证框；密码位填 Access Token
		noStore(w.Header())
		w.Header().Set("WWW-Authenticate", `Basic realm="Mirror Lens", charset="UTF-8"`)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "401 需要 Access Token\nAuthorization: Bearer <token>\n")
	}
}

func removeFile(name string) { _ = os.Remove(name) }

// ActiveTasks 给面板用：正在传的任务 + 最近完成 + 整机 60 秒吞吐。
func (g *Gateway) ActiveTasks() ActiveSnapshot {
	if g == nil {
		return ActiveSnapshot{}
	}
	return g.tasks.snapshot()
}

// planLabel 是面板显示的资源名；适配器没给就退回缓存键。
func planLabel(plan *adapter.Plan) string {
	if plan == nil {
		return ""
	}
	if plan.Label != "" {
		return plan.Label
	}
	return plan.CacheKey
}

// countWriter 只数真正写给客户端的字节，304 和 Range 都不会多算。
type countWriter struct {
	http.ResponseWriter
	n       int64
	t       *liveTask
	through bool // 是否计入整机吞吐：回源已经记过的字节不要再记一遍
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	if c.t != nil && n > 0 {
		if c.through {
			c.t.add(int64(n))
		} else {
			c.t.addOnly(int64(n))
		}
	}
	return n, err
}

func entryTTL(plan *adapter.Plan) int64 {
	if plan.Immutable {
		return 10 * 365 * 24 * 3600
	}
	return int64(plan.TTL.Seconds())
}

func proxyToken(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	if u, p, ok := r.BasicAuth(); ok {
		if p != "" {
			return p
		}
		// git 客户端常写成 http://<token>@host/，密码位是空的
		return u
	}
	return ""
}

// loggw 统一日志字段，错误必带 error_code 便于聚合。
// 不要塞 URL/Token/Cookie，只传脱敏过的地址。
func loggw(level slog.Level, msg string, ctx context.Context, plan *adapter.Plan, code logging.Code, err error, extra ...any) {
	attrs := []any{
		"request_id", logging.RequestID(ctx),
		"kind", plan.Kind,
		"cache_key", logging.SanitizeKey(plan.CacheKey),
	}
	if plan.Source != "" {
		attrs = append(attrs, "source", plan.Source)
	}
	if code != "" {
		attrs = append(attrs, "error_code", code)
	}
	if err != nil {
		// 错误文本里常带完整 URL（*url.Error），这里统一剥掉查询串，
		// 保留 %w 链以便 errors.Is/As 照常工作。
		attrs = append(attrs, "error", logging.SafeErr(err))
	}
	attrs = append(attrs, extra...)
	slog.Log(ctx, level, msg, attrs...)
}

// safeUpstream 把上游地址转成可进日志的骨架：去掉查询串和 userinfo，
// 保留 host 与路径。只用于输出，原始 *url.URL 不动。
func safeUpstream(u *url.URL) string {
	if u == nil {
		return ""
	}
	return logging.SanitizeURL(u.String())
}

// codeFor 只做粗粒度归类：日志要能聚合，不必精确到 errno。
func codeFor(err error) logging.Code {
	var ue *upstreamError
	if errors.As(err, &ue) {
		if ue.Code == http.StatusTooManyRequests {
			return logging.UpstreamRateLimited
		}
		return logging.UpstreamHTTPError
	}
	var le *localError
	if errors.As(err, &le) {
		return logging.CacheCommitFailed
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return logging.UpstreamTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return logging.UpstreamTimeout
	}
	return logging.UpstreamConnectFailed
}

type upstreamError struct {
	Code int
	// RetryAfter 是 429 响应里源自己说的等待时间，解析不出来就是 0。
	RetryAfter time.Duration
}

func (e *upstreamError) Error() string { return fmt.Sprintf("上游返回 %d", e.Code) }

// retryAfterHeader 解析 Retry-After（秒数或 HTTP-date）。
// 解析不出、或时间已过去，都当没有这个头。
func retryAfterHeader(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// localError 标记本地故障（盘满、提交失败）：换源也一样失败，
// 记到源站账上会把一排好源误伤成冷却/熔断。
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }

func local(err error) error { return &localError{err: err} }

// isSourceFault：本地故障、超上限、客户端取消都不算源的账。
// 其余（连不上、超时、5xx、内容/长度对不上）才算源故障。
func isSourceFault(err error) bool {
	if err == nil {
		return false
	}
	var le *localError
	if errors.As(err, &le) {
		return false
	}
	var tl *tooLargeError
	if errors.As(err, &tl) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// diskWriter 记录写盘是否失败，用来分开本地故障和上游中断。
type diskWriter struct {
	w   io.Writer
	err error
}

func (d *diskWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if err != nil {
		d.err = err
	}
	return n, err
}

type tooLargeError struct{ Max int64 }

func (e *tooLargeError) Error() string {
	return fmt.Sprintf("文件超过单文件上限 %d MB", e.Max>>20)
}

// isMiss：404/410 是「没有这个资源」，不是源站出问题。
func isMiss(code int) bool {
	return code == http.StatusNotFound || code == http.StatusGone
}

// isAuthDenied 是「没资格问」而不是「没有」：不算源站故障，
// 但也不进 Miss 统计（资源在不在没答上来）。
func isAuthDenied(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

// setRetryAfter 把源站给的等待时间原样转给客户端；没有就什么都不做。
func setRetryAfter(w http.ResponseWriter, err error) {
	var ue *upstreamError
	if errors.As(err, &ue) && ue.Code == http.StatusTooManyRequests && ue.RetryAfter > 0 {
		secs := int(ue.RetryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
}

func statusFor(err error) int {
	var ue *upstreamError
	if errors.As(err, &ue) {
		switch ue.Code {
		case http.StatusNotFound, http.StatusGone:
			return http.StatusNotFound
		case http.StatusForbidden:
			return http.StatusForbidden
		case http.StatusTooManyRequests:
			// 429 是「等会儿再来」，不是坏网关。原样透出，客户端才会退避；
			// 折成 502 会让客户端立刻重试，把限流打成雪崩。
			return http.StatusTooManyRequests
		case http.StatusServiceUnavailable:
			return http.StatusServiceUnavailable
		}
		return http.StatusBadGateway
	}
	var tl *tooLargeError
	if errors.As(err, &tl) {
		return http.StatusRequestEntityTooLarge
	}
	if errors.Is(err, safehttp.ErrBlocked) || errors.Is(err, adapter.ErrReadOnly) {
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}

// limitFor 返回这次请求的落盘上限：协议自己带了大文件阈值就用自己的。
func (g *Gateway) limitFor(plan *adapter.Plan) int64 {
	if plan.MaxBytes > 0 {
		return plan.MaxBytes
	}
	return g.maxSize
}
