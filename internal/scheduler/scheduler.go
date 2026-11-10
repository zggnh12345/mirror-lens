// Package scheduler 决定一次回源该走哪个镜像源。
//
// 评分只用探测样本（tier>=2），代理快慢是用户到本机，混进来会失真；
// 只有回源才选源，TTL 重新验证一律回官方源；熔断按 (协议, 源) 记。
package scheduler

import (
	"context"
	"log"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

type Config struct {
	Enabled       bool
	MaxCandidates int
	StickyTTL     time.Duration
	FailCooldown  time.Duration
	ListTTL       time.Duration
	// Dynamic 打开后按实测吞吐在 1..MaxPerSource 间自动调整并发（AIMD）。
	Dynamic bool
	// MaxPerSource 是单台源的回源并发上限，0 取 4，负数不限。
	// 只约束预检/分片，不约束流式转发——卡住用户下载等于拒绝服务。
	MaxPerSource int
	// MaxCooldown 是单次冷却上限：Retry-After: 86400 不该把源关一天。
	MaxCooldown time.Duration
	// Headroom 是普通派发占硬上限的比例（3.0 默认 0.8）。
	Headroom float64
	// Admission 是准入模式：observe / blocking / wait_then_fallback / non_blocking。
	Admission string
	// WaitTimeout 是 wait_then_fallback 的最长等待。
	WaitTimeout time.Duration
	// SpeedCapKBps 是速度项的饱和点（KB/s）：达到它即满分。
	// 生产 P75≈12MB/s；沿用 1.5MB/s 会让 77% 的源都拿满分，速度维失去区分度。
	SpeedCapKBps float64
}

func (c *Config) fill() {
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = 3
	}
	if c.StickyTTL <= 0 {
		c.StickyTTL = 10 * time.Minute
	}
	if c.FailCooldown <= 0 {
		c.FailCooldown = 5 * time.Minute
	}
	if c.ListTTL <= 0 {
		c.ListTTL = 30 * time.Second
	}
	if c.MaxCooldown <= 0 {
		c.MaxCooldown = time.Hour
	}
	if c.MaxPerSource == 0 {
		c.MaxPerSource = 4
	}
	if c.Headroom <= 0 || c.Headroom > 1 {
		c.Headroom = 0.8
	}
	if c.Admission == "" {
		c.Admission = "wait_then_fallback"
	}
	if c.WaitTimeout <= 0 {
		c.WaitTimeout = 2 * time.Second
	}
	if c.SpeedCapKBps <= 0 {
		c.SpeedCapKBps = 12000 // 12 MB/s，生产 P75
	}
}

type Candidate struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	BaseURL string  `json:"base_url"`
	Score   float64 `json:"score"`
	// Probe 表示半开试探请求，本次结果决定它回到可用还是继续熔断。
	Probe bool `json:"probe,omitempty"`
	// Trust 是内容可信度（-1 不符 … 3 长期一致），只用于同分排序，不参与评分。
	Trust int `json:"trust"`
	// Preferred 是用户手动指定的优先源，同等可用性下排最前。
	Preferred bool `json:"preferred"`
	// GroupRank 是分组的优先级，越小越先试；未分组为 999。
	GroupRank int `json:"group_rank"`
	// GhcrPrefix 代 ghcr 时用带 ghcr.io 前缀的路径（见 store.Source.GhcrPrefix）。
	GhcrPrefix bool `json:"ghcr_prefix,omitempty"`
	// RangeCap / RangeAt 是上次 Range 预检的结论与时间，用来跳过重复探测。
	RangeCap int   `json:"range_cap,omitempty"`
	RangeAt  int64 `json:"range_at,omitempty"`
	// SpeedKBps 是该源最近的速度估计（EWMA），用来估并行/单流哪条更快。
	SpeedKBps int64 `json:"speed_kbps,omitempty"`
}

type stickyPick struct {
	cand Candidate
	at   time.Time
}

// breaker 是单个 (协议, 源) 的熔断状态：fails=0 闭合，now<until 打开，
// now>=until 半开——放一个探测，成功闭合，失败冷却翻倍。
type breaker struct {
	fails   int
	until   time.Time
	probing bool
	probeAt time.Time
}

const (
	// 半开探测在飞超过此时长视为卡死，允许再放一个。
	probeStale = 30 * time.Second
	// 冷却最长翻 8 倍（默认 5m → 40m），免得坏源每分钟被戳一次。
	maxBackoffShift = 3
)

type listEntry struct {
	items []Candidate
	at    time.Time
}

type Scheduler struct {
	st  *store.Store
	cfg Config
	now func() time.Time

	mu     sync.Mutex
	lists  map[string]listEntry
	sticky map[string]stickyPick
	brk    map[string]*breaker // "kind|id"
	// 手动调度：manual 是每种协议的前 3 个源（有序），manualMode=manual 才生效。
	// 与候选列表同生命周期刷新（30s），改设置时 API 会主动失效缓存。
	manualMode string
	manual     map[string][]int64

	loadMu sync.Mutex
	gates  map[int64]*gate // 源 id → 并发闸门（容量可动态调整）
	dyn    map[int64]*dynState
	leases *leaseManager // 3.0 统一回源账本（A 档）

	// gone 是已删除源的墓碑，挡住迟到回调把状态重建出来。
	// 用 sync.Map 避开与 Forget（s.mu → loadMu）的反向锁序。
	gone sync.Map // int64 → struct{}
}

func New(st *store.Store, cfg Config) *Scheduler {
	cfg.fill()
	if cfg.Dynamic {
		// 3.0 起容量由 headroom 固定换算，AIMD 不再参与派发。
		log.Printf("sources.dynamic 在 3.0 已失效（容量改由 headroom 决定），本实例按固定配额运行")
		cfg.Dynamic = false
	}
	return &Scheduler{
		st:     st,
		cfg:    cfg,
		now:    time.Now,
		lists:  map[string]listEntry{},
		sticky: map[string]stickyPick{},
		brk:    map[string]*breaker{},
		gates:  map[int64]*gate{},
		dyn:    map[int64]*dynState{},
		leases: newLeaseManager(),
	}
}

func (s *Scheduler) Enabled() bool { return s != nil && s.cfg.Enabled }

// Invalidate 丢掉候选缓存。状态、开关、协议健康一变就调用，
// 否则刚判死的源还能在 ListTTL 内被选中。
func (s *Scheduler) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lists = map[string]listEntry{}
	s.sticky = map[string]stickyPick{}
	s.mu.Unlock()
}

// Forget 清掉某个源的全部运行态：候选缓存、熔断、闸门、吞吐、逐源指标。
// id 可能被新源复用，留着旧闸门和熔断会让新源一上来就被限流。
func (s *Scheduler) Forget(id int64) {
	if s == nil || id == 0 {
		return
	}
	suffix := "|" + strconv.FormatInt(id, 10)
	s.mu.Lock()
	// 先立墓碑再清状态：晚到的回调看到墓碑才不会再写回来。
	s.gone.Store(id, struct{}{})
	for k := range s.brk {
		if strings.HasSuffix(k, suffix) {
			delete(s.brk, k)
		}
	}
	delete(s.dyn, id)
	s.lists = map[string]listEntry{}
	s.sticky = map[string]stickyPick{}
	s.mu.Unlock()

	s.loadMu.Lock()
	delete(s.gates, id)
	s.loadMu.Unlock()
	s.leases.forget(id)

	metrics.ForgetSource(id)
}

// Revive 撤销删除墓碑。id 正常不会复用，这里是防御性的：
// 没有它，复用的新源会被当成已删除，报告和闸门全被静默丢弃。
func (s *Scheduler) Revive(id int64) {
	if s == nil || id == 0 {
		return
	}
	s.gone.Delete(id)
}

// isGone 报告该 id 是否已被删除，任何锁状态下都能问。
func (s *Scheduler) isGone(id int64) bool {
	_, ok := s.gone.Load(id)
	return ok
}

// deleted 是 isGone 的别名，给不持有 s.mu 的调用方用。
func (s *Scheduler) deleted(id int64) bool { return s.isGone(id) }

// Candidates 返回按优先级排好的源，最多 MaxCandidates 个。
func (s *Scheduler) Candidates(kind string) []Candidate {
	return s.CandidatesN(kind, s.cfg.MaxCandidates)
}

// CandidatesN 取前 limit 个源。分片并行要按 Worker 数凑源，
// 不该被回退链长度（max_candidates）卡住。
func (s *Scheduler) CandidatesN(kind string, limit int) []Candidate {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	if le, ok := s.lists[kind]; ok && s.now().Sub(le.at) < s.cfg.ListTTL {
		out := s.orderN(kind, le.items, limit)
		s.mu.Unlock()
		return out
	}
	s.mu.Unlock()

	// 查库不能在 s.mu 里：这里两条 SQL，慢查询的时候 Observe/Acquire
	// 全被堵住，表现是「所有源一起变慢」，还会被 AIMD 记到源头上。
	q0 := time.Now()
	stats, err := s.st.SourceCandidates(kind)
	if d := time.Since(q0); d > 200*time.Millisecond {
		log.Printf("调度：SourceCandidates(%s) 耗时 %v（锁外）", kind, d)
	}
	if err != nil {
		return nil
	}
	cooling, cerr := s.st.SourceFailedRecently(kind, s.now().Add(-s.cfg.FailCooldown).Unix())
	mo := s.st.GetSchedOrder()

	s.mu.Lock()
	defer s.mu.Unlock()
	// 等锁期间别人可能已经填好，直接用，别重复查库也避免覆盖更新的列表。
	if le, ok := s.lists[kind]; ok && s.now().Sub(le.at) < s.cfg.ListTTL {
		return s.orderN(kind, le.items, limit)
	}
	// 重启后从事件表恢复最近失败的源，接着熔断而不是重新试错。
	if cerr == nil {
		for _, st := range stats {
			k := pkey(kind, st.ID)
			if cooling[st.ID] && s.brk[k] == nil {
				s.brk[k] = &breaker{fails: 1, until: s.now().Add(s.cfg.FailCooldown)}
			}
		}
	}
	s.manualMode, s.manual = mo.Mode, mo.Orders
	items := make([]Candidate, 0, len(stats))
	for _, st := range stats {
		items = append(items, Candidate{ID: st.ID, Name: st.Name, BaseURL: st.BaseURL, Score: score(st, s.cfg.SpeedCapKBps), Trust: st.Trust, Preferred: st.Preferred, GroupRank: st.GroupRank, GhcrPrefix: st.GhcrPrefix, RangeCap: st.RangeCap, RangeAt: st.RangeAt, SpeedKBps: st.SpeedKBps})
	}
	s.lists[kind] = listEntry{items: items, at: s.now()}
	return s.orderN(kind, items, limit)
}

// groupKey 把「未分组」归到最后一档：0 表示没有分组，组 rank 从 1 起。
func groupKey(rank int) int {
	if rank <= 0 {
		return 999
	}
	return rank
}

// order 叠加熔断与粘性偏好。
//
// 打开的直接排除（降权仍会排前面，白等一次超时）；半开的排队尾，有健康源
// 时不拿真实请求去赌；冷却逾期一个周期未探，则插队首只试一次。
func (s *Scheduler) order(kind string, in []Candidate) []Candidate {
	return s.orderN(kind, in, 0)
}

func (s *Scheduler) orderN(kind string, in []Candidate, limit int) []Candidate {
	now := s.now()
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if b := s.brk[pkey(kind, c.ID)]; b != nil && b.fails > 0 {
			if now.Before(b.until) {
				continue
			}
			if b.probing && now.Sub(b.probeAt) < probeStale {
				continue
			}
			b.probing, b.probeAt = true, now
			c.Probe = true
			c.Score = -1
			if now.Sub(b.until) >= s.cfg.FailCooldown {
				c.Score = 2 // 逾期未探，插队首只试一次
			}
		}
		if sp, ok := s.sticky[kind]; ok && sp.cand.ID == c.ID && now.Sub(sp.at) < s.cfg.StickyTTL {
			c.Score += 0.5 // 刚成功的源优先复用，省一次试错
		}
		out = append(out, c)
	}
	// 占满名额的源整体后移，免得最快的那台被所有任务一起压垮。
	// 分组优先级压过容量：高优先组里某台满了，也应该先试同组的下一台，
	// 而不是让低优先组插队（3.0 排序修正）。
	// atCap 要拿 leaseSet 的锁；放在比较器里会被调用 O(n log n) 次。
	// 排序前拍一次快照，结果等价，锁竞争降到 O(n)。
	capAt := make(map[int64]bool, len(out))
	for _, c := range out {
		capAt[c.ID] = s.atCap(c.ID)
	}
	// 手动名次：用户设定的前 3 个源排在所有自动排序之前，其余统一给 3。
	manualRank := map[int64]int{}
	if s.manualMode == "manual" {
		for i, id := range s.manual[store.SchedKind(kind)] {
			if i >= 3 {
				break
			}
			manualRank[id] = i
		}
	}
	mrank := func(id int64) int {
		if r, ok := manualRank[id]; ok {
			return r
		}
		return 3
	}
	sort.SliceStable(out, func(i, j int) bool {
		if mi, mj := mrank(out[i].ID), mrank(out[j].ID); mi != mj {
			return mi < mj
		}
		if gi, gj := groupKey(out[i].GroupRank), groupKey(out[j].GroupRank); gi != gj {
			return gi < gj
		}
		if bi, bj := capAt[out[i].ID], capAt[out[j].ID]; bi != bj {
			return !bi
		}
		// 组内再看手动优先。
		if out[i].Preferred != out[j].Preferred {
			return out[i].Preferred
		}
		if out[i].Score == out[j].Score {
			// 同分看可信度：分数只看可用率/延迟/速度，可信度是另一条轴。
			if out[i].Trust != out[j].Trust {
				return out[i].Trust > out[j].Trust
			}
			if li, lj := s.InFlight(out[i].ID), s.InFlight(out[j].ID); li != lj {
				return li < lj
			}
			return out[i].ID < out[j].ID
		}
		return out[i].Score > out[j].Score
	})
	if limit <= 0 {
		limit = s.cfg.MaxCandidates
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Acquire 是 2.x 的兼容入口，返回的释放函数必须调用（可重复）。
//
// observe 阶段保留旧的准入计数，同时向 Lease 账本登记，避免两套生命周期；
// 正式准入阶段（Step 8 起）只走 Lease，一次请求一个名额。
func (s *Scheduler) Acquire(ctx context.Context, id int64) (func(), error) {
	l, tok, done, err := s.AcquireBody(ctx, id)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if l != nil {
				l.Release(tok)
			}
			if done != nil {
				done()
			}
		})
	}, nil
}

// AcquireBody 与 Acquire 语义完全一致（含 observe 模式仍走旧 gate），
// 区别是把名额凭证交回调用方：由 Body 的生命周期负责 Release，
// done 只负责闸门/在途计数。分片路径用它把 Body 纳入 Lease 账本。
func (s *Scheduler) AcquireBody(ctx context.Context, id int64) (*Lease, Token, func(), error) {
	noop := func() {}
	if s == nil || id == 0 || s.cfg.MaxPerSource <= 0 {
		return nil, Token{}, noop, nil
	}
	if s.deleted(id) {
		// 源在下载途中被删：不建闸门直接放行，在途请求不该失败。
		return nil, Token{}, noop, nil
	}

	if s.cfg.Admission != "observe" {
		l, err := s.AcquireLease(ctx, id, KindNormal)
		if err != nil {
			return nil, Token{}, nil, err
		}
		if l == nil {
			return nil, Token{}, noop, nil
		}
		metrics.SourceInflight(id, 1)
		var once sync.Once
		done := func() { once.Do(func() { metrics.SourceInflight(id, -1) }) }
		return l, l.Token(), done, nil
	}

	release, err := s.gateFor(id).acquire(ctx)
	if err != nil {
		return nil, Token{}, nil, err
	}
	l, _ := s.acquireLeaseMode(ctx, id, KindNormal, "observe")
	metrics.SourceInflight(id, 1)
	var once sync.Once
	done := func() {
		once.Do(func() {
			release()
			metrics.SourceInflight(id, -1)
		})
	}
	if l == nil {
		return nil, Token{}, done, nil
	}
	return l, l.Token(), done, nil
}

// ReclaimSource 回收调用时该源已存在的全部 Lease，返回回收数量。
// 注意它只作用于当前快照：调用之后新申请的 Lease 不受影响，
// 因此不等同于源下线/隔离；真正的踢源需要先阻止新 Lease 进入。
// Body 仍持有引用时，名额等最后一个 Body 退出才归还。
func (s *Scheduler) ReclaimSource(id int64, reason string) int {
	if s == nil || id == 0 {
		return 0
	}
	if s.leases == nil {
		return 0
	}
	set := s.leases.peek(id)
	if set == nil {
		return 0
	}
	set.mu.Lock()
	ls := make([]*Lease, 0, len(set.leases))
	for _, l := range set.leases {
		ls = append(ls, l)
	}
	set.mu.Unlock()
	n := 0
	for _, l := range ls {
		if l.Reclaim(reason) {
			n++
		}
	}
	return n
}

// AcquireLease 按当前准入模式申请一个 A 档名额。必须在拨号之前调用。
func (s *Scheduler) AcquireLease(ctx context.Context, id int64, kind LeaseKind) (*Lease, error) {
	if s == nil || id == 0 || s.cfg.MaxPerSource <= 0 {
		return nil, nil
	}
	if s.deleted(id) {
		// 源在下载途中被删：不再为新请求记账（账面在 Forget 里已清）。
		return nil, nil
	}
	return s.acquireLeaseMode(ctx, id, kind, s.cfg.Admission)
}

func (s *Scheduler) acquireLeaseMode(ctx context.Context, id int64, kind LeaseKind, mode string) (*Lease, error) {
	hard, dispatch := s.cfg.caps()
	set := s.leases.get(id, hard, dispatch)
	l, err := set.acquire(ctx, kind, mode, s.cfg.WaitTimeout)
	if err != nil {
		return nil, err
	}
	set.mu.Lock()
	set.publishLocked()
	set.mu.Unlock()
	return l, nil
}

// LeaseHeld 是某台源当前持有的租约数（含 reserved/reclaiming）。
func (s *Scheduler) LeaseHeld(id int64) int {
	if s == nil || s.cfg.MaxPerSource <= 0 {
		return 0
	}
	hard, dispatch := s.cfg.caps()
	held, _, _, _, _ := s.leases.get(id, hard, dispatch).snapshot()
	return held
}

// gateFor 取某台源的闸门，首次使用时初始化。
func (s *Scheduler) gateFor(id int64) *gate {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	if s.gates == nil {
		s.gates = map[int64]*gate{}
	}
	if g, ok := s.gates[id]; ok {
		return g
	}
	// 从配置上限起步，没样本不做猜测：有证据才退，证明恢复再涨。
	g := newGate(id, s.cfg.MaxPerSource, s.cfg.MaxPerSource)
	s.gates[id] = g
	// 与 Forget 撞车时两种顺序都不会把闸门留在表里，
	// 本次请求仍拿到临时闸门继续跑。
	if s.isGone(id) {
		delete(s.gates, id)
		return newGate(id, s.cfg.MaxPerSource, s.cfg.MaxPerSource)
	}
	return g
}

// InFlight 是某台源此刻占用的名额数，用于选源时负载均衡。
func (s *Scheduler) InFlight(id int64) int {
	return s.LeaseHeld(id)
}

// atCap 判断普通请求此刻是否会被拒绝；observe 阶段不把 80% 当上限。
func (s *Scheduler) atCap(id int64) bool {
	if s == nil || s.cfg.MaxPerSource <= 0 {
		return false
	}
	hard, dispatch := s.cfg.caps()
	limit := dispatch
	if s.cfg.Admission == "observe" {
		limit = hard
	}
	held, _, _, _, _ := s.leases.get(id, hard, dispatch).snapshot()
	return held >= limit
}

// Rewrite 把官方地址改写成某个源的地址。
//
//	docker：换 host，/v2/... 原样保留
//	github：前缀代理，<base>/<完整原始 URL>
func (s *Scheduler) Rewrite(kind string, base *url.URL, c Candidate) (*url.URL, bool) {
	cb, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || cb.Host == "" {
		return nil, false
	}
	if kind == "docker" || kind == "ghcr" {
		u := *base
		u.Scheme = cb.Scheme
		u.Host = cb.Host
		u.Path = base.Path
		// 带 ghcr.io 前缀的源（mirror-lens 同款，如 gp.zzgg.eu.org）回源路径是
		// /v2/ghcr.io/<repo>/...，而 base.Path 是 /v2/<repo>/...（name 已剥掉 ghcr.io）。
		// 把开头的 /v2 换成 /v2/<base.Host> 即得。base.Host 在 ghcr 场景恒为 ghcr.io。
		if kind == "ghcr" && c.GhcrPrefix {
			u.Path = "/v2/" + base.Host + strings.TrimPrefix(base.Path, "/v2")
		}
		u.RawQuery = base.RawQuery
		return &u, true
	}
	u := *cb
	u.Path = strings.TrimRight(cb.Path, "/") + "/" + base.String()
	return &u, true
}

// Hint 是失败回报里的附加信息。
type Hint struct {
	// Code 是上游 HTTP 状态码，0 表示连接失败/超时；只用于限流统计。
	Code int

	// Mode 是耗时口径：single 整次、stream 首字节、chunk 单段，不能混。
	Mode string

	RetryAfter time.Duration
}

// Report 记录一次实际回源结果：成功闭合熔断并加粘性，失败进冷却（逐次翻倍）。
// 带 Retry-After 时取 max(本地退避, Retry-After) 再夹到 MaxCooldown。
func (s *Scheduler) Report(kind string, c Candidate, ok bool, d time.Duration, hints ...Hint) {
	if s == nil || c.ID == 0 {
		return
	}
	k := pkey(kind, c.ID)
	s.mu.Lock()
	if s.isGone(c.ID) {
		// 源已删除：写回去就是复活刚清理的熔断、粘性、事件和指标。
		s.mu.Unlock()
		return
	}
	if ok {
		delete(s.brk, k)
		s.sticky[kind] = stickyPick{cand: c, at: s.now()}
	} else {
		b := s.brk[k]
		if b == nil {
			b = &breaker{}
		}
		b.fails++
		b.probing = false
		shift := b.fails - 1
		if shift > maxBackoffShift {
			shift = maxBackoffShift
		}
		cool := s.cfg.FailCooldown << shift
		if len(hints) > 0 && hints[0].RetryAfter > cool {
			cool = hints[0].RetryAfter
		}
		if cool > s.cfg.MaxCooldown {
			cool = s.cfg.MaxCooldown
		}
		b.until = s.now().Add(cool)
		s.brk[k] = b
	}
	s.mu.Unlock()

	// git 能通说明实现了 smart HTTP，记下来下次不用再试错。
	if ok && kind == "git" {
		_ = s.st.SetGitCap(c.ID, true)
	}
	code, mode := 0, ""
	if len(hints) > 0 {
		code, mode = hints[0].Code, hints[0].Mode
	}
	if mode == "" {
		mode = "other"
	}
	metrics.SourceResult(kind, ok, code, mode, d)
	s.st.RecordSourceEvent(c.ID, kind, ok, d.Milliseconds(), code)
}

// Snapshot 给面板看：每个协议当前的顺序与分数。
type SnapshotRow struct {
	Kind  string      `json:"kind"`
	Items []Candidate `json:"items"`
}

func (s *Scheduler) Snapshot(kinds []string) []SnapshotRow {
	var out []SnapshotRow
	for _, k := range kinds {
		cs := s.Candidates(k)
		if len(cs) == 0 {
			continue
		}
		out = append(out, SnapshotRow{Kind: k, Items: cs})
	}
	return out
}

func pkey(kind string, id int64) string {
	return kind + "|" + strconv.FormatInt(id, 10)
}

// score 把三个指标压到 0..1 再加权。
// 成功率做贝叶斯平滑（先验 50%）；缺速度样本给中性 0.5，给 0 会让新源排不上。
func score(st store.SourceStat, speedCapKBps float64) float64 {
	const prior, priorWeight = 0.5, 2.0
	okRate := (float64(st.OK) + prior*priorWeight) / (float64(st.Samples) + priorWeight)
	lat := 0.5
	if st.LatencyMS > 0 {
		lat = 1 / (1 + float64(st.LatencyMS)/800)
	}
	spd := 0.5
	if st.SpeedKBps > 0 {
		if speedCapKBps <= 0 {
			speedCapKBps = 12000
		}
		spd = math.Min(float64(st.SpeedKBps)/speedCapKBps, 1)
	}
	return 0.5*okRate + 0.25*lat + 0.25*spd
}

// ScoreOf 暴露调度用的评分公式，供面板显示，保证两边是同一份数。
func ScoreOf(st store.SourceStat, speedCapKBps float64) float64 {
	return score(st, speedCapKBps)
}

// DispatchCap 返回单源硬上限与普通派发上限（3.0 固定配额，AIMD 已下线）。
func (s *Scheduler) DispatchCap() (hard, dispatch int) {
	if s == nil {
		return 0, 0
	}
	return s.cfg.caps()
}

// SpeedCapKBps 是当前速度项的饱和点。
func (s *Scheduler) SpeedCapKBps() float64 {
	if s == nil {
		return 12000
	}
	return s.cfg.SpeedCapKBps
}

// InvalidateLists 丢掉候选缓存，改完调度设置后立刻生效。
func (s *Scheduler) InvalidateLists() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lists = map[string]listEntry{}
	s.mu.Unlock()
}
