// Package metrics 导出 Prometheus 文本格式指标。
//
// 手写文本格式不引 client_golang：指标固定、标签全是封闭集合
// （场景、状态码、源类型、模式），类型上就塞不进 URL / IP / 文件名。
//
// 全局单注册表，未启用时记录函数都是空操作。
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const prefix = "mirrorlens_"

// 耗时桶（秒）：命中几十毫秒、回源几十秒，跨三个数量级。
var durBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// 吞吐量桶（字节/秒）：64 KiB/s ~ 50 MiB/s。
var tpBuckets = []float64{65536, 262144, 1048576, 4194304, 10485760, 52428800}

type httpKey struct {
	scene  string
	method string
	code   int
}

type cacheKey struct {
	scene  string
	result string
}

type srcKey struct {
	kind   string
	result string
}

type srcDurKey struct {
	kind string
	mode string
}

type hist struct {
	counts []uint64
	sum    float64
	n      uint64
}

func newHist(buckets []float64) *hist {
	return &hist{counts: make([]uint64, len(buckets)+1)} // 末格是 +Inf
}

func (h *hist) observe(v float64, buckets []float64) {
	i := sort.SearchFloat64s(buckets, v)
	if i >= len(buckets) {
		i = len(buckets) // +Inf
	}
	h.counts[i]++
	h.sum += v
	h.n++
}

// Registry 是所有计数器的容器。
type Registry struct {
	mu sync.Mutex

	httpReq   map[httpKey]uint64
	httpDur   map[string]*hist
	httpBytes map[string]uint64

	cache map[cacheKey]uint64
	tp    map[string]*hist

	srcReq   map[srcKey]uint64
	srcResp  map[string]uint64 // 收到真实响应的次数（429 率分母）
	src429   map[string]uint64
	srcMiss  map[srcMissKey]uint64
	srcDur   map[srcDurKey]*hist
	srcTTFB  map[srcDurKey]*hist
	inflight map[int64]int
	srclimit map[int64]int
	limited  map[string]uint64

	// 快照 gauge 由 janitor 刷新：抓取时重扫整盘，百万文件会卡死。
	haveCache   bool
	cacheBytes  uint64
	cacheBlobs  uint64
	cacheFree   uint64
	haveSources bool
	sources     map[SourceStat]uint64

	// 命中率滚动窗口：janitor 用与上轮的差值算本轮命中率，抓取只读 gauge。
	hitHits   map[string]*atomic.Uint64
	hitTotal  map[string]*atomic.Uint64
	haveRatio bool
	hitRatio  map[string]float64
	lastHits  map[string]uint64
	lastTotal map[string]uint64

	// 热度：累计请求数，janitor 取差值得到本轮请求量。
	sceneTotal map[string]*atomic.Uint64
	haveHot    bool
	sceneDelta map[string]uint64
	lastScene  map[string]uint64
}

// New 返回空注册表。
func New() *Registry {
	return &Registry{
		httpReq:    map[httpKey]uint64{},
		httpDur:    map[string]*hist{},
		httpBytes:  map[string]uint64{},
		cache:      map[cacheKey]uint64{},
		tp:         map[string]*hist{},
		srcReq:     map[srcKey]uint64{},
		srcResp:    map[string]uint64{},
		src429:     map[string]uint64{},
		srcMiss:    map[srcMissKey]uint64{},
		srcDur:     map[srcDurKey]*hist{},
		srcTTFB:    map[srcDurKey]*hist{},
		inflight:   map[int64]int{},
		srclimit:   map[int64]int{},
		limited:    map[string]uint64{},
		sources:    map[SourceStat]uint64{},
		hitHits:    map[string]*atomic.Uint64{},
		hitTotal:   map[string]*atomic.Uint64{},
		hitRatio:   map[string]float64{},
		lastHits:   map[string]uint64{},
		lastTotal:  map[string]uint64{},
		sceneTotal: map[string]*atomic.Uint64{},
		sceneDelta: map[string]uint64{},
		lastScene:  map[string]uint64{},
	}
}

var current atomic.Pointer[Registry]

// Enable 建注册表并设为全局。
func Enable() *Registry {
	r := New()
	current.Store(r)
	return r
}

// Disable 关闭记录（测试用）。
func Disable() { current.Store(nil) }

// Get 返回当前注册表，没启用时是 nil。
func Get() *Registry { return current.Load() }

// ---------- 记录 ----------

// HTTP 记一次请求。scene 是封闭集合，不是 URL。
func HTTP(scene, method string, code int, d time.Duration, bytes int64, cache string) {
	r := current.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.httpReq[httpKey{scene, method, code}]++
	h := r.httpDur[scene]
	if h == nil {
		h = newHist(durBuckets)
		r.httpDur[scene] = h
	}
	h.observe(d.Seconds(), durBuckets)
	if bytes > 0 {
		r.httpBytes[scene] += uint64(bytes)
	}
	if cache != "" {
		r.cache[cacheKey{scene, cache}]++
		// hit 才算命中，miss/pass 只进分母。
		tot := r.hitTotal[scene]
		if tot == nil {
			tot = &atomic.Uint64{}
			r.hitTotal[scene] = tot
		}
		tot.Add(1)
		st := r.sceneTotal[scene]
		if st == nil {
			st = &atomic.Uint64{}
			r.sceneTotal[scene] = st
		}
		st.Add(1)
		if cache == "hit" {
			h := r.hitHits[scene]
			if h == nil {
				h = &atomic.Uint64{}
				r.hitHits[scene] = h
			}
			h.Add(1)
		}
	}
	// 吞吐只量回源：命中缓存时那是磁盘速度；
	// 不足 64 KiB 或 0.5s 内的样本抖动太大，跳过。
	if cache == "miss" && bytes >= 65536 && d >= 500*time.Millisecond {
		t := r.tp[scene]
		if t == nil {
			t = newHist(tpBuckets)
			r.tp[scene] = t
		}
		t.observe(float64(bytes)/d.Seconds(), tpBuckets)
	}
}

// SourceResult 记一次实际回源结果。
func SourceResult(kind string, ok bool, code int, mode string, d time.Duration) {
	r := current.Load()
	if r == nil {
		return
	}
	res := "fail"
	if ok {
		res = "ok"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srcReq[srcKey{kind, res}]++
	// 只有收到响应才进 429 率分母：连不上/超时没有状态码，
	// 算进分母只会把比率稀释成一个好看的数字。
	if code > 0 {
		r.srcResp[kind]++
	}
	if code == http.StatusTooManyRequests {
		r.src429[kind]++
	}
	if d > 0 {
		k := srcDurKey{kind, mode}
		h := r.srcDur[k]
		if h == nil {
			h = newHist(durBuckets)
			r.srcDur[k] = h
		}
		h.observe(d.Seconds(), durBuckets)
	}
}

// srcMissKey 的 code 只有 404/410，由 SourceMiss 把关，保证标签封闭。
type srcMissKey struct {
	kind string
	code int
}

// SourceMiss 记一次源上没有该资源。镜像少一个文件不代表源站坏了，
// 故不进健康分、不进避退，单独计数。
func SourceMiss(kind string, code int) {
	if code != http.StatusNotFound && code != http.StatusGone {
		return
	}
	r := current.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.srcMiss[srcMissKey{kind, code}]++
	r.mu.Unlock()
}

// SourceTTFB 记首字节耗时，与 SourceResult 的完整耗时分成两个指标：
// stream 请求 TTFB 80ms、传完 40s，混在一个直方图里两个数都读不出。
func SourceTTFB(kind, mode string, d time.Duration) {
	r := current.Load()
	if r == nil || d <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := srcDurKey{kind, mode}
	h := r.srcTTFB[k]
	if h == nil {
		h = newHist(durBuckets)
		r.srcTTFB[k] = h
	}
	h.observe(d.Seconds(), durBuckets)
}

// SourceInflight 调整某台源当前的并发占用（+1 进入，-1 退出）。
func SourceInflight(id int64, delta int) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.inflight[id] + delta
	if v <= 0 {
		delete(r.inflight, id)
		return
	}
	r.inflight[id] = v
}

// ForgetSource 清掉某源的逐源指标：源删了序列还在，
// id 复用后新源会背上旧数字。
func ForgetSource(id int64) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, id)
	delete(r.srclimit, id)
}

// SourceLimit 记录某源当前并发上限（动态调度会改）。
func SourceLimit(id int64, n int) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srclimit[id] = n
}

// RateLimited 记一次入口限流拒绝。
func RateLimited(layer string) {
	r := current.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limited[layer]++
}

// SourceStat 是源站数标签。status 为封闭集合
// （pending|usable|degraded|dead|blocked|archived），kind 是协议名。
type SourceStat struct {
	Kind   string
	Status string
}

// SetCache 刷新缓存盘快照（字节/条目/剩余），由 janitor 每轮调用。
func SetCache(bytes, blobs, free uint64) {
	r := current.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.haveCache = true
	r.cacheBytes = bytes
	r.cacheBlobs = blobs
	r.cacheFree = free
	r.mu.Unlock()
}

// SetSources 刷新源站数快照。
func SetSources(m map[SourceStat]uint64) {
	r := current.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.haveSources = true
	r.sources = m
	r.mu.Unlock()
}

// SnapshotHitRatio 由 janitor 周期调用，用与上轮的差值算本轮命中率：
// 累计比会让启动时的冷启动未命中永久拉低曲线。
func SnapshotHitRatio() {
	r := current.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for scene, tot := range r.hitTotal {
		now := tot.Load()
		h := r.hitHits[scene]
		nh := uint64(0)
		if h != nil {
			nh = h.Load()
		}
		lastT := r.lastTotal[scene]
		lastH := r.lastHits[scene]
		dt := now - lastT
		if dt > 0 {
			r.hitRatio[scene] = float64(nh-lastH) / float64(dt)
		} else {
			r.hitRatio[scene] = 0 // 本周期无流量，归零避免 stale
		}
		r.lastTotal[scene] = now
		r.lastHits[scene] = nh
	}
	r.haveRatio = true
}

// SnapshotSceneHotness 由 janitor 周期取累计请求数差值，写入 sceneDelta。
func SnapshotSceneHotness() {
	r := current.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for scene, c := range r.sceneTotal {
		now := c.Load()
		r.sceneDelta[scene] = now - r.lastScene[scene] // 零流量时差值自然为 0
		r.lastScene[scene] = now
	}
	r.haveHot = true
}

// ---------- HTTP ----------

// Scene 把路径压成有限场景：绝不把 URL 当标签。
func Scene(path string) string {
	switch {
	case strings.HasPrefix(path, "/v2/"):
		return "docker"
	case strings.HasPrefix(path, "/gh/"):
		return "github-release"
	case strings.HasPrefix(path, "/raw/"):
		return "github-raw"
	case strings.HasPrefix(path, "/git/"):
		return "git"
	case strings.HasPrefix(path, "/http/"):
		return "http"
	case strings.HasPrefix(path, "/api/"):
		return "api"
	case path == "/healthz":
		return "healthz"
	case path == "/metrics":
		return "metrics"
	case path == "/" || path == "/login" || path == "/setup" || strings.HasSuffix(path, ".css") ||
		strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".svg"):
		return "panel"
	}
	return "other"
}

func method(m string) string {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS":
		return m
	}
	return "other"
}

// cacheResult 把 X-ML-Cache 归一成三值；无此头返回空。
func cacheResult(v string) string {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "HIT", "HIT-REVAL":
		return "hit"
	case "MISS":
		return "miss"
	case "PASS":
		return "pass"
	}
	return ""
}

type recorder struct {
	http.ResponseWriter
	code  int
	bytes int64
}

func (w *recorder) WriteHeader(c int) {
	if w.code == 0 {
		w.code = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *recorder) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Flush 透传，包一层不能吃掉 Flusher。
func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 透传底层 writer，供 ResponseController 设超时等。
func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Middleware 记录每个请求；放最外层，限流的 429 也要统计。
func Middleware(next http.Handler) http.Handler {
	if current.Load() == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scene := Scene(r.URL.Path)
		if scene == "metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.code == 0 {
			rec.code = http.StatusOK
		}
		HTTP(scene, method(r.Method), rec.code, time.Since(start), rec.bytes,
			cacheResult(rec.Header().Get("X-ML-Cache")))
	})
}

// Handler 输出 Prometheus 文本格式。
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(r.render())
	})
}

func (r *Registry) render() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder
	if len(r.httpReq) > 0 {
		b.WriteString("# HELP " + prefix + "http_requests_total 按场景、方法、状态码统计的请求数\n")
		b.WriteString("# TYPE " + prefix + "http_requests_total counter\n")
		for _, k := range sortedHTTP(r.httpReq) {
			fmt.Fprintf(&b, "%shttp_requests_total{scene=%q,method=%q,code=%q} %d\n",
				prefix, k.scene, k.method, strconv.Itoa(k.code), r.httpReq[k])
		}
	}
	writeHist(&b, prefix+"http_request_duration_seconds", "请求耗时（秒），按场景", "scene",
		r.httpDur, durBuckets)
	if len(r.httpBytes) > 0 {
		b.WriteString("# HELP " + prefix + "http_response_bytes_total 写给客户端的响应字节数\n")
		b.WriteString("# TYPE " + prefix + "http_response_bytes_total counter\n")
		for _, s := range sortedKeys(r.httpBytes) {
			fmt.Fprintf(&b, "%shttp_response_bytes_total{scene=%q} %d\n", prefix, s, r.httpBytes[s])
		}
	}
	if len(r.cache) > 0 {
		b.WriteString("# HELP " + prefix + "cache_requests_total 缓存查询结果（hit/miss/pass）\n")
		b.WriteString("# TYPE " + prefix + "cache_requests_total counter\n")
		for _, k := range sortedCache(r.cache) {
			fmt.Fprintf(&b, "%scache_requests_total{scene=%q,result=%q} %d\n", prefix, k.scene, k.result, r.cache[k])
		}
	}
	// 缓存盘快照，janitor 按时刷新
	if r.haveCache {
		b.WriteString("# HELP " + prefix + "cache_bytes 缓存盘当前占用字节（磁盘实测，非索引之和）\n")
		b.WriteString("# TYPE " + prefix + "cache_bytes gauge\n")
		fmt.Fprintf(&b, "%scache_bytes %d\n", prefix, r.cacheBytes)
		b.WriteString("# HELP " + prefix + "cache_blobs 缓存条目数\n")
		b.WriteString("# TYPE " + prefix + "cache_blobs gauge\n")
		fmt.Fprintf(&b, "%scache_blobs %d\n", prefix, r.cacheBlobs)
		b.WriteString("# HELP " + prefix + "cache_free_bytes 缓存盘剩余字节\n")
		b.WriteString("# TYPE " + prefix + "cache_free_bytes gauge\n")
		fmt.Fprintf(&b, "%scache_free_bytes %d\n", prefix, r.cacheFree)
	}
	// 源站数快照
	if r.haveSources {
		b.WriteString("# HELP " + prefix + "sources 各协议/状态的源站数量\n")
		b.WriteString("# TYPE " + prefix + "sources gauge\n")
		for _, k := range sortedSrcStat(r.sources) {
			fmt.Fprintf(&b, "%ssources{kind=%q,status=%q} %d\n", prefix, k.Kind, k.Status, r.sources[k])
		}
	}

	// 命中率：janitor 每轮用差值算
	if r.haveRatio {
		b.WriteString("# HELP " + prefix + "cache_hit_ratio 最近一个周期内的缓存命中率（0~1）\n")
		b.WriteString("# TYPE " + prefix + "cache_hit_ratio gauge\n")
		for _, s := range sortedKeys(r.hitRatio) {
			fmt.Fprintf(&b, "%scache_hit_ratio{scene=%q} %s\n", prefix, s, trimFloat(r.hitRatio[s]))
		}
	}

	// 热度：janitor 每轮取差值
	if r.haveHot {
		b.WriteString("# HELP " + prefix + "scene_requests_delta 最近一个周期内的场景请求量（热度）\n")
		b.WriteString("# TYPE " + prefix + "scene_requests_delta gauge\n")
		for _, s := range sortedKeys(r.sceneDelta) {
			fmt.Fprintf(&b, "%sscene_requests_delta{scene=%q} %d\n", prefix, s, r.sceneDelta[s])
		}
	}

	writeHist(&b, prefix+"download_throughput_bytes_per_second",
		"回源响应的端到端吞吐量（字节/秒），仅统计未命中缓存的响应", "scene", r.tp, tpBuckets)
	if len(r.srcReq) > 0 {
		b.WriteString("# HELP " + prefix + "source_requests_total 回源结果（ok/fail）\n")
		b.WriteString("# TYPE " + prefix + "source_requests_total counter\n")
		for _, k := range sortedSrc(r.srcReq) {
			fmt.Fprintf(&b, "%ssource_requests_total{kind=%q,result=%q} %d\n", prefix, k.kind, k.result, r.srcReq[k])
		}
	}
	if len(r.src429) > 0 {
		b.WriteString("# HELP " + prefix + "source_429_total 上游返回 429 的次数\n")
		b.WriteString("# TYPE " + prefix + "source_429_total counter\n")
		for _, s := range sortedKeys(r.src429) {
			fmt.Fprintf(&b, "%ssource_429_total{kind=%q} %d\n", prefix, s, r.src429[s])
		}
	}
	if len(r.srcMiss) > 0 {
		b.WriteString("# HELP " + prefix + "source_miss_total 源上没有这个资源的次数（404/410，不计为源站故障）\n")
		b.WriteString("# TYPE " + prefix + "source_miss_total counter\n")
		for _, k := range sortedMissKeys(r.srcMiss) {
			fmt.Fprintf(&b, "%ssource_miss_total{kind=%q,code=%q} %d\n", prefix, k.kind, strconv.Itoa(k.code), r.srcMiss[k])
		}
	}
	if len(r.srcResp) > 0 {
		b.WriteString("# HELP " + prefix + "source_http_responses_total 收到真实 HTTP 响应的次数（429 率的分母）\n")
		b.WriteString("# TYPE " + prefix + "source_http_responses_total counter\n")
		for _, s := range sortedKeys(r.srcResp) {
			fmt.Fprintf(&b, "%ssource_http_responses_total{kind=%q} %d\n", prefix, s, r.srcResp[s])
		}
	}
	writeHist2(&b, prefix+"source_fetch_duration_seconds",
		"回源完整耗时（秒）：mode=single 整次回源，stream 整次流式传输，chunk 单段", r.srcDur, durBuckets)
	// 首字节单独一列：调度看整次耗时，运维看源站响应速度。
	if len(r.srcTTFB) > 0 {
		writeHist2(&b, prefix+"source_ttfb_seconds",
			"回源首字节耗时（秒）：mode=stream 从发出请求到收到响应头", r.srcTTFB, durBuckets)
	}
	// 并发上限（动态调度调整后）
	if len(r.srclimit) > 0 {
		lids := make([]int64, 0, len(r.srclimit))
		for id := range r.srclimit {
			lids = append(lids, id)
		}
		sort.Slice(lids, func(i, j int) bool { return lids[i] < lids[j] })
		b.WriteString("# HELP " + prefix + "source_concurrency_limit 源站当前并发上限（动态调整）\n")
		b.WriteString("# TYPE " + prefix + "source_concurrency_limit gauge\n")
		for _, id := range lids {
			fmt.Fprintf(&b, "%ssource_concurrency_limit{source_id=%q} %d\n", prefix, strconv.FormatInt(id, 10), r.srclimit[id])
		}
	}
	ids := make([]int64, 0, len(r.inflight))
	for id := range r.inflight {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > 0 {
		b.WriteString("# HELP " + prefix + "source_inflight 源站当前并发占用数\n")
		b.WriteString("# TYPE " + prefix + "source_inflight gauge\n")
		for _, id := range ids {
			fmt.Fprintf(&b, "%ssource_inflight{source_id=%q} %d\n", prefix, strconv.FormatInt(id, 10), r.inflight[id])
		}
	}
	if len(r.limited) > 0 {
		b.WriteString("# HELP " + prefix + "ratelimit_rejected_total 入口限流拒绝数（按层）\n")
		b.WriteString("# TYPE " + prefix + "ratelimit_rejected_total counter\n")
		for _, s := range sortedKeys(r.limited) {
			fmt.Fprintf(&b, "%sratelimit_rejected_total{layer=%q} %d\n", prefix, s, r.limited[s])
		}
	}
	return []byte(b.String())
}

func writeHist(b *strings.Builder, name, help, label string, m map[string]*hist, buckets []float64) {
	if len(m) == 0 {
		return
	}
	keys := sortedKeys(m)
	writeHistHead(b, name, help, label)
	for _, k := range keys {
		writeHistBody(b, name, label, k, m[k], buckets)
	}
}

// writeHist2 处理双标签（kind,mode）。
func writeHist2(b *strings.Builder, name, help string, m map[srcDurKey]*hist, buckets []float64) {
	if len(m) == 0 {
		return
	}
	keys := make([]srcDurKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].mode < keys[j].mode
	})
	writeHistHead(b, name, help, "kind,mode")
	for _, k := range keys {
		writeHistBody2(b, name, k.kind, k.mode, m[k], buckets)
	}
}

func writeHistHead(b *strings.Builder, name, help, labels string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
}

func writeHistBody(b *strings.Builder, name, label, key string, h *hist, buckets []float64) {
	var cum uint64
	for i, le := range buckets {
		cum += h.counts[i]
		fmt.Fprintf(b, "%s_bucket{%s=%q,le=%q} %d\n", name, label, key, trimFloat(le), cum)
	}
	cum += h.counts[len(buckets)]
	fmt.Fprintf(b, "%s_bucket{%s=%q,le=\"+Inf\"} %d\n", name, label, key, cum)
	fmt.Fprintf(b, "%s_sum{%s=%q} %s\n", name, label, key, trimFloat(h.sum))
	fmt.Fprintf(b, "%s_count{%s=%q} %d\n", name, label, key, h.n)
}

func writeHistBody2(b *strings.Builder, name, kind, mode string, h *hist, buckets []float64) {
	var cum uint64
	for i, le := range buckets {
		cum += h.counts[i]
		fmt.Fprintf(b, "%s_bucket{kind=%q,mode=%q,le=%q} %d\n", name, kind, mode, trimFloat(le), cum)
	}
	cum += h.counts[len(buckets)]
	fmt.Fprintf(b, "%s_bucket{kind=%q,mode=%q,le=\"+Inf\"} %d\n", name, kind, mode, cum)
	fmt.Fprintf(b, "%s_sum{kind=%q,mode=%q} %s\n", name, kind, mode, trimFloat(h.sum))
	fmt.Fprintf(b, "%s_count{kind=%q,mode=%q} %d\n", name, kind, mode, h.n)
}

// trimFloat 用定点格式：比 1.048576e+06 好读，
// 也避开个别客户端对指数写法的解析差异。
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func sortedHTTP(m map[httpKey]uint64) []httpKey {
	out := make([]httpKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].scene != out[j].scene {
			return out[i].scene < out[j].scene
		}
		if out[i].method != out[j].method {
			return out[i].method < out[j].method
		}
		return out[i].code < out[j].code
	})
	return out
}

func sortedCache(m map[cacheKey]uint64) []cacheKey {
	out := make([]cacheKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].scene != out[j].scene {
			return out[i].scene < out[j].scene
		}
		return out[i].result < out[j].result
	})
	return out
}

func sortedSrc(m map[srcKey]uint64) []srcKey {
	out := make([]srcKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].result < out[j].result
	})
	return out
}

// sortedMissKeys 保证输出顺序稳定，两次抓取能直接 diff。
func sortedMissKeys(m map[srcMissKey]uint64) []srcMissKey {
	out := make([]srcMissKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].code < out[j].code
	})
	return out
}

func sortedSrcStat(m map[SourceStat]uint64) []SourceStat {
	out := make([]SourceStat, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Status < out[j].Status
	})
	return out
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
