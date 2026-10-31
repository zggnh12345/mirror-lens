// Package scheduler 决定一次回源该走哪个镜像源。
//
// 三条硬规则：
//
//  1. 评分只用探测样本（checks.tier>=2）。代理请求的快慢是「用户到本机」，
//     和「本机到源站」不是一回事，混在一起会让评分失真。
//  2. 只有回源才选源。缓存命中不碰网络；TTL 到期做重新验证时一律回官方源，
//     避免镜像站把旧内容一直当成「没变」喂回来。
//  3. 熔断按 (协议, 源) 记。前缀代理做不了 git，不代表它 raw 也不行。
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
	// Dynamic 打开后，每台源的并发上限在 1..MaxPerSource 之间按实测吞吐自动
	// 调整（AIMD）。关掉就是固定的 MaxPerSource。
	Dynamic bool
	// MaxPerSource 是单台源同时进行的回源请求上限，0 取默认 4，负数表示不限。
	// 只约束「会占住连接」的请求（预检、分片），不约束长连接流式转发——
	// 那是用户自己的下载，卡住等于拒绝服务。
	MaxPerSource int
	// MaxCooldown 是单次冷却的上限。本地退避和 Retry-After 都要夹在它以内：
	// 一个写着 Retry-After: 86400 的响应不该把源关一天。
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
	// Probe 表示这是半开状态下的试探请求，本次结果决定它回到可用还是继续熔断。
	Probe bool `json:"probe,omitempty"`
	// Trust 是内容校验的可信度（-1 不符 / 0 未验证 / 1 仅可用 / 2 内容一致 / 3 长期一致）。
	// 只用于同分时的排序和展示，不参与评分。
	Trust int `json:"trust"`
}

type stickyPick struct {
	cand Candidate
	at   time.Time
}

// breaker 是单个 (协议, 源) 的熔断状态。
//
//	fails=0            闭合，正常参与排序
//	now < until        打开，直接跳过
//	now >= until       半开，放一个探测请求过去；成功即闭合，失败则冷却翻倍
type breaker struct {
	fails   int
	until   time.Time
	probing bool
	probeAt time.Time
}

const (
	// 半开探测在飞超过这么久还没回报，视为卡死，允许再放一个。
	probeStale = 30 * time.Second
	// 冷却最长翻到 8 倍（默认 5m → 40m），避免一个永久坏源每分钟被戳一次。
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
	// 重启后从事件表把「最近一次就是失败」的源捡回来，接着熔断而不是重新试错。
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
		items = append(items, Candidate{ID: st.ID, Name: st.Name, BaseURL: st.BaseURL, Score: score(st), Trust: st.Trust})
	}
	s.lists[kind] = listEntry{items: items, at: s.now()}
	return s.order(kind, items)
}

// order 叠加熔断与粘性偏好。
//
// 打开的源直接排除，而不是降权：降权后它仍可能排在前面，白白再等一次超时。
// 半开的源排在队尾——有健康源时不必拿真实请求去赌，等健康源都失败时它自然被轮到。
// 若冷却已经过期整整一个周期还没被探过，就给它一次队首机会，避免永久坏源被
// 永久排除在外、恢复后也没人知道。
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
				c.Score = 2 // 逾期未探：插到队首，只试这一次
			}
		}
		if sp, ok := s.sticky[kind]; ok && sp.cand.ID == c.ID && now.Sub(sp.at) < s.cfg.StickyTTL {
			c.Score += 0.5 // 刚成功过的源优先复用，省一次试错
		}
		out = append(out, c)
	}
	// 分数不变，只在分数相同时让负载低的源先上；已经占满名额的源整体后移，
	// 免得「最快的那台」被所有任务一起压垮。
	sort.SliceStable(out, func(i, j int) bool {
		if bi, bj := s.atCap(out[i].ID), s.atCap(out[j].ID); bi != bj {
			return !bi
		}
		if out[i].Score == out[j].Score {
			// 同分先看内容可信度：速度一样时，宁可走那台被反复校验过的。
			// 分数本身不动——评分只看可用率/延迟/速度，可信度是另一条轴。
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

// Acquire 为一次回源请求占一个并发名额，返回的释放函数必须调用（可重复调用）。
// 名额满时在这里等，等的是「轮到自己」，不是源站故障——不计健康、不熔断。
func (s *Scheduler) Acquire(ctx context.Context, id int64) (func(), error) {
	if s == nil || id == 0 || s.cfg.MaxPerSource <= 0 {
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

// gateFor 取某台源的闸门，第一次用时按动态/固定策略初始化。
func (s *Scheduler) gateFor(id int64) *gate {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	if s.gates == nil {
		s.gates = map[int64]*gate{}
	}
	if g, ok := s.gates[id]; ok {
		return g
	}
	// 从配置的上限起步，和不开动态时完全一致：没有样本之前不做任何猜测，
	// 有证据（吞吐掉、429）才往下退，退了之后能证明恢复得动再涨回去。
	g := newGate(s.cfg.MaxPerSource, s.cfg.MaxPerSource)
	s.gates[id] = g
	return g
}

// InFlight 是某台源此刻占用的名额数，用来做选源时的负载均衡。
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
	// Code 是这次上报对应的上游 HTTP 状态码；0 表示没有（连接失败、超时）。
	// 只用于统计限流比例，不参与退避计算。
	Code int

	// Mode 说明这次上报的耗时是什么：single 整次回源、stream 首字节、chunk 单段。
	// 三种耗时的含义完全不同，混在一个直方图里就没法看了。
	Mode string

	RetryAfter time.Duration
}

// Report 记录一次实际回源结果：成功闭合熔断并加粘性，失败进冷却（逐次翻倍）。
//
// 带 Retry-After 的失败取 max(本地退避, Retry-After)，再夹到 MaxCooldown：
// 源明确说等 2 分钟时，5 分钟的默认冷却是在浪费一次本来能用的回源；反过来
// 说等一天时，也不能真的等一天。
func (s *Scheduler) Report(kind string, c Candidate, ok bool, d time.Duration, hints ...Hint) {
	if s == nil || c.ID == 0 {
		return
	}
	k := pkey(kind, c.ID)
	s.mu.Lock()
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

	// git 能通说明这个源真的实现了 smart HTTP，记下来，下次不用再试错。
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

// score 把三个指标压到同一个 0..1 区间再加权。
//
// 成功率做贝叶斯平滑：1 次成功不该等于 100%，先按 50% 起算，样本多了才说话。
// 没有速度样本时给中性 0.5，而不是 0——缺样本不代表慢，给 0 会让新源永远排不上。
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
