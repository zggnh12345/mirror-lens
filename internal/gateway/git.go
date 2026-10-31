package gateway

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// ServeGit 处理 /git/<host>/<repo>.git/...
//
// 拉取是两段：GET info/refs 拿引用（小、可缓存几秒），POST git-upload-pack
// 拿 packfile（大、跟客户端协商结果强相关，直通不缓存）。
func (g *Gateway) ServeGit(w http.ResponseWriter, r *http.Request) {
	g.withAuth(func(w http.ResponseWriter, r *http.Request) { g.serveGit(w, r) })(w, r)
}

func (g *Gateway) serveGit(w http.ResponseWriter, r *http.Request) {
	plan, err := g.gitAd.Plan(r)
	if err != nil {
		// 路径不对是客户端的问题，别报成 502
		code := http.StatusBadRequest
		if errors.Is(err, adapter.ErrReadOnly) {
			code = http.StatusForbidden
		}
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}
	if err := g.checkTarget(plan.Upstream, r.Host); err != nil {
		code := statusFor(err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}

	// upload-pack 的请求体是客户端与服务端的协商（have/want），响应是 packfile。
	// 两份都不能缓冲：几百 MB 的包体，先落盘再转发等于把 clone 拖慢一倍。
	if r.Method == http.MethodPost {
		g.pipeGit(w, r, plan)
		return
	}

	if plan.Cacheable {
		if e, _ := g.st.CacheGet(plan.CacheKey); e != nil && g.fresh(e) {
			g.serveGitCached(w, r, e, "HIT", true)
			return
		}
		unlock := g.lockKey(plan.CacheKey)
		defer unlock()
		if e, _ := g.st.CacheGet(plan.CacheKey); e != nil && g.fresh(e) {
			g.serveGitCached(w, r, e, "HIT", true)
			return
		}
	}

	entry, _, err := g.fetch(r.Context(), plan, nil)
	if err != nil {
		code := statusFor(err)
		log.Printf("git info/refs 回源失败 %s: %v", plan.Upstream, err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}
	setSource(w, plan)
	g.serveGitCached(w, r, entry, "MISS", false)
}

// serveGitCached 和 serveCached 的区别只有 Cache-Control：
// info/refs 是「此刻的引用」，不能让中间层按 max-age 缓存，否则 clone 会拿到过期的 tip。
func (g *Gateway) serveGitCached(w http.ResponseWriter, r *http.Request, e *store.CacheEntry, label string, fromCache bool) {
	if e == nil {
		http.Error(w, "502 缓存条目缺失", http.StatusBadGateway)
		return
	}
	f, err := g.cache.Open(e.SHA256)
	if err != nil {
		log.Printf("缓存文件丢失 %s (%s): %v", e.Key, e.SHA256, err)
		_ = g.st.CacheDelete(e.Key)
		http.Error(w, "502 缓存文件丢失，请重试", http.StatusBadGateway)
		return
	}
	defer f.Close()

	if e.ContentType != "" {
		w.Header().Set("Content-Type", e.ContentType)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-ML-Cache", label)
	cw := &countWriter{ResponseWriter: w}
	if _, err := io.Copy(cw, f); err != nil {
		log.Printf("回写 git 响应中断 %s: %v", e.Key, err)
	}
	if fromCache {
		g.st.CacheHit(e.Key)
		if cw.n > 0 {
			g.st.RecordTraffic(store.TrafficHit, store.KindGit, cw.n)
		}
	}
}

func (g *Gateway) pipeGit(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, plan.Upstream.String(), r.Body)
	if err != nil {
		http.Error(w, "502 "+err.Error(), http.StatusBadGateway)
		return
	}
	// 原样把长度带上，否则上游会当成 chunked；Git 服务端两种都吃，但保持原样最稳
	req.ContentLength = r.ContentLength
	req.Header.Set("User-Agent", "mirror-lens/"+Version+" (+https://github.com/zggnh12345/mirror-lens)")
	req.Header.Set("Accept-Encoding", "identity")
	// Content-Encoding 必须一起转发：git 会把 fetch 请求体 gzip 后再发，
	// 只转字节不转这个头，上游会把它当明文解析并回 400。
	for _, k := range []string{"Content-Type", "Content-Encoding", "Accept", "Git-Protocol"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	for k, vs := range plan.Header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := g.cli.Do(req)
	if err != nil {
		code := statusFor(err)
		log.Printf("git upload-pack 失败 %s: %v", plan.Upstream, err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header, "Content-Type", "Content-Encoding")
	w.Header().Set("X-ML-Cache", "PASS")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端走了，上游连接跟着关掉
			}
			total += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			log.Printf("git upload-pack 传输中断 %s（已发 %d 字节）: %v", plan.Upstream, total, rerr)
			return
		}
	}
	g.st.RecordTraffic(store.TrafficBypass, store.KindGit, total)
}
