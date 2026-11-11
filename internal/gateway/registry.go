package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// RegistryOn 告诉 api 层要不要挂 /v2/ 路由。
func (g *Gateway) RegistryOn() bool { return g.regAd != nil }

// ServeRegistry 处理 /v2/...，只读。
func (g *Gateway) ServeRegistry(w http.ResponseWriter, r *http.Request) {
	g.withAuth(func(w http.ResponseWriter, r *http.Request) { g.serveRegistry(w, r) })(w, r)
}

// registryChannelOff 判断某仓库属于哪条通道、该通道是否被关掉。
func (g *Gateway) registryChannelOff(repo string) (string, bool) {
	if strings.HasPrefix(repo, "ghcr.io/") {
		if g.ghcrOff {
			return "GHCR 加速未开启", true
		}
		return "", false
	}
	if g.dockerOff {
		return "Docker 加速未开启", true
	}
	return "", false
}

func (g *Gateway) serveRegistry(w http.ResponseWriter, r *http.Request) {
	if g.regAd == nil {
		http.Error(w, "404 Docker 加速未开启", http.StatusNotFound)
		return
	}
	// OCI referrers（SBOM/签名索引）我们不做。回一个合法的空索引而不是 404：
	// 部分 docker/containerd 版本在 404 时仍会解析正文，body 一空就报
	// "failed to decode referrers index: unexpected EOF"，整次 pull 失败。
	if isReferrersPath(r.URL.Path) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`))
		return
	}
	kind, repo, _, err := g.regAd.Route(r.URL.Path)
	if err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	// 通道开关按请求里的站前缀判：/v2/ghcr.io/... 走 GHCR 开关，
	// 其余（裸名或别的 registry）走 Docker 开关。Ping 不受影响，
	// 否则 docker 客户端连"能不能连"都判不出来。
	switch kind {
	case registryadapter.KindManifest, registryadapter.KindBlob:
		if msg, off := g.registryChannelOff(repo); off {
			http.Error(w, "404 "+msg, http.StatusNotFound)
			return
		}
	}
	switch kind {
	case registryadapter.KindPing:
		// docker 第一件事就是探这个；401 时它才会去 docker login
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}")

	case registryadapter.KindWrite:
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 镜像站只读，不支持 push/delete", http.StatusMethodNotAllowed)

	case registryadapter.KindManifest:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "405", http.StatusMethodNotAllowed)
			return
		}
		g.serveManifest(w, r)

	case registryadapter.KindBlob:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "405", http.StatusMethodNotAllowed)
			return
		}
		g.serveBlob(w, r)

	default:
		http.Error(w, "404", http.StatusNotFound)
	}
}

// isReferrersPath 判断 /v2/<repo>/referrers/<digest>。
// repo 名里本身可能带 referrers 段，所以要求它后面只剩一层。
func isReferrersPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/v2/")
	if !ok {
		return false
	}
	i := strings.LastIndex(rest, "/referrers/")
	if i <= 0 {
		return false
	}
	return !strings.Contains(rest[i+len("/referrers/"):], "/")
}

func (g *Gateway) serveManifest(w http.ResponseWriter, r *http.Request) {
	plan, err := g.regAd.Plan(r)
	if err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := g.checkTarget(plan.Upstream, r.Host); err != nil {
		code := statusFor(err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}

	e, _ := g.st.CacheGet(plan.CacheKey)
	// 用 plan 的 TTL 判断新鲜度：条目自己的 ttl 字段在历史数据里可能是 0，
	// 那样 tag 会永远被判过期，每次都去回源验证（实测 1.5s 起步）。
	fromCache := e != nil && g.freshFor(e, plan)
	if !fromCache {
		ne, notMod, ferr := g.fetchOnce(r.Context(), plan, e)
		if ferr != nil {
			code := statusFor(ferr)
			http.Error(w, fmt.Sprintf("%d 回源失败: %v", code, ferr), code)
			return
		}
		// 304：内容没变，把条目的时钟推一下，否则下一跳又被判过期，
		// 永远停在「每次都要回源验证」的状态里。
		if notMod {
			g.st.CacheRefresh(plan.CacheKey)
			fromCache = true
			if ne == nil {
				ne, _ = g.st.CacheGet(plan.CacheKey)
			}
		}
		if ne != nil {
			e = ne
		}
		g.enforceQuota()
	}
	setSource(w, plan)
	g.writeManifest(w, r, e, fromCache)
}

// fromCache=false 时字节已记 fill，再记 hit 会把每次未命中都算成命中。
func (g *Gateway) writeManifest(w http.ResponseWriter, r *http.Request, e *store.CacheEntry, fromCache bool) {
	if e == nil {
		http.Error(w, "502 清单缺失", http.StatusBadGateway)
		return
	}
	f, err := g.cache.Open(e.SHA256)
	if err != nil {
		_ = g.st.CacheDelete(e.Key)
		http.Error(w, "502 缓存文件丢失，请重试", http.StatusBadGateway)
		return
	}
	defer f.Close()

	h := w.Header()
	if e.ContentType != "" {
		h.Set("Content-Type", e.ContentType)
	} else {
		h.Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
	}
	// 客户端会拿这个头跟请求的 digest 比对
	h.Set("Docker-Content-Digest", "sha256:"+e.SHA256)
	h.Set("ETag", `"`+e.SHA256+`"`)
	h.Set("Content-Length", fmt.Sprint(e.Size))
	label := "HIT"
	if !fromCache {
		label = "MISS"
	}
	h.Set("X-ML-Cache", label)
	cw := &countWriter{ResponseWriter: w}
	if r.Method == http.MethodHead {
		cw.WriteHeader(http.StatusOK)
	} else if _, err := io.Copy(cw, f); err != nil {
		log.Printf("回写清单中断 %s: %v", e.Key, err)
	}
	if fromCache {
		g.st.CacheHit(e.Key)
		if cw.n > 0 {
			g.st.RecordTraffic(store.TrafficHit, store.KindDocker, cw.n)
		}
	}
}

func (g *Gateway) serveBlob(w http.ResponseWriter, r *http.Request) {
	plan, err := g.regAd.Plan(r)
	if err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := g.checkTarget(plan.Upstream, r.Host); err != nil {
		code := statusFor(err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}
	digest := "sha256:" + plan.ExpectSHA

	// digest 是内容哈希，命中就是永远命中，不用再回源验证
	if e, _ := g.st.CacheGet(plan.CacheKey); e != nil {
		// 文件能打开就走统一的 writeBlobFile：它负责头、命中计数和面板任务。
		if f, err := g.cache.Open(e.SHA256); err == nil {
			_ = f.Close()
			g.writeBlobFile(w, r, e, digest)
			return
		}
		_ = g.st.CacheDelete(e.Key)
	}

	// HEAD 只问大小和 digest，不下整份。
	// 但它必须和 GET 一样走多源：官方不可达而镜像可用时，
	// 客户端拿 manifest 前的探测请求不能先失败。
	if r.Method == http.MethodHead {
		g.headBlob(w, r, plan)
		return
	}

	// 客户端带 Range（containerd 断点续传）：直通不落盘。
	// 落盘路径只回整份 200，会白费客户端已下的一半字节。
	if r.Header.Get("Range") != "" {
		g.passThrough(w, r, plan)
		return
	}

	unlock := g.lockKey(plan.CacheKey)
	defer unlock()
	if e, _ := g.st.CacheGet(plan.CacheKey); e != nil {
		g.writeBlobFile(w, r, e, digest)
		return
	}
	g.fillStream(w, r, plan)
	g.enforceQuota()
}

func (g *Gateway) writeBlobFile(w http.ResponseWriter, r *http.Request, e *store.CacheEntry, digest string) {
	f, err := g.cache.Open(e.SHA256)
	if err != nil {
		_ = g.st.CacheDelete(e.Key)
		http.Error(w, "502 缓存文件丢失，请重试", http.StatusBadGateway)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Docker-Content-Digest", digest)
	h.Set("Accept-Ranges", "bytes")
	h.Set("X-ML-Cache", "HIT")
	g.st.CacheHit(e.Key)
	// 面板：docker pull 命中缓存也是真在传，得留一条任务，
	// 否则日常拉已经缓存的镜像时面板永远是空的。
	var task *liveTask
	if r.Method == http.MethodGet && e.Size >= liveMinBytes {
		task = g.tasks.start("docker", shortDigest(digest), "cache", e.Size, nil)
		defer task.finish(true)
	}
	cw := &countWriter{ResponseWriter: w, t: task, through: true}
	http.ServeContent(cw, r, path.Base(e.SHA256), g.now(), f)
	if cw.n > 0 {
		g.st.RecordTraffic(store.TrafficHit, store.KindDocker, cw.n)
	}
}

// headBlob 处理未命中缓存的 blob HEAD。
//
// HEAD 与 GET 共用 targets() 和 Registry Rewrite：候选源在前、官方垫底，
// 每个目标都过 checkTarget；成功/失败/429/503 都按 plan.Sched() 记进
// registry 族，不会落到 docker 或 ghcr 的健康维度。
// HEAD 没有 Body：任何分支都不写 Body，也不会在响应提交后换源。
func (g *Gateway) headBlob(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) {
	var (
		lastCode int
		lastHdr  http.Header
		lastErr  error
	)
	for _, t := range g.targets(plan, nil) {
		if err := g.checkTarget(t.u, r.Host); err != nil {
			// 不安全的目标直接跳过：不发请求，也不产生健康样本。
			log.Printf("HEAD 跳过目标 %s: %v", safeUpstream(t.u), logging.SafeErr(err))
			continue
		}
		start := time.Now()
		code, hdr, err := g.headOnce(r.Context(), plan, t.u)
		if err != nil {
			lastErr = err
			g.reportSource(plan.Sched(), t, false, time.Since(start), scheduler.Hint{Mode: "head"})
			continue
		}
		lastCode, lastHdr = code, hdr
		switch {
		case code == http.StatusOK:
			g.reportSource(plan.Sched(), t, true, time.Since(start),
				scheduler.Hint{Code: code, Mode: "head"})
			// HEAD 没有 Body，真实的外源流量只有响应头。
			g.st.RecordTraffic(store.TrafficBypass, store.KindOfKey(plan.CacheKey), headerBytes(hdr))
			g.writeHeadOK(w, plan, t, hdr)
			return
		case isAuthDenied(code):
			// 与 GET 同口径：401/403 是「没资格问」，换源也拿不到，
			// 直接把挑战头交给客户端去认证；不冷却（P2-2 待统一分类）。
			g.reportSource(plan.Sched(), t, true, time.Since(start),
				scheduler.Hint{Code: code, Mode: "head"})
			g.writeHeadStatus(w, hdr, code)
			return
		case isMiss(code):
			// 未命中不是源故障：只记事件，不冷却（与透传路径同口径）。
			metrics.SourceMiss(plan.Kind, code)
			g.reportSource(plan.Sched(), t, true, time.Since(start),
				scheduler.Hint{Code: code, Mode: "head"})
		default:
			g.reportSource(plan.Sched(), t, false, time.Since(start),
				scheduler.Hint{Code: code, Mode: "head", RetryAfter: retryAfterHeader(hdr)})
		}
	}

	// 所有目标都失败：把最后一次真实的上游状态交给客户端。
	if lastCode == 0 {
		code := statusFor(lastErr)
		log.Printf("HEAD 回源失败 %s: %v", safeUpstream(plan.Upstream), logging.SafeErr(lastErr))
		g.writeHeadStatus(w, nil, code)
		return
	}
	g.writeHeadStatus(w, lastHdr, lastCode)
}

// writeHeadStatus 交出一个非 200 的 HEAD 结果：只带认证/限流等必要头，
// 不写 Body，也不把上游错误响应的 Content-Length 当成资源大小。
func (g *Gateway) writeHeadStatus(w http.ResponseWriter, hdr http.Header, code int) {
	noStore(w.Header())
	copyHeadHeaders(w.Header(), hdr, "WWW-Authenticate", "Retry-After", "Docker-Distribution-Api-Version")
	w.Header().Set("X-ML-Cache", "MISS")
	w.WriteHeader(code)
}

// headOnce 发一次原生 HEAD：只到响应头，读完（最多 4K）并关闭 Body。
func (g *Gateway) headOnce(ctx context.Context, plan *adapter.Plan, u *url.URL) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "mirror-lens/"+Version+" (+https://github.com/zggnh12345/mirror-lens)")
	req.Header.Set("Accept-Encoding", "identity")
	for k, vs := range plan.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := g.cli.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, resp.Header.Clone(), nil
}

// writeHeadOK 只提交状态和头，不写 Body。
func (g *Gateway) writeHeadOK(w http.ResponseWriter, plan *adapter.Plan, t fetchTarget, hdr http.Header) {
	h := w.Header()
	copyHeadHeaders(h, hdr, "Content-Length", "Content-Type", "Docker-Content-Digest",
		"Docker-Distribution-Api-Version", "ETag", "Last-Modified",
		"Cache-Control", "Content-Disposition")
	if h.Get("Accept-Ranges") == "" {
		h.Set("Accept-Ranges", "bytes")
	}
	h.Set("X-ML-Cache", "MISS")
	if t.c != nil {
		plan.Source = t.c.Name
		h.Set("X-ML-Source", t.c.Name)
	}
	w.WriteHeader(http.StatusOK)
}

func copyHeadHeaders(dst, src http.Header, keys ...string) {
	if src == nil {
		return
	}
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

// headerBytes 估算响应头字节数：HEAD 的唯一上游流量就是这些头。
func headerBytes(h http.Header) int64 {
	var n int64
	for k, vs := range h {
		n += int64(len(k) + 2) // ": "
		for _, v := range vs {
			n += int64(len(v) + 2) // "\r\n"
		}
	}
	return n
}

// shortDigest 把 sha256:xxx 缩成面板一行放得下的形式。
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return "sha256:" + d
}
