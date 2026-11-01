// Package scheduler 决定一次回源该走哪个镜像源。
//
// 评分只用探测样本（tier>=2），代理快慢是用户到本机，混进来会失真；
// 只有回源才选源，TTL 重新验证一律回官方源；熔断按 (协议, 源) 记。
package scheduler

import (
	"context"
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

	loadMu sync.Mutex
	gates  map[int64]*gate // 源 id → 并发闸门（容量可动态调整）
	dyn    map[int64]*dynState

	// gone 是已删除源的墓碑，挡住迟到回调把状态重建出来。
	// 用 sync.Map 避开与 Forget（s.mu → loadMu）的反向锁序。
	gone sync.Map // int64 → struct{}
}

func New(st *store.Store, cfg Config) *Scheduler {
	cfg.fill()
	return &Scheduler{
		st:     st,
		cfg:    cfg,
		now:    time.Now,
		lists:  map[string]listEntry{},
		sticky: map[string]stickyPick{},
		brk:    map[string]*breaker{},
		gates:  map[int64]*gate{},
		dyn:    map[int64]*dynState{},
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
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if le, ok := s.lists[kind]; ok && s.now().Sub(le.at) < s.cfg.ListTTL {
		return s.order(kind, le.items)
	}
	stats, err := s.st.SourceCandidates(kind)
	if err != nil {
		return nil
	}
	// 重启后从事件表恢复最近失败的源，接着熔断而不是重新试错。
	if cooling, err := s.st.SourceFailedRecently(kind, s.now().Add(-s.cfg.FailCooldown).Unix()); err == nil {
		for _, st := range stats {
			k := pkey(kind, st.ID)
			if cooling[st.ID] && s.brk[k] == nil {
				s.brk[k] = &breaker{fails: 1, until: s.now().Add(s.cfg.FailCooldown)}
			}
		}
	}
	items := make([]Candidate, 0, len(stats))
	for _, st := range stats {
		items = append(items, Candidate{ID: st.ID, Name: st.Name, BaseURL: st.BaseURL, Score: score(st), Trust: st.Trust, Preferred: st.Preferred})
	}
	s.lists[kind] = listEntry{items: items, at: s.now()}
	return s.order(kind, items)
}

// order 叠加熔断与粘性偏好。
//
// 打开的直接排除（降权仍会排前面，白等一次超时）；半开的排队尾，有健康源
// 时不拿真实请求去赌；冷却逾期一个周期未探，则插队首只试一次。
func (s *Scheduler) order(kind string, in []Candidate) []Candidate {
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
	sort.SliceStable(out, func(i, j int) bool {
		if bi, bj := s.atCap(out[i].ID), s.atCap(out[j].ID); bi != bj {
			return !bi
		}
		// 手动优先的源排同组最前，但放在 atCap 之后，别把满员源硬塞前面。
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
	if len(out) > s.cfg.MaxCandidates {
		out = out[:s.cfg.MaxCandidates]
	}
	return out
}

// Acquire 占一个回源并发名额，返回的释放函数必须调用（可重复）。
// 满员时等待是排队而非故障，不计健康、不熔断。
func (s *Scheduler) Acquire(ctx context.Context, id int64) (func(), error) {
	if s == nil || id == 0 || s.cfg.MaxPerSource <= 0 {
		return func() {}, nil
	}
	if s.deleted(id) {
		// 源在下载途中被删：不建闸门直接放行，在途请求不该失败。
		return func() {}, nil
	}
	release, err := s.gateFor(id).acquire(ctx)
	if err != nil {
		return nil, err
	}
	metrics.SourceInflight(id, 1)
	var once sync.Once
	return func() {
		once.Do(func() {
			release()
			metrics.SourceInflight(id, -1)
		})
	}, nil
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
	g := newGate(s.cfg.MaxPerSource, s.cfg.MaxPerSource)
	s.gates[id] = g
	// 与 Forget 撞车时两种顺序都不会把闸门留在表里，
	// 本次请求仍拿到临时闸门继续跑。
	if s.isGone(id) {
		delete(s.gates, id)
		return newGate(s.cfg.MaxPerSource, s.cfg.MaxPerSource)
	}
	return g
}

// InFlight 是某台源此刻占用的名额数，用于选源时负载均衡。
func (s *Scheduler) InFlight(id int64) int {
	if s == nil || s.cfg.MaxPerSource <= 0 {
		return 0
	}
	g := s.gateFor(id)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight
}

func (s *Scheduler) atCap(id int64) bool {
	if s == nil || s.cfg.MaxPerSource <= 0 {
		return false
	}
	g := s.gateFor(id)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight >= g.limit
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
	if kind == "docker" {
		u := *base
		u.Scheme = cb.Scheme
		u.Host = cb.Host
		u.Path = base.Path
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
func score(st store.SourceStat) float64 {
	const prior, priorWeight = 0.5, 2.0
	okRate := (float64(st.OK) + prior*priorWeight) / (float64(st.Samples) + priorWeight)
	lat := 0.5
	if st.LatencyMS > 0 {
		lat = 1 / (1 + float64(st.LatencyMS)/800)
	}
	spd := 0.5
	if st.SpeedKBps > 0 {
		spd = math.Min(float64(st.SpeedKBps)/1500, 1)
	}
	return 0.5*okRate + 0.25*lat + 0.25*spd
}
