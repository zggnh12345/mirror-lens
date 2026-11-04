package gateway

import (
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// 活跃任务表：面板要能看见「正在传什么、多快、谁在供」。
// 只做只读展示，不参与调度决策；采样全部在内存里，重启即清空。
// 速度用最近 speedWindow 的滑动窗口算，避免数字在 0 和峰值之间乱跳。

const (
	speedWindow   = 6 * time.Second // 速度窗口；再长就对「刚刚变快」不敏感
	sampleMinGap  = 250 * time.Millisecond
	recentKeep    = 8
	throughWindow = 60 // 整机吞吐窗口（秒）
	// liveMinBytes 以下的小文件不上面板：几百毫秒就传完，闪一下只剩干扰。
	liveMinBytes = 4 << 20
)

// SourceShare 是一台源在这次传输里实际供了多少字节。
type SourceShare struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	OK    bool   `json:"ok"`
}

// LiveTask 是面板上的一行。
type LiveTask struct {
	ID      int64         `json:"id"`
	Kind    string        `json:"kind"`
	Target  string        `json:"target"`
	Mode    string        `json:"mode"` // stream / parallel
	Done    int64         `json:"done"`
	Total   int64         `json:"total"` // -1 = 上游没给长度
	BPS     int64         `json:"bps"`
	Elapsed float64       `json:"elapsed_s"`
	Sources []SourceShare `json:"sources,omitempty"`
	Par     *ParInfo      `json:"par,omitempty"` // 仅分片并行
}

// ParInfo 说清这次并行是怎么切的：几段、几台源、几个 worker。
// 段数和并发数只有并行引擎自己知道，不靠源数量推。
type ParInfo struct {
	Segments int `json:"segments"`
	Sources  int `json:"sources"`
	Workers  int `json:"workers"`
}

// RecentTask 是刚结束的一条；不落库，只留最近几条。
type RecentTask struct {
	ID      int64   `json:"id"`
	Kind    string  `json:"kind"`
	Target  string  `json:"target"`
	Mode    string  `json:"mode"`
	Total   int64   `json:"total"`
	OK      bool    `json:"ok"`
	DurS    float64 `json:"dur_s"`
	EndedAt int64   `json:"ended_at"`
}

// ActiveSnapshot 是 /api/accel/active 的响应。
type ActiveSnapshot struct {
	// Now 是服务器时间（Unix 秒）：面板的「刚刚完成」要跟它比，
	// 不能拿浏览器时钟比，两边差几分钟就会把刚跑完的任务当成旧的。
	Now         int64        `json:"now"`
	Active      []LiveTask   `json:"active"`
	Recent      []RecentTask `json:"recent"`
	WindowBytes int64        `json:"window_bytes"`
	WindowBPS   int64        `json:"window_bps"`
}

type sample struct {
	at time.Time
	n  int64
}

type liveTask struct {
	id      int64
	kind    string
	target  string
	mode    string
	total   int64
	reg     *taskReg
	started time.Time
	done    atomic.Int64
	parSeg  int // 分片并行的段数；0 表示不是并行
	parWrk  int

	mu      sync.Mutex
	names   []string
	shares  []*atomic.Int64
	oks     []*atomic.Bool
	samples []sample
}

type bucket struct {
	sec   int64
	bytes int64
}

type taskReg struct {
	mu     sync.Mutex
	seq    int64
	active map[int64]*liveTask
	recent []RecentTask
	bucket [throughWindow]bucket
}

func newTaskReg() *taskReg {
	return &taskReg{active: make(map[int64]*liveTask, 8)}
}

// start 登记一次传输；total < 0 表示上游没给长度。
func (r *taskReg) start(kind, target, mode string, total int64, sources []string) *liveTask {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.seq++
	t := &liveTask{
		id:      r.seq,
		kind:    kind,
		target:  target,
		mode:    mode,
		total:   total,
		started: time.Now(),
		reg:     r,
		names:   sources,
		shares:  make([]*atomic.Int64, len(sources)),
		oks:     make([]*atomic.Bool, len(sources)),
	}
	for i := range sources {
		t.shares[i] = &atomic.Int64{}
		t.oks[i] = &atomic.Bool{}
	}
	r.active[t.id] = t
	r.mu.Unlock()
	return t
}

// add 记客户端实际收到的字节，同时进整机吞吐窗口。
func (t *liveTask) add(n int64) {
	if t == nil || n <= 0 {
		return
	}
	cur := t.done.Add(n)
	// 单源传输没有按源分账的钩子，把字节记到唯一那台源上；
	// 否则 shares[0] 恒为 0，面板按 bytes>0 过滤后就只剩「单源」两个字。
	if len(t.shares) == 1 {
		t.shares[0].Add(n)
	}
	if t.reg != nil {
		t.reg.addThrough(n)
	}
	t.sample(cur)
}

// addOnly 只记任务自己的进度与速度，不进整机吞吐窗口。
// 回源和随后下发的是同一批字节，两边都计数会把「近 1 分钟」撑成两倍。
func (t *liveTask) addOnly(n int64) {
	if t == nil || n <= 0 {
		return
	}
	t.sample(t.done.Add(n))
}

func (t *liveTask) sample(cur int64) {
	t.mu.Lock()
	last := len(t.samples) - 1
	if last < 0 || time.Since(t.samples[last].at) >= sampleMinGap {
		t.samples = append(t.samples, sample{at: time.Now(), n: cur})
		if len(t.samples) > 64 { // 6 秒窗口内最多 24 个，留点余量
			t.samples = t.samples[len(t.samples)-64:]
		}
	}
	t.mu.Unlock()
}

// addSource 记某台源供出的字节（只用于分片并行）。
func (t *liveTask) addSource(i int, n int64, ok bool) {
	if t == nil || i < 0 || i >= len(t.shares) {
		return
	}
	if n > 0 {
		t.shares[i].Add(n)
	}
	if ok {
		t.oks[i].Store(true)
	}
}

// setPar 记录分片并行的段数与并发数（并行引擎起步后调用一次）。
func (t *liveTask) setPar(seg, workers int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.parSeg, t.parWrk = seg, workers
	t.mu.Unlock()
}

// finish 把任务移出活跃表，进最近完成列表。
func (t *liveTask) finish(ok bool) {
	if t == nil {
		return
	}
	r := t.reg
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.active, t.id)
	rec := RecentTask{
		ID:      t.id,
		Kind:    t.kind,
		Target:  t.target,
		Mode:    t.mode,
		Total:   t.done.Load(),
		OK:      ok,
		DurS:    time.Since(t.started).Seconds(),
		EndedAt: time.Now().Unix(),
	}
	r.recent = append([]RecentTask{rec}, r.recent...)
	if len(r.recent) > recentKeep {
		r.recent = r.recent[:recentKeep]
	}
	r.mu.Unlock()
}

func (t *liveTask) bps() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.samples) < 2 {
		return 0
	}
	cut := time.Now().Add(-speedWindow)
	i := 0
	for i < len(t.samples)-1 && t.samples[i].at.Before(cut) {
		i++
	}
	first, last := t.samples[i], t.samples[len(t.samples)-1]
	d := last.at.Sub(first.at).Seconds()
	if d <= 0.05 {
		return 0
	}
	v := int64(float64(last.n-first.n) / d)
	if v < 0 {
		return 0
	}
	return v
}

func (t *liveTask) snapshot() LiveTask {
	lt := LiveTask{
		ID:      t.id,
		Kind:    t.kind,
		Target:  t.target,
		Mode:    t.mode,
		Done:    t.done.Load(),
		Total:   t.total,
		BPS:     t.bps(),
		Elapsed: time.Since(t.started).Seconds(),
	}
	t.mu.Lock()
	seg, wrk := t.parSeg, t.parWrk
	t.mu.Unlock()
	if seg > 0 {
		lt.Par = &ParInfo{Segments: seg, Sources: len(t.names), Workers: wrk}
	}
	if len(t.names) > 0 {
		lt.Sources = make([]SourceShare, 0, len(t.names))
		for i, n := range t.names {
			lt.Sources = append(lt.Sources, SourceShare{
				Name:  n,
				Bytes: t.shares[i].Load(),
				OK:    t.oks[i].Load(),
			})
		}
	}
	return lt
}

// addThrough 把字节记进整机 60 秒吞吐窗口。
func (r *taskReg) addThrough(n int64) {
	if r == nil || n <= 0 {
		return
	}
	sec := time.Now().Unix()
	idx := int(sec % throughWindow)
	r.mu.Lock()
	if r.bucket[idx].sec != sec {
		r.bucket[idx] = bucket{sec: sec}
	}
	r.bucket[idx].bytes += n
	r.mu.Unlock()
}

func (r *taskReg) through() int64 {
	if r == nil {
		return 0
	}
	now := time.Now().Unix()
	var sum int64
	r.mu.Lock()
	for i := range r.bucket {
		if b := r.bucket[i]; b.sec > now-throughWindow {
			sum += b.bytes
		}
	}
	r.mu.Unlock()
	return sum
}

// snapshot 汇总面板要的数据。活跃任务按开始时间排序，最老的在前。
func (r *taskReg) snapshot() ActiveSnapshot {
	out := ActiveSnapshot{Active: []LiveTask{}, Recent: []RecentTask{}}
	if r == nil {
		return out
	}
	r.mu.Lock()
	ts := make([]*liveTask, 0, len(r.active))
	for _, t := range r.active {
		ts = append(ts, t)
	}
	out.Recent = append(out.Recent, r.recent...)
	r.mu.Unlock()

	sort.Slice(ts, func(i, j int) bool { return ts[i].id < ts[j].id })
	for _, t := range ts {
		out.Active = append(out.Active, t.snapshot())
	}
	out.Now = time.Now().Unix()
	out.WindowBytes = r.through()
	// 「总计」应是当前速率：各活跃任务 6 秒滑动窗口速度之和。用 60 秒平均
	// 会严重滞后，和每行任务显示的速度（同窗口）永远对不上。
	var sum int64
	for _, t := range ts {
		sum += t.bps()
	}
	out.WindowBPS = sum
	return out
}

// taskWriter 只统计真正写给客户端的字节；写失败的那部分不算。
type taskWriter struct {
	http.ResponseWriter
	t *liveTask
}

func (w *taskWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.t.add(int64(n))
	}
	return n, err
}

// taskOnlyWriter 给「回源落盘」这一段计数。回源是真正的上游网络流量，
// 所以计入整机吞吐；随后从磁盘下发的那段不再计，免得同一批字节算两次。
type taskOnlyWriter struct {
	w io.Writer
	t *liveTask
}

func (w *taskOnlyWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		w.t.add(int64(n))
	}
	return n, err
}

func (w *taskWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
