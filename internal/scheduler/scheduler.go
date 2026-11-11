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
	// RegistryPath 是该源对 Registry Host 已验证的路径模式（native | host_prefix）。
	// 仅 registry:<host> 候选有值；一个候选只绑定一条路径，不保留路径回退。
	RegistryPath string `json:"registry_path_mode,omitempty"`
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
	// brev 是每个 (kind,id) 的单调版本号，落库时用于挡住晚到的旧写入。
	// 在 s.mu 里分配，所以顺序就是内存状态的真实变更顺序，与墙上时钟无关。
	brev map[string]int64
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

	// gen 是源的身份代次：删源/改地址时递增。上报在锁内取号，落库前
	// 再校验一次，跨越 Forget 边界的旧写入会被丢弃。
	gen map[int64]uint64

	// pmu 串行化所有持久化写入，并让 Forget 能等在飞写入结束再清库。
	// 只在刷盘时持有，绝不与 s.mu 形成「先 mu 后 pmu」的持锁顺序。
	pmu sync.Mutex
	// persistFn / clearFn 可注入，故障测试用来模拟写库、清库失败。
	persistFn func(store.OutcomeRecord) error
	clearFn   func(int64) error

	// wseq 让「检查积压 → 直接落库」与「入队」互斥：否则 DB 恢复后的
	// 第一条结果可能越过仍然积压的旧事件直接插队，事件表顺序就不再是
	// rev 顺序，按 id 重建连续失败的逻辑会算错。
	wseq sync.Mutex

	// 写库失败后的待重试队列：每个 (kind,id) 一条 FIFO，事件按产生顺序
	// 补写（有上限），熔断状态靠 rev 守卫只前进不后退；跨代次的旧写入丢弃。
	retryMu sync.Mutex
	pending map[string][]pendingWrite
	// pstate 单独保存每个 (kind,id) 尚未确认落盘的最新熔断快照：事件队列
	// 溢出时只丢旧事件，这份快照一定补写（BreakerOnly，不会重复事件）。
	pstate    map[string]pendingWrite
	retryBusy map[string]bool
	retryWait map[string]time.Duration

	// legacyDone 记录已经做过老库事件兜底的协议：窗口函数只跑一次。
	legacyDone map[string]bool

	// rl 是每个协议一把刷新锁：候选列表重建（含两条查询）同一时刻只跑一个，
	// 避免并发刷新重复扫事件表，也避免后到者覆盖先到的列表。
	rl sync.Map // kind → *sync.Mutex
}

// pendingCap 是单个 (kind,id) 待重试队列上限。数据库长时间不可写时丢弃
// 最旧的事件，保证内存有界；熔断状态永远保留最新一份。
const pendingCap = 64

// pendingWrite 是一份等待重新落库的结果，按到达顺序排队。
type pendingWrite struct {
	rec store.OutcomeRecord
	gen uint64
}

func New(st *store.Store, cfg Config) *Scheduler {
	cfg.fill()
	if cfg.Dynamic {
		// 3.0 起容量由 headroom 固定换算，AIMD 不再参与派发。
		log.Printf("sources.dynamic 在 3.0 已失效（容量改由 headroom 决定），本实例按固定配额运行")
		cfg.Dynamic = false
	}
	s := &Scheduler{
		st:     st,
		cfg:    cfg,
		now:    time.Now,
		lists:  map[string]listEntry{},
		sticky: map[string]stickyPick{},
		brk:    map[string]*breaker{},
		brev:   map[string]int64{},
		gates:  map[int64]*gate{},
		dyn:    map[int64]*dynState{},
		leases: newLeaseManager(),
		gen:    map[int64]uint64{},

		pending:    map[string][]pendingWrite{},
		pstate:     map[string]pendingWrite{},
		retryBusy:  map[string]bool{},
		retryWait:  map[string]time.Duration{},
		legacyDone: map[string]bool{},
	}
	if st != nil {
		s.persistFn = st.RecordOutcome
		s.clearFn = st.ClearBreakerStates
	}
	s.restoreBreakers()
	return s
}

// restoreBreakers 把落盘的熔断状态装回内存。
//
// 恢复的是 fails 和绝对截止时间，不是「从现在起再冷却一个周期」：剩余
// 10 秒的冷却重启后还是 10 秒。时钟大幅回拨（截止时间比 now+MaxCooldown
// 还远）说明状态已经不可信，按最大冷却收敛，避免一个坏时间戳把源永久
// 关在黑名单里。
func (s *Scheduler) restoreBreakers() {
	if s == nil || s.st == nil {
		return
	}
	rows, err := s.st.LoadBreakerStates()
	if err != nil {
		log.Printf("调度：恢复熔断状态失败: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range rows {
		k := pkey(b.Kind, b.SourceID)
		// 墓碑也要续上版本号：否则重启后的第一次写入会被更早的高版本挡住。
		if b.Rev > s.brev[k] {
			s.brev[k] = b.Rev
		}
		if b.Fails <= 0 {
			continue
		}
		until := capUntil(time.Unix(b.Until, 0), now, s.cfg.MaxCooldown)
		s.brk[k] = &breaker{fails: b.Fails, until: until}
	}
}

// capUntil 把截止时间压到 now+max 之内。时钟异常可能让旧事件时间落在
// 未来，光限制冷却时长不够，这里限制的是最终时刻。
func capUntil(u, now time.Time, max time.Duration) time.Time {
	if max <= 0 {
		return u
	}
	if lim := now.Add(max); u.After(lim) {
		return lim
	}
	return u
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
	// 代次加一：已经取号但还没落库的上报，落库前会发现代次变了而放弃。
	s.gen[id]++
	for k := range s.brk {
		if strings.HasSuffix(k, suffix) {
			delete(s.brk, k)
			delete(s.brev, k)
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

	// 持久化状态也要清：id 可能被新源复用，或者同一 id 换了地址，
	// 旧熔断不该跟着到下一台服务器上。
	//
	// 先拿 pmu 等在飞写入结束，再删库，顺序反了会被迟到的写入塞回来。
	// 注意这里已经放开 s.mu，不会和上报路径形成 mu→pmu 的反向锁序。
	s.clearBreakerState(id, s.genSnapshot(id), 0)
	// 队列里属于这个源的待重试项也一并丢掉。
	s.retryMu.Lock()
	for k := range s.pending {
		if strings.HasSuffix(k, suffix) {
			delete(s.pending, k)
			delete(s.pstate, k)
			delete(s.retryWait, k)
			delete(s.retryBusy, k)
		}
	}
	s.retryMu.Unlock()
	metrics.ForgetSource(id)
}

// genSnapshot 读当前代次，给清理任务做「还是同一代」的校验。
func (s *Scheduler) genSnapshot(id int64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen[id]
}

// clearBreakerState 删除持久化熔断状态，失败按 200ms/400ms/800ms/1.6s
// 重试 4 次；仍失败就明确记录「已放弃」，而不是静默当作清干净。
//
// 每次删除前校验代次：源被快速删掉又新建时，新源自己的状态不能被这次
// 迟到的清理连带删掉。
func (s *Scheduler) clearBreakerState(id int64, gen uint64, attempt int) {
	if s.st == nil {
		return
	}
	s.pmu.Lock()
	s.mu.Lock()
	same := s.gen[id] == gen
	s.mu.Unlock()
	var err error
	if same {
		if s.clearFn != nil {
			err = s.clearFn(id)
		}
	}
	s.pmu.Unlock()
	if err == nil || !same {
		return
	}
	if attempt >= 4 {
		log.Printf("调度：源 %d 的持久化熔断清理连续失败，已放弃（重启可能恢复旧状态）: %v", id, err)
		return
	}
	d := time.Duration(1<<uint(attempt)) * 200 * time.Millisecond
	log.Printf("调度：清理源 %d 的持久化熔断失败，%v 后重试: %v", id, d, err)
	time.AfterFunc(d, func() { s.clearBreakerState(id, gen, attempt+1) })
}

// Revive 撤销删除墓碑。id 正常不会复用，这里是防御性的：
// 没有它，复用的新源会被当成已删除，报告和闸门全被静默丢弃。
func (s *Scheduler) Revive(id int64) {
	if s == nil || id == 0 {
		return
	}
	s.gone.Delete(id)
	// 新生命周期必须有新代次：Forget 安排出去的延迟清库任务持的是旧代次，
	// 不递增就会误删这个源刚写下的新状态。
	//
	// 整段持 pmu：在飞写入是「pmu 内查代次 + 写库」，不排同一把锁的话，
	// 已经通过代次校验的旧写入会在 gen++ 之后落库，把新生命周期刚清掉的
	// 熔断状态又写回来。id 被 rowid 复用时这是真实路径，不是理论问题。
	s.pmu.Lock()
	suffix := "|" + strconv.FormatInt(id, 10)
	s.mu.Lock()
	s.gen[id]++
	for k := range s.brk {
		if strings.HasSuffix(k, suffix) {
			delete(s.brk, k)
			delete(s.brev, k)
		}
	}
	s.mu.Unlock()
	s.retryMu.Lock()
	for k := range s.pending {
		if strings.HasSuffix(k, suffix) {
			delete(s.pending, k)
			delete(s.pstate, k)
			delete(s.retryWait, k)
			delete(s.retryBusy, k)
		}
	}
	s.retryMu.Unlock()
	// 防御性清库：id 复用时旧生命周期的行必须消失，否则重启后又被加载。
	// 清失败只记日志——下一次成功上报会用品墓碑行覆盖它。
	if s.clearFn != nil {
		if err := s.clearFn(id); err != nil {
			log.Printf("调度：复活源 %d 时清理旧熔断状态失败: %v", id, err)
		}
	}
	s.pmu.Unlock()
}

// isGone 报告该 id 是否已被删除，任何锁状态下都能问。
func (s *Scheduler) isGone(id int64) bool {
	_, ok := s.gone.Load(id)
	return ok
}

// deleted 是 isGone 的别名，给不持有 s.mu 的调用方用。
func (s *Scheduler) deleted(id int64) bool { return s.isGone(id) }

// StickyFor 返回该调度维度当前的粘性源，0 表示没有。
// 只读，不触发排序或加载，给诊断和测试确认「谁被粘住了」。
func (s *Scheduler) StickyFor(kind string) int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sticky[kind].cand.ID
}

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
	if out, ok := s.cached(kind, limit); ok {
		return out
	}
	// 同一个协议的重建串行化：两条查询 + 一次老库兜底只跑一遍，
	// 并发请求不会各自扫一遍事件表，也不会后到覆盖先到。
	lk := s.refreshLock(kind)
	lk.Lock()
	defer lk.Unlock()
	if out, ok := s.cached(kind, limit); ok {
		return out
	}

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
	// 老库兜底只在真正重建列表时做，且每协议一次；失败不置位，下次刷新再试。
	s.mu.Lock()
	needFallback := s.st != nil && !s.legacyDone[kind]
	s.mu.Unlock()
	var runs map[int64]store.FailureRun
	var cerr error
	if needFallback {
		runs, cerr = s.st.SourceFailureRuns(kind, 64)
	}
	mo := s.st.GetSchedOrder()

	s.mu.Lock()
	defer s.mu.Unlock()
	// 老库还没有 breaker_state（升级后的第一次启动）：从事件表兜底。
	//
	// 旧版本没有持久化 fails/until，也没记当时的 Retry-After，所以这里只能
	// 按事件顺序还原「连续失败次数」，再用基础退避公式推算截止时间；这是
	// 近似恢复，不是精确恢复。截止时间仍以最后一条失败事件为基准，不按
	// 「现在 + 一个周期」重算，否则剩余 10 秒的冷却会被拉长成 5 分钟。
	if needFallback && cerr == nil {
		s.legacyDone[kind] = true
		for _, st := range stats {
			k := pkey(kind, st.ID)
			r, bad := runs[st.ID]
			// brev!=0 说明这个键有过持久化状态（哪怕现在是一条 fails=0 的
			// 墓碑）：以数据库为准，不能再用更老的事件把它翻出来。
			if !bad || r.Fails == 0 || s.brk[k] != nil || s.brev[k] != 0 {
				continue
			}
			shift := r.Fails - 1
			if shift > maxBackoffShift {
				shift = maxBackoffShift
			}
			cool := s.cfg.FailCooldown << shift
			if cool > s.cfg.MaxCooldown {
				cool = s.cfg.MaxCooldown
			}
			until := time.Unix(r.LastTS, 0).Add(cool)
			// 旧事件的时间戳可能因为时钟异常落在未来，和持久化恢复一样
			// 加一道绝对上限，避免造出一个远超 MaxCooldown 的冷却。
			until = capUntil(until, s.now(), s.cfg.MaxCooldown)
			s.brk[k] = &breaker{fails: r.Fails, until: until}
		}
	}
	s.manualMode, s.manual = mo.Mode, mo.Orders
	items := make([]Candidate, 0, len(stats))
	for _, st := range stats {
		items = append(items, Candidate{ID: st.ID, Name: st.Name, BaseURL: st.BaseURL, Score: score(st, s.cfg.SpeedCapKBps), Trust: st.Trust, Preferred: st.Preferred, GroupRank: st.GroupRank, GhcrPrefix: st.GhcrPrefix, RangeCap: st.RangeCap, RangeAt: st.RangeAt, SpeedKBps: st.SpeedKBps, RegistryPath: st.RegistryPath})
	}
	s.lists[kind] = listEntry{items: items, at: s.now()}
	return s.orderN(kind, items, limit)
}

// cached 返回仍在 TTL 内的候选列表。
func (s *Scheduler) cached(kind string, limit int) ([]Candidate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	le, ok := s.lists[kind]
	if !ok || s.now().Sub(le.at) >= s.cfg.ListTTL {
		return nil, false
	}
	return s.orderN(kind, le.items, limit), true
}

// refreshLock 返回协议级的重建锁。
func (s *Scheduler) refreshLock(kind string) *sync.Mutex {
	if v, ok := s.rl.Load(kind); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	v, _ := s.rl.LoadOrStore(kind, m)
	return v.(*sync.Mutex)
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
	// registry:<host> 的路径只由候选自己已验证的模式决定，不改道、不回退：
	// native      → /v2/<name>/...
	// host_prefix → /v2/<host>/<name>/... 是独立的一族，不在这里生成。
	if kind == "registry" {
		return nil, false
	}
	if host, ok := strings.CutPrefix(kind, "registry:"); ok {
		if host == "" || !strings.EqualFold(host, base.Host) {
			return nil, false
		}
		u := *base
		u.Scheme, u.Host = cb.Scheme, cb.Host
		switch c.RegistryPath {
		case "native":
			u.Path = base.Path
		case "host_prefix":
			u.Path = "/v2/" + host + strings.TrimPrefix(base.Path, "/v2")
			// 路径已经换过，base 的 RawPath 描述的是旧路径：留着会让 String()
			// 在「旧 RawPath 恰好仍是新 Path 的合法编码」时输出错误转义，必须清空。
			u.RawPath = ""
		default:
			// 空值/未知模式一律拒绝，绝不猜一条路径出来。
			return nil, false
		}
		u.RawQuery = base.RawQuery
		return &u, true
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

// Outcome 是一次回源的调度语义。Miss/Auth 是中性结果：既不是成功，
// 也不是源故障，所以既不建粘性也不进冷却，只留事件备查。
type Outcome int

const (
	// OutcomeOK 拿到了内容。
	OutcomeOK Outcome = iota
	// OutcomeMiss 是 404/410：源答得很正常，只是没有这个资源。
	OutcomeMiss
	// OutcomeAuth 是 401/403：源要求认证，换源不一定能解决，但不代表源坏。
	OutcomeAuth
	// OutcomeFail 是 429/5xx/网络失败：源本身有问题，按故障策略处理。
	OutcomeFail
)

// Report 兼容旧调用：true=成功，false=失败。
func (s *Scheduler) Report(kind string, c Candidate, ok bool, d time.Duration, hints ...Hint) {
	o := OutcomeFail
	if ok {
		o = OutcomeOK
	}
	s.ReportOutcome(kind, c, o, d, hints...)
}

// ReportOutcome 记录一次实际回源结果。
//
//   - OutcomeOK：闭合熔断、刷新粘性、计成功样本；
//   - OutcomeMiss / OutcomeAuth：中性，不动熔断与粘性，事件照记；
//   - OutcomeFail：进冷却（逐次翻倍），带 Retry-After 时取 max(本地退避, Retry-After)。
func (s *Scheduler) ReportOutcome(kind string, c Candidate, o Outcome, d time.Duration, hints ...Hint) {
	if s == nil || c.ID == 0 {
		return
	}
	k := pkey(kind, c.ID)
	code, mode := 0, ""
	if len(hints) > 0 {
		code, mode = hints[0].Code, hints[0].Mode
	}
	if mode == "" {
		mode = "other"
	}
	var now time.Time
	var brk *store.BreakerState
	s.mu.Lock()
	if s.isGone(c.ID) {
		// 源已删除：写回去就是复活刚清理的熔断、粘性、事件和指标。
		s.mu.Unlock()
		return
	}
	now = s.now()
	switch o {
	case OutcomeOK:
		delete(s.brk, k)
		s.sticky[kind] = stickyPick{cand: c, at: now}
		// fails=0 的墓碑行：让并发里晚到的失败按 rev 比不过它。
		brk = &store.BreakerState{SourceID: c.ID, Kind: kind, UpdatedAt: now.UnixNano()}
	case OutcomeMiss, OutcomeAuth:
		// 中性结果：不建粘性、不清熔断、不进冷却，也不写状态行。
		// 已有粘性也不动——它是这台源上一次「真拿到内容」的证明。
	default:
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
		b.until = now.Add(cool)
		s.brk[k] = b
		brk = &store.BreakerState{
			SourceID: c.ID, Kind: kind, Fails: b.fails,
			Until: b.until.Unix(), UpdatedAt: now.UnixNano(),
		}
	}
	// 版本号在锁内递增：内存里谁后改，谁的号就大。中性事件不写状态行，
	// 但也要取号，保证它在重试队列里和失败/成功事件保持同一顺序。
	s.brev[k]++
	rev := s.brev[k]
	if brk != nil {
		brk.Rev = rev
	}
	gen := s.gen[c.ID]
	s.mu.Unlock()

	// git 能通说明实现了 smart HTTP，记下来下次不用再试错。
	if o == OutcomeOK && kind == "git" && s.st != nil {
		_ = s.st.SetGitCap(c.ID, true)
	}
	res := metrics.SourceNeutral
	switch o {
	case OutcomeOK:
		res = metrics.SourceOK
	case OutcomeFail:
		res = metrics.SourceFail
	}
	metrics.SourceResult(kind, res, code, mode, d)
	// 事件与熔断状态一个事务落库：只写进一件，重启后就会按错误的那半恢复。
	rec := store.OutcomeRecord{
		SourceID: c.ID, Kind: kind, OK: o == OutcomeOK,
		Neutral: o == OutcomeMiss || o == OutcomeAuth,
		Code:    code, MS: d.Milliseconds(), Breaker: brk, Rev: rev,
	}
	s.persist(gen, rec)
}

// persist 把结果写进库。写失败时不能回滚内存——可能已经有更高 rev 的
// 后续结果——而是把事件追加到该 key 的 FIFO 队列，同时把熔断快照单独
// 记下来：事件队列有上限，快照没有，保证状态不会被事件洪峰挤掉。
func (s *Scheduler) persist(gen uint64, rec store.OutcomeRecord) {
	if s.persistFn == nil {
		return
	}
	s.wseq.Lock()
	defer s.wseq.Unlock()
	// 该 key 还有没补完的事件：一律排队，不能直写插到旧 rev 前面。
	if s.hasBacklog(pkeyOf(rec)) {
		s.enqueueLocked(gen, rec)
		return
	}
	if s.write(rec, gen) {
		s.dropState(rec)
		return
	}
	s.enqueueLocked(gen, rec)
}

// hasBacklog 报告该 key 是否还有待补的事件或未落盘的熔断快照。
func (s *Scheduler) hasBacklog(k string) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if len(s.pending[k]) > 0 {
		return true
	}
	_, ok := s.pstate[k]
	return ok
}

func pkeyOf(rec store.OutcomeRecord) string { return pkey(rec.Kind, rec.SourceID) }

// dropState 在一条带熔断状态的记录确认落盘后，清掉它的待写快照。
func (s *Scheduler) dropState(rec store.OutcomeRecord) {
	if rec.Breaker == nil {
		return
	}
	k := pkeyOf(rec)
	s.retryMu.Lock()
	if p, ok := s.pstate[k]; ok && p.rec.Rev <= rec.Rev {
		delete(s.pstate, k)
	}
	s.retryMu.Unlock()
}

// write 落盘一条记录。返回 true 表示已处理（写入成功，或代次已过期而
// 主动丢弃）；false 表示失败、需要重试。
//
// 持 pmu 再校验代次：Forget/Revive 都是「gen++ → 放 mu → 拿 pmu → 改库」，
// 所以这里若看到代次已变，说明身份已经换过，这份写入必须放弃。
func (s *Scheduler) write(rec store.OutcomeRecord, gen uint64) bool {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.mu.Lock()
	stale := s.isGone(rec.SourceID) || s.gen[rec.SourceID] != gen
	s.mu.Unlock()
	if stale {
		return true
	}
	if err := s.persistFn(rec); err != nil {
		log.Printf("调度：写回源结果失败（源 %d/%s rev=%d）: %v", rec.SourceID, rec.Kind, rec.Rev, err)
		return false
	}
	return true
}

func (s *Scheduler) enqueue(gen uint64, rec store.OutcomeRecord) {
	s.wseq.Lock()
	defer s.wseq.Unlock()
	s.enqueueLocked(gen, rec)
}

// enqueueLocked 要求持 wseq。
func (s *Scheduler) enqueueLocked(gen uint64, rec store.OutcomeRecord) {
	k := pkeyOf(rec)
	s.retryMu.Lock()
	// 熔断快照独立保留：即使随后事件队列溢出丢掉这条事件，状态也不会丢。
	if rec.Breaker != nil {
		if p, ok := s.pstate[k]; !ok || p.rec.Rev < rec.Rev {
			s.pstate[k] = pendingWrite{rec: rec, gen: gen}
		}
	}
	// 不按到达顺序判断新旧：写失败与入队之间没有顺序保证，rev1 可能晚于
	// rev2 到达。按 rev 插入，保证事件表里的顺序和状态变更顺序一致。
	q := append(s.pending[k], pendingWrite{rec: rec, gen: gen})
	sort.SliceStable(q, func(i, j int) bool { return q[i].rec.Rev < q[j].rec.Rev })
	if len(q) > pendingCap {
		drop := len(q) - pendingCap
		log.Printf("调度：%s 待重试事件队列已满，丢弃 %d 条较旧事件（熔断快照另存）", k, drop)
		q = q[drop:]
	}
	s.pending[k] = q
	s.retryMu.Unlock()
	s.schedulePending(k, 200*time.Millisecond)
}

func (s *Scheduler) schedulePending(k string, d time.Duration) {
	s.retryMu.Lock()
	_, hasState := s.pstate[k]
	if len(s.pending[k]) == 0 && !hasState {
		s.retryBusy[k] = false
		s.retryMu.Unlock()
		return
	}
	if s.retryBusy[k] {
		s.retryMu.Unlock()
		return
	}
	s.retryBusy[k] = true
	if d <= 0 {
		d = time.Millisecond
	}
	s.retryWait[k] = d
	s.retryMu.Unlock()
	time.AfterFunc(d, func() { s.flushPending(k) })
}

func (s *Scheduler) flushPending(k string) {
	s.retryMu.Lock()
	var p pendingWrite
	stateOnly := false
	if q := s.pending[k]; len(q) > 0 {
		p = q[0]
	} else if v, ok := s.pstate[k]; ok {
		// 事件都补完了，但这条状态还没确认落盘（可能事件已被溢出丢弃）：
		// 只写状态，不重复插事件。
		p, stateOnly = v, true
	} else {
		s.retryBusy[k] = false
		delete(s.retryWait, k)
		s.retryMu.Unlock()
		return
	}
	s.retryMu.Unlock()

	rec := p.rec
	if stateOnly {
		rec.BreakerOnly = true
	}
	ok := s.write(rec, p.gen)

	s.retryMu.Lock()
	if ok {
		if !stateOnly {
			// 只有写成功才出队；失败时保留队首，退避后重写同一条。
			if q := s.pending[k]; len(q) > 0 && q[0].rec.Rev == p.rec.Rev && q[0].gen == p.gen {
				s.pending[k] = q[1:]
			}
		}
		if p.rec.Breaker != nil {
			if v, still := s.pstate[k]; still && v.rec.Rev <= p.rec.Rev {
				delete(s.pstate, k)
			}
		}
	}
	more := len(s.pending[k]) > 0
	if _, still := s.pstate[k]; still {
		more = true
	}
	d := s.retryWait[k]
	if !more {
		delete(s.pending, k)
		delete(s.retryWait, k)
	}
	s.retryBusy[k] = false
	s.retryMu.Unlock()

	if ok {
		// 还有积压：立刻取下一条，不要等下一次上报来唤醒。
		if more {
			s.schedulePending(k, 5*time.Millisecond)
		}
		return
	}
	d *= 2
	if d < 200*time.Millisecond {
		d = 200 * time.Millisecond
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	log.Printf("调度：重试写回仍失败（源 %d/%s），%v 后再试", p.rec.SourceID, p.rec.Kind, d)
	s.schedulePending(k, d)
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
