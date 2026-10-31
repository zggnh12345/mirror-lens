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
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// ServeHTTPProxy 处理 /http/<编码后的完整地址>。
func (g *Gateway) ServeHTTPProxy(w http.ResponseWriter, r *http.Request) {
	g.withAuth(func(w http.ResponseWriter, r *http.Request) { g.serveHTTP(w, r) })(w, r)
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 只支持 GET/HEAD", http.StatusMethodNotAllowed)
		return
	}
	plan, err := g.httpAd.Plan(r)
	if err != nil {
		noStore(w.Header())
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := g.checkTarget(plan.Upstream, r.Host); err != nil {
		code := statusFor(err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}

	// 缓存里有就直接从盘上切：ServeContent 会自己处理 Range 和 HEAD。
	if e, _ := g.st.CacheGet(plan.CacheKey); e != nil && g.fresh(e) {
		g.serveCached(w, r, e, "HIT", true)
		return
	}

	// 缓存里没有、客户端又只要一段（下载器续传）或者只是 HEAD：直通，不落盘。
	// 这种请求本来就不需要完整文件，先缓存整份再切片只会白占盘。
	if r.Header.Get("Range") != "" || r.Method == http.MethodHead {
		g.passThrough(w, r, plan)
		return
	}

	// 同一个键串行填充：后来的请求等前一个填完，直接读盘，不会两份字节抢一个文件。
	unlock := g.lockKey(plan.CacheKey)
	defer unlock()
	if e, _ := g.st.CacheGet(plan.CacheKey); e != nil && g.fresh(e) {
		g.serveCached(w, r, e, "HIT", true)
		return
	}
	g.fillStream(w, r, plan)
}

// checkTarget 在发出任何字节之前先判一次目标，别等拨号才发现是内网。
func (g *Gateway) checkTarget(u *url.URL, selfHost string) error {
	if err := g.guard.Check(u); err != nil {
		return err
	}
	if selfHost != "" && strings.EqualFold(u.Host, selfHost) {
		return fmt.Errorf("%w: 不能代理自己", adapter.ErrBadPath)
	}
	return nil
}

func (g *Gateway) fresh(e *store.CacheEntry) bool {
	if e.TTL <= 0 {
		return false
	}
	return g.now().Unix()-e.CreatedAt < e.TTL
}

// lockKey 锁住同一个缓存键的填充流程。用带引用计数的表，
// 否则每个不同的 URL 都会留下一把锁，跑久了只增不减。
func (g *Gateway) lockKey(k string) func() {
	return g.locks.Lock(k)
}

// fillStream 边下边发；中途断了留下 .part，下次带 If-Range 续。
func (g *Gateway) fillStream(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) {
	prev, _ := g.st.CacheGet(plan.CacheKey)

	var pm *cache.PartialMeta
	if p, ok := g.cache.ReadPartial(plan.CacheKey); ok {
		if p.URL == plan.Upstream.String() {
			pm = p
		} else {
			g.cache.DropPartial(plan.CacheKey)
		}
	}

	// 阶段 7：有可信摘要、多台源都支持 Range 时，分片并行下载。
	// 它自己判断前提是否成立；不成立就什么都不写，继续下面的单流逻辑。
	if pm == nil && prev == nil && g.tryParallel(w, r, plan) {
		return
	}

	for attempt := 0; attempt < 2; attempt++ {
		resp, tgt, ttfb, err := g.upstreamRequestFrom(r.Context(), plan, pm, prev)
		if err != nil {
			g.fail(w, r, plan, prev, err)
			return
		}

		switch resp.StatusCode {
		case http.StatusNotModified:
			resp.Body.Close()
			if prev != nil {
				g.st.CacheRefresh(plan.CacheKey)
				g.serveCached(w, r, prev, "HIT-REVAL", true)
				return
			}
			prev = nil // 没有缓存却回了 304，去掉条件头再来
			continue

		case http.StatusPartialContent:
			start, total, err := parseContentRange(resp.Header.Get("Content-Range"))
			if pm == nil || err != nil || start != pm.Have || total != pm.Size {
				// 上游给的片段和我们的续传点对不上，宁可从头下
				resp.Body.Close()
				g.cache.DropPartial(plan.CacheKey)
				pm = nil
				continue
			}

		case http.StatusRequestedRangeNotSatisfiable:
			resp.Body.Close()
			g.cache.DropPartial(plan.CacheKey)
			pm = nil
			continue

		case http.StatusOK:
			pm = nil // 上游忽略 Range（或本来就没续传），从头写

		default:
			resp.Body.Close()
			g.fail(w, r, plan, prev, &upstreamError{Code: resp.StatusCode})
			return
		}

		g.streamBody(w, r, plan, resp, pm, tgt, ttfb)
		return
	}
	http.Error(w, "502 上游反复返回对不上的分段响应", http.StatusBadGateway)
}

// upstreamRequestFrom 在真正开始传输之前选源。
//
// 只在这个时刻能换源：一旦响应头写给客户端，再换就等于给客户端拼一段
// 来源不同的字节流。已经有半截文件（pm）或正在重新验证（prev）时也只回官方源，
// 因为续传的 If-Range 与 ETag 都绑定了原来那台服务器。
func (g *Gateway) upstreamRequestFrom(ctx context.Context, plan *adapter.Plan, pm *cache.PartialMeta, prev *store.CacheEntry) (*http.Response, *fetchTarget, time.Duration, error) {
	targets := g.targets(plan, prev)
	if pm != nil {
		targets = targets[:1] // 续传只认官方源
		if len(targets) == 1 {
			targets[0] = fetchTarget{u: plan.Upstream}
		}
	}
	var lastErr error
	for i := range targets {
		t := targets[i]
		start := time.Now()
		resp, err := g.doUpstream(ctx, plan, pm, prev, t.u)
		if err != nil {
			if t.c == nil {
				return nil, nil, 0, err
			}
			// 客户端自己取消（关页面/断网）不是这台源的问题。
			if ctx.Err() == nil {
				g.sched.Report(plan.Kind, *t.c, false, time.Since(start))
			}
			log.Printf("源 %s 连不上，换下一个：%v", t.c.Name, err)
			lastErr = err
			continue
		}
		if t.c != nil && retryableFromMirror(resp.StatusCode) {
			io.CopyN(io.Discard, resp.Body, 4096)
			resp.Body.Close()
			ue := &upstreamError{Code: resp.StatusCode, RetryAfter: retryAfterHeader(resp.Header)}
			g.sched.Report(plan.Kind, *t.c, false, time.Since(start), scheduler.Hint{RetryAfter: ue.RetryAfter, Code: ue.Code})
			if ue.RetryAfter > 0 {
				log.Printf("源 %s 返回 %d，要求 %s 后再来，换下一个", t.c.Name, resp.StatusCode, ue.RetryAfter)
			} else {
				log.Printf("源 %s 返回 %d，换下一个", t.c.Name, resp.StatusCode)
			}
			lastErr = ue
			continue
		}
		// 成功不在这里记：响应头到了不代表身子能收完。见 streamBody 的收尾。
		if t.c != nil {
			plan.Source = t.c.Name
			plan.Upstream = t.u
		}
		return resp, &t, time.Since(start), nil
	}
	if lastErr == nil {
		lastErr = adapter.ErrNoTarget
	}
	return nil, nil, 0, lastErr
}

// 镜像站的这几种状态码代表「这台源现在不好用」，值得换一台；
// 404/410 是资源本身不存在，换源也没用，直接按上游结果处理。
func retryableFromMirror(code int) bool {
	switch code {
	case http.StatusForbidden, http.StatusUnauthorized, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return code >= 500
}

func (g *Gateway) doUpstream(ctx context.Context, plan *adapter.Plan, pm *cache.PartialMeta, prev *store.CacheEntry, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mirror-lens/"+Version+" (+https://github.com/zggnh12345/mirror-lens)")
	req.Header.Set("Accept-Encoding", "identity")
	for k, vs := range plan.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	switch {
	case pm != nil && pm.Have > 0:
		req.Header.Set("Range", "bytes="+strconv.FormatInt(pm.Have, 10)+"-")
		if pm.ETag != "" {
			req.Header.Set("If-Range", pm.ETag)
		} else {
			req.Header.Set("If-Range", pm.LastModified)
		}
	case prev != nil:
		if prev.ETag != "" {
			req.Header.Set("If-None-Match", prev.ETag)
		} else if prev.LastModified != "" {
			req.Header.Set("If-Modified-Since", prev.LastModified)
		}
	}
	return g.cli.Do(req)
}

// 取数的三态结局。中性表示「不是源的错」：客户端自己断开、本地磁盘写不进去，
// 这些都不该影响这台源的评分。
type fetchOutcome int

const (
	outcomeNeutral fetchOutcome = iota
	outcomeOK
	outcomeFail
)

func (g *Gateway) streamBody(w http.ResponseWriter, r *http.Request, plan *adapter.Plan, resp *http.Response, pm *cache.PartialMeta, tgt *fetchTarget, ttfb time.Duration) {
	defer resp.Body.Close()

	outcome := outcomeNeutral
	defer func() {
		if tgt == nil || outcome == outcomeNeutral {
			return
		}
		// 只有整份传完才算成功：头到了、身子断了，对客户端就是一次失败下载。
		// 提前记成功会让这台源一直粘着（每次成功把 sticky 续到 10 分钟后），
		// 熔断也永远不触发——「握手快、传输慢」的源能把全部流量攥在手里。
		g.reportSource(plan.Kind, *tgt, outcome == outcomeOK, ttfb, scheduler.Hint{Code: resp.StatusCode, Mode: "stream"})
	}()

	total := resp.ContentLength
	have := int64(0)
	if resp.StatusCode == http.StatusPartialContent {
		start, t, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			log.Printf("上游 Content-Range 解析失败 %s: %v", plan.Upstream, err)
			http.Error(w, "502 上游分段响应不合法", http.StatusBadGateway)
			return
		}
		have, total = start, t
	}

	cacheable := plan.Cacheable
	ttl := plan.TTL
	if plan.OnResponse != nil {
		cacheable, ttl = plan.OnResponse(resp)
	}
	limit := g.limitFor(plan)
	if total > limit {
		cacheable = false // 超过单文件上限就只透传，不占盘
	}

	h := w.Header()
	copyHeader(h, resp.Header, "Content-Type", "Content-Disposition", "Content-Language")
	h.Set("Accept-Ranges", "bytes")
	if et := resp.Header.Get("ETag"); et != "" {
		h.Set("ETag", et)
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		h.Set("Last-Modified", lm)
	}
	if total >= 0 {
		h.Set("Content-Length", strconv.FormatInt(total, 10))
	}
	label := "MISS"
	if have > 0 {
		label = "RESUME"
	}
	if !cacheable {
		label = "BYPASS" // 不落盘：太大、no-store，或者写盘出错
	}
	h.Set("X-ML-Cache", label)
	if plan.Source != "" {
		h.Set("X-ML-Source", plan.Source)
	}
	// 边下边发也要把缓存语义说清楚：没有它，边缘只能按默认策略猜，
	// 大文件反而成了最不可缓存的那类。
	g.cacheHeadersPlan(h, plan)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	hasher := sha256.New()
	var part *os.File
	sent := int64(0)

	if cacheable {
		partPath, _ := g.cache.PartialPaths(plan.CacheKey)
		if have > 0 {
			// 上次已经下好的字节，这次先补给客户端，同时接着算哈希
			pf, err := os.Open(partPath)
			if err == nil {
				n, cerr := io.Copy(io.MultiWriter(w, hasher), pf)
				pf.Close()
				if cerr != nil {
					g.keepPartial(plan, resp, total, n, nil)
					return
				}
				sent = n
			}
			if sent != have {
				have, sent = 0, 0
				cacheable = false
				g.cache.DropPartial(plan.CacheKey)
			}
		}
		if cacheable {
			if have == 0 {
				if err := g.cache.ResetPartial(plan.CacheKey); err != nil {
					cacheable = false
				}
			}
			if cacheable {
				var err error
				part, err = g.cache.OpenPartial(plan.CacheKey)
				if err != nil {
					log.Printf("打开缓存文件失败，降级为透传: %v", err)
					cacheable = false
				}
			}
		}
	}

	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// 客户端走了：把已下部分留给下一次续传
				g.keepPartial(plan, resp, total, sent, part)
				return
			}
			if part != nil {
				if _, werr := part.Write(buf[:n]); werr != nil {
					part.Close()
					part = nil
					g.cache.DropPartial(plan.CacheKey)
					log.Printf("写缓存失败，剩下的转透传: %v", werr)
				} else {
					hasher.Write(buf[:n])
				}
			}
			sent += int64(n)
			if total >= 0 && sent > total {
				if part != nil {
					part.Close()
					part = nil
				}
				g.cache.DropPartial(plan.CacheKey)
				log.Printf("上游超出声明的长度，丢弃缓存: %s", plan.Upstream)
				outcome = outcomeFail
				return
			}
			if part != nil && sent > limit {
				part.Close()
				part = nil
				g.cache.DropPartial(plan.CacheKey)
				log.Printf("超过单文件上限，转透传: %s", plan.Upstream)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// 客户端自己走了（取消）不算源的错；否则就是「头到了、身子断了」。
			if r.Context().Err() == nil {
				outcome = outcomeFail
			}
			g.keepPartial(plan, resp, total, sent, part)
			return
		}
	}

	if part == nil {
		if total >= 0 && sent != total {
			outcome = outcomeFail
		} else {
			outcome = outcomeOK
		}
		g.st.RecordTraffic(store.TrafficBypass, store.KindOfKey(plan.CacheKey), sent-have)
		return
	}
	part.Close()
	if total >= 0 && sent != total {
		g.cache.DropPartial(plan.CacheKey)
		log.Printf("长度不符，丢弃：期望 %d 实得 %d (%s)", total, sent, plan.Upstream)
		if r.Context().Err() == nil {
			outcome = outcomeFail
		}
		return
	}

	sha := hex.EncodeToString(hasher.Sum(nil))
	if plan.ExpectSHA != "" && !strings.EqualFold(sha, plan.ExpectSHA) {
		// 上游给错了字节（或中途被换过）。客户端已经收到一部分，但它自己也会校验；
		// 这里唯一要保证的是：绝不把错的内容写进缓存。
		g.cache.DropPartial(plan.CacheKey)
		g.verifyFail.Add(1)
		log.Printf("内容校验失败，不入缓存：期望 %s 实得 %s (%s)", plan.ExpectSHA, sha, plan.Upstream)
		outcome = outcomeFail
		return
	}
	if checked, ok := checkDigest(resp.Header, sha); checked && !ok {
		g.cache.DropPartial(plan.CacheKey)
		g.verifyFail.Add(1)
		log.Printf("内容与上游声明的摘要不符，不入缓存：%s", plan.Upstream)
		outcome = outcomeFail
		return
	}
	partPath, _ := g.cache.PartialPaths(plan.CacheKey)
	// 与 fetch()/并行填充同一把 blob 锁：落盘到写索引之间，janitor 不能插进来
	// 把这个「文件已在、索引还没写」的 blob 当孤儿删掉。
	unlock := g.cache.LockBlob(sha)
	if _, err := g.cache.Commit(partPath, sha); err != nil {
		unlock()
		g.cache.DropPartial(plan.CacheKey)
		log.Printf("缓存落盘失败: %v", err)
		return
	}
	e := store.CacheEntry{
		Key:          plan.CacheKey,
		SHA256:       sha,
		Size:         sent,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Upstream:     plan.Upstream.String(),
		CreatedAt:    g.now().Unix(),
		TTL:          int64(ttl.Seconds()),
		Verify:       verifyState(plan),
	}
	err := g.st.CachePut(e)
	unlock()
	if err != nil {
		log.Printf("写缓存索引失败 %s: %v", plan.CacheKey, err)
	}
	g.cache.DropPartial(plan.CacheKey)
	g.st.RecordTraffic(store.TrafficFill, store.KindOfKey(plan.CacheKey), sent)
	outcome = outcomeOK
}

// keepPartial 保留半截文件供下次续传；没有验证依据的（无 ETag/Last-Modified）直接扔掉，
// 否则可能把旧版本的前半段拼到新版本上。
func (g *Gateway) keepPartial(plan *adapter.Plan, resp *http.Response, total, sent int64, part *os.File) {
	if part != nil {
		part.Close()
	}
	et := resp.Header.Get("ETag")
	lm := resp.Header.Get("Last-Modified")
	if total < 0 || sent <= 0 || sent >= total || (et == "" && lm == "") {
		g.cache.DropPartial(plan.CacheKey)
		return
	}
	if err := g.cache.WritePartial(plan.CacheKey, cache.PartialMeta{
		URL:          plan.Upstream.String(),
		ETag:         et,
		LastModified: lm,
		ContentType:  resp.Header.Get("Content-Type"),
		Size:         total,
		Have:         sent,
		Updated:      g.now().Unix(),
	}); err != nil {
		log.Printf("记录续传点失败: %v", err)
		g.cache.DropPartial(plan.CacheKey)
	}
}

// passThrough 原样转发 Range / 条件请求，不碰缓存。
func (g *Gateway) passThrough(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) {
	// 直通也要挑源：Range 请求常常是对大文件的续传，绕过镜像等于把最费
	// 流量的那部分请求原样丢回官方源。
	targets := g.targets(plan, nil)
	if !plan.Stream {
		targets = targets[len(targets)-1:] // 注册表自己已经挑过源了
	}

	var (
		resp *http.Response
		err  error
		src  string
	)
	for i, t := range targets {
		req, rerr := http.NewRequestWithContext(r.Context(), r.Method, t.u.String(), nil)
		if rerr != nil {
			err = rerr
			continue
		}
		req.Header.Set("User-Agent", "mirror-lens/"+Version+" (+https://github.com/zggnh12345/mirror-lens)")
		req.Header.Set("Accept-Encoding", "identity")
		for _, k := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
			if v := r.Header.Get(k); v != "" {
				req.Header.Set(k, v)
			}
		}
		for k, vs := range plan.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err = g.cli.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode >= 500 && i < len(targets)-1 {
			resp.Body.Close()
			continue
		}
		if t.c != nil {
			src = t.c.Name
			plan.Source = src
		}
		break
	}
	if resp == nil {
		code := statusFor(err)
		log.Printf("透传失败 %s: %v", plan.Upstream, err)
		noStore(w.Header())
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header, "Content-Type", "Content-Length", "Content-Range",
		"Accept-Ranges", "ETag", "Last-Modified", "Content-Disposition", "Cache-Control",
		"Docker-Content-Digest", "Docker-Distribution-Api-Version")
	// 上游经常 307 到 CDN，落地的响应里没有 Docker-Content-Digest。
	// 补的是「请求路径里的摘要」，不是网关算出来的结果：直通既不落盘也不校验，
	// 只保证 containerd 拿到它要的那个值。X-ML-Digest 标明来源，
	// 免得把「补齐」读成「已验证」。
	if w.Header().Get("Docker-Content-Digest") == "" {
		if d := blobDigestFromPath(r.URL.Path); d != "" {
			w.Header().Set("Docker-Content-Digest", d)
			w.Header().Set("X-ML-Digest", "request")
		}
	}
	if src != "" {
		w.Header().Set("X-ML-Source", src)
	}
	w.Header().Set("X-ML-Cache", "PASS")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	n, _ := io.Copy(w, resp.Body)
	g.st.RecordTraffic(store.TrafficBypass, store.KindOfKey(plan.CacheKey), n)
}

func (g *Gateway) fail(w http.ResponseWriter, r *http.Request, plan *adapter.Plan, prev *store.CacheEntry, err error) {
	var ue *upstreamError
	if errors.As(err, &ue) && (ue.Code == http.StatusNotFound || ue.Code == http.StatusGone) {
		g.st.CacheDelete(plan.CacheKey)
		noStore(w.Header())
		http.Error(w, fmt.Sprintf("404 上游返回 %d", ue.Code), http.StatusNotFound)
		return
	}
	if prev != nil {
		log.Printf("回源失败，用旧缓存 %s: %v", plan.CacheKey, err)
		g.serveCached(w, r, prev, "STALE", true)
		return
	}
	code := statusFor(err)
	log.Printf("回源失败 %s: %v", plan.CacheKey, err)
	noStore(w.Header())
	http.Error(w, fmt.Sprintf("%d %v", code, err), code)
}

// blobDigestFromPath 从 /v2/<name>/blobs/<digest> 里取出摘要；不是 blob 路径返回空。
func blobDigestFromPath(p string) string {
	i := strings.LastIndex(p, "/blobs/")
	if i < 0 {
		return ""
	}
	d := p[i+len("/blobs/"):]
	if !strings.HasPrefix(d, "sha256:") || strings.ContainsAny(d, "/?#") || len(d) != len("sha256:")+64 {
		return ""
	}
	return d
}

// parseContentRange 解析 "bytes 100-499/1234"，总长未知时返回 -1。
func parseContentRange(s string) (start, total int64, err error) {
	if !strings.HasPrefix(s, "bytes ") {
		return 0, 0, fmt.Errorf("Content-Range 格式不对: %q", s)
	}
	s = strings.TrimPrefix(s, "bytes ")
	slash := strings.LastIndex(s, "/")
	if slash < 0 {
		return 0, 0, fmt.Errorf("Content-Range 缺总长: %q", s)
	}
	rangePart, totalPart := s[:slash], s[slash+1:]
	dash := strings.Index(rangePart, "-")
	if dash < 0 {
		return 0, 0, fmt.Errorf("Content-Range 缺区间: %q", s)
	}
	start, err = strconv.ParseInt(rangePart[:dash], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if totalPart == "*" {
		return start, -1, nil
	}
	total, err = strconv.ParseInt(totalPart, 10, 64)
	return start, total, err
}

func copyHeader(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}
