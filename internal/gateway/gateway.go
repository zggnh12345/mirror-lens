// Package gateway 是统一加速入口：认证 → 适配器解析 → 缓存 → 回源。
//
// 阶段 0/1 只挂载 /raw/，但流程和接口按后续 /http/、/git/、/v2/ 的形状写。
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
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
	"github.com/zggnh12345/mirror-lens/internal/keymutex"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

const Version = "2.6.0-dev"

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
	maxSize int64

	cacheMaxBytes int64
	quotaMu       sync.Mutex
	gc            *gc.GC
	now           func() time.Time
	sched         *scheduler.Scheduler
	flights       sync.Map      // cacheKey -> *flight（缓冲型适配器用）
	locks         *keymutex.Map // cacheKey -> 锁（流式填充用，空闲即回收）
	verifyFail    atomic.Int64  // 校验不过、因此没落盘的次数（进程内计数）
	par           ParOptions
	parTargets    func(*adapter.Plan) []fetchTarget // 测试注入用，nil 时走 targets()
	requireToken  bool                              // 代理路径是否强制 Access Token（false = 匿名放行）
	parFills      atomic.Int64                      // 真正走了并行分片的次数
	parBytes      atomic.Int64                      // 并行分片下下来的字节
	parFalls      atomic.Int64                      // 试过但退回单流（不支持 Range、源不够等）
}

// ParOptions 是并行分片的运行时参数，来自配置。
type ParOptions struct {
	Enabled   bool
	MinSize   int64
	ChunkSize int64
	Workers   int
}

type Options struct {
	Cache         *cache.Store
	RawBase       string
	MaxBytes      int64
	GitTTL        time.Duration
	RawTTL        time.Duration
	Registry      *registryadapter.Adapter
	CacheMaxBytes int64 // 缓存总量上限（字节），0 = 不限
	Guard         *safehttp.Guard
	Parallel      ParOptions
	// RequireToken 控制 /raw /http /git /v2 是否需要 Access Token。
	// 默认 false：匿名可读，适合当公共加速站；true 时非浏览器客户端必须带 token。
	RequireToken bool
}

func New(st *store.Store, o Options) *Gateway {
	if o.MaxBytes <= 0 {
		o.MaxBytes = 256 << 20
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
	return &Gateway{
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
		gc:            gc.New(st, o.Cache),
		now:           time.Now,
		par:           o.Parallel,
		requireToken:  o.RequireToken,
	}
}

// SetRawTTL 给测试用：把可变资源的重新验证间隔调小。
// GC 暴露给 API 层做手动清理，保证和淘汰走同一套删除逻辑。
func (g *Gateway) GC() *gc.GC { return g.gc }

// CacheStore 给统计接口算磁盘实际占用。
func (g *Gateway) CacheStore() *cache.Store { return g.cache }

// VerifyFailed 返回进程内「校验不过、因此没落盘」的次数。
//
// 没有对应的缓存记录可查：坏内容一字节都不写索引，所以只能用计数器留痕。
func (g *Gateway) VerifyFailed() int64 {
	if g == nil {
		return 0
	}
	return g.verifyFail.Load()
}

// CacheLimit 是配置里的容量上限，0 表示不限。
func (g *Gateway) CacheLimit() int64 { return g.cacheMaxBytes }

func (g *Gateway) SetRawTTL(d time.Duration) { g.raw.TTL = d }

// SetGitTTL 调整 info/refs 的缓存时长；0 表示不缓存。
func (g *Gateway) SetGitTTL(d time.Duration) { g.gitAd.TTL = d }

type flight struct {
	done   chan struct{}
	entry  *store.CacheEntry
	notMod bool
	err    error
}

// setSource 告诉调用方这次是谁供的货，排查「为什么变慢」时一眼能看出。
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
	g.withAuth(func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, g.raw)
	})(w, r)
}

// ServeGH 是 release 资产与源码包的入口。和 /raw/ 一样自带 Bearer/Basic 认证：
// 客户端是 curl、wget、gh，不会带面板 cookie。
func (g *Gateway) ServeGH(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	prev, err := g.st.CacheGet(plan.CacheKey)
	if err != nil {
		log.Printf("缓存索引读取失败 %s: %v", plan.CacheKey, err)
	}

	if prev != nil {
		fresh := plan.Immutable || g.now().Sub(time.Unix(prev.CreatedAt, 0)) < plan.TTL
		if fresh {
			g.serveCached(w, r, prev, "HIT", true)
			return
		}
		entry, notMod, err := g.fetchOnce(r.Context(), plan, prev)
		switch {
		case err == nil && notMod:
			g.st.CacheRefresh(plan.CacheKey)
			g.serveCached(w, r, prev, "HIT-REVAL", true)
			return
		case err == nil:
			setSource(w, plan)
			g.serveCached(w, r, entry, "MISS", false)
			return
		default:
			var ue *upstreamError
			if errors.As(err, &ue) && (ue.Code == http.StatusNotFound || ue.Code == http.StatusGone) {
				// 上游说文件没了，就不要拿旧的糊弄人
				g.st.CacheDelete(plan.CacheKey)
				http.Error(w, fmt.Sprintf("%d 上游返回 %d", http.StatusNotFound, ue.Code), http.StatusNotFound)
				return
			}
			// 其余错误（超时、5xx、连不上）先给缓存里的旧版本，标 STALE
			log.Printf("回源失败，使用旧缓存 %s: %v", plan.CacheKey, err)
			g.serveCached(w, r, prev, "STALE", true)
			return
		}
	}

	entry, _, err := g.fetchOnce(r.Context(), plan, nil)
	if err != nil {
		code := statusFor(err)
		log.Printf("回源失败 %s: %v", plan.CacheKey, err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}
	setSource(w, plan)
	g.serveCached(w, r, entry, "MISS", false)
}

// fetchOnce 保证同一个缓存键同时只有一次回源：并发请求等着，
// 拿到的都是同一次写入完成后的完整文件。
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
	u *url.URL
	c *scheduler.Candidate
}

// targets 决定这次回源按什么顺序试哪些地址。
//
// prev != nil 表示这是一次 TTL 到期后的重新验证：只回官方源。
// 镜像站可能把旧内容当成「没变」，让它参与验证等于把过期数据固化下来。
func (g *Gateway) targets(plan *adapter.Plan, prev *store.CacheEntry) []fetchTarget {
	out := []fetchTarget{{u: plan.Upstream}}
	if prev != nil || !g.sched.Enabled() {
		return out
	}
	cands := g.sched.Candidates(plan.Kind)
	var head []fetchTarget
	for _, c := range cands {
		cc := c
		u, ok := g.sched.Rewrite(plan.Kind, plan.Upstream, cc)
		if !ok || strings.EqualFold(u.Host, plan.Upstream.Host) {
			continue
		}
		head = append(head, fetchTarget{u: u, c: &cc})
	}
	return append(head, out...)
}

func (g *Gateway) fetch(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry) (*store.CacheEntry, bool, error) {
	if plan.Upstream == nil || plan.Upstream.Host == "" {
		return nil, false, adapter.ErrNoTarget
	}
	var lastErr error
	for _, t := range g.targets(plan, prev) {
		start := time.Now()
		e, notMod, err := g.fetchURL(ctx, plan, prev, t.u)
		if err == nil {
			if t.c != nil {
				g.sched.Report(plan.Kind, *t.c, true, time.Since(start))
				plan.Source = t.c.Name
			}
			return e, notMod, nil
		}
		if t.c == nil {
			return nil, false, err // 官方源失败就是失败，不再兜圈子
		}
		// 客户端自己取消（关页面、断网）不算这台源的账。
		if ctx.Err() == nil {
			var hint scheduler.Hint
			var ue *upstreamError
			if errors.As(err, &ue) {
				hint.RetryAfter = ue.RetryAfter
				hint.Code = ue.Code
			}
			g.sched.Report(plan.Kind, *t.c, false, time.Since(start), hint)
		}
		log.Printf("源 %s 不可用，换下一个：%v", t.c.Name, err)
		lastErr = err
	}
	return nil, false, lastErr
}

func (g *Gateway) fetchURL(ctx context.Context, plan *adapter.Plan, prev *store.CacheEntry, u *url.URL) (*store.CacheEntry, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, err
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
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return prev, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		io.CopyN(io.Discard, resp.Body, 4096)
		return nil, false, &upstreamError{Code: resp.StatusCode, RetryAfter: retryAfterHeader(resp.Header)}
	}
	if plan.Validate != nil {
		if err := plan.Validate(resp); err != nil {
			return nil, false, err
		}
	}

	tmp, err := g.cache.CreateTemp()
	if err != nil {
		return nil, false, err
	}
	tmpName := tmp.Name()
	// 无论走到哪条分支都要注销：否则这个文件名会被 SweepTemp 永久跳过。
	defer g.cache.ForgetTemp(tmpName)
	cleanup := func() {
		tmp.Close()
		removeFile(tmpName)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, g.maxSize+1))
	if err != nil {
		cleanup()
		return nil, false, fmt.Errorf("读取上游中断: %w", err)
	}
	if n > g.maxSize {
		cleanup()
		return nil, false, &tooLargeError{Max: g.maxSize}
	}
	// 上游声明了长度就必须一致，否则是截断的响应
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		cleanup()
		return nil, false, fmt.Errorf("长度不符：期望 %d，实际 %d", resp.ContentLength, n)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, false, err
	}
	if err := tmp.Close(); err != nil {
		removeFile(tmpName)
		return nil, false, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if plan.ExpectSHA != "" && !strings.EqualFold(sum, plan.ExpectSHA) {
		removeFile(tmpName)
		g.verifyFail.Add(1)
		return nil, false, fmt.Errorf("内容与客户端给的摘要不符：期望 %s 实得 %s", plan.ExpectSHA, sum)
	}
	if checked, ok := checkDigest(resp.Header, sum); checked && !ok {
		removeFile(tmpName)
		g.verifyFail.Add(1)
		return nil, false, fmt.Errorf("内容与上游声明的摘要不符：%s", plan.Upstream)
	}
	e := &store.CacheEntry{
		Key:          plan.CacheKey,
		SHA256:       sum,
		Size:         n,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Upstream:     u.String(),
		CreatedAt:    g.now().Unix(),
		TTL:          entryTTL(plan),
		Verify:       verifyState(plan),
	}
	// 落盘和写索引必须在同一把 blob 锁里：janitor 的孤儿回收也在锁内查引用再删，
	// 否则「文件已就位、索引还没写」的瞬间会被当成孤儿删掉。
	unlock := g.cache.LockBlob(sum)
	if _, err := g.cache.Commit(tmpName, sum); err != nil {
		unlock()
		removeFile(tmpName)
		return nil, false, err
	}
	err = g.st.CachePut(*e)
	unlock()
	if err != nil {
		log.Printf("写缓存索引失败 %s: %v", plan.CacheKey, err)
	}
	g.st.RecordTraffic(store.TrafficFill, store.KindOfKey(plan.CacheKey), n)
	return e, false, nil
}

// serveCached 发缓存里的内容。fromCache=false 表示这份内容是刚回源填进来的：
// 回源字节已经由填充那边记过 fill，这里再记一次 hit 会把命中率算高。
// 统计的 hit 字节按实际写给客户端的量算——Range 只要 10 MB 就记 10 MB，
// 不是整个文件的长度。
func (g *Gateway) serveCached(w http.ResponseWriter, r *http.Request, e *store.CacheEntry, label string, fromCache bool) {
	if e == nil {
		http.Error(w, "502 缓存条目缺失", http.StatusBadGateway)
		return
	}
	f, err := g.cache.Open(e.SHA256)
	if err != nil {
		// 索引有、文件没了：删掉索引，让下次重新回源
		log.Printf("缓存文件丢失 %s (%s): %v", e.Key, e.SHA256, err)
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
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("X-ML-Cache", label)
	cw := &countWriter{ResponseWriter: w}
	// ServeContent 负责 Range / 206 / 416 / If-None-Match，不用自己写
	http.ServeContent(cw, r, path.Base(e.Key), time.Unix(e.CreatedAt, 0), f)
	if fromCache {
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
		if auth.Authed(r, g.st) {
			next(w, r)
			return
		}
		if tok := proxyToken(r); tok != "" {
			if _, ok := g.st.VerifyToken(tok); ok {
				next(w, r)
				return
			}
		}
		// Basic 让 git/docker 客户端弹认证框；密码位填 Access Token
		w.Header().Set("WWW-Authenticate", `Basic realm="Mirror Lens", charset="UTF-8"`)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "401 需要 Access Token\nAuthorization: Bearer <token>\n")
	}
}

func removeFile(name string) { _ = os.Remove(name) }

// countWriter 只数真正写给客户端的字节，304 和 Range 都不会多算。
type countWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// entryTTL 把计划里的新鲜期换算成秒；不可变资源给一个足够长的值。
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

type upstreamError struct {
	Code int
	// RetryAfter 是 429 响应里源自己说的等待时间，解析不出来就是 0。
	RetryAfter time.Duration
}

func (e *upstreamError) Error() string { return fmt.Sprintf("上游返回 %d", e.Code) }

// retryAfterHeader 解析 Retry-After：秒数或 HTTP-date，两种写法都合法。
// 解析不出来、或者给了个过去的时间，就当没有这个头。
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

type tooLargeError struct{ Max int64 }

func (e *tooLargeError) Error() string {
	return fmt.Sprintf("文件超过单文件上限 %d MB", e.Max>>20)
}

func statusFor(err error) int {
	var ue *upstreamError
	if errors.As(err, &ue) {
		switch ue.Code {
		case http.StatusNotFound, http.StatusGone:
			return http.StatusNotFound
		case http.StatusForbidden:
			return http.StatusForbidden
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
