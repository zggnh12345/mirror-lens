package gateway

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"path"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// RegistryOn 告诉 api 层要不要挂 /v2/ 路由。
func (g *Gateway) RegistryOn() bool { return g.regAd != nil }

// ServeRegistry 处理 /v2/...，只读。
func (g *Gateway) ServeRegistry(w http.ResponseWriter, r *http.Request) {
	g.withAuth(func(w http.ResponseWriter, r *http.Request) { g.serveRegistry(w, r) })(w, r)
}

func (g *Gateway) serveRegistry(w http.ResponseWriter, r *http.Request) {
	if g.regAd == nil {
		http.Error(w, "404 Docker 加速未开启", http.StatusNotFound)
		return
	}
	kind, _, _, err := g.regAd.Route(r.URL.Path)
	if err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
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
	fromCache := e != nil
	if e == nil || (!plan.Immutable && !g.fresh(e)) {
		e, _, err = g.fetchOnce(r.Context(), plan, e)
		if err != nil {
			code := statusFor(err)
			http.Error(w, fmt.Sprintf("%d 回源失败: %v", code, err), code)
			return
		}
		fromCache = false
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
		f, err := g.cache.Open(e.SHA256)
		if err == nil {
			defer f.Close()
			h := w.Header()
			h.Set("Content-Type", "application/octet-stream")
			h.Set("Docker-Content-Digest", digest)
			h.Set("Accept-Ranges", "bytes")
			h.Set("X-ML-Cache", "HIT")
			g.st.CacheHit(e.Key)
			cw := &countWriter{ResponseWriter: w}
			http.ServeContent(cw, r, path.Base(e.SHA256), g.now(), f)
			if cw.n > 0 {
				g.st.RecordTraffic(store.TrafficHit, store.KindDocker, cw.n)
			}
			return
		}
		_ = g.st.CacheDelete(e.Key)
	}

	// HEAD 只问大小和 digest，不下整份
	if r.Method == http.MethodHead {
		g.headUpstream(w, r, plan)
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
	cw := &countWriter{ResponseWriter: w}
	http.ServeContent(cw, r, path.Base(e.SHA256), g.now(), f)
	if cw.n > 0 {
		g.st.RecordTraffic(store.TrafficHit, store.KindDocker, cw.n)
	}
}

func (g *Gateway) headUpstream(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodHead, plan.Upstream.String(), nil)
	if err != nil {
		http.Error(w, "502 "+err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range plan.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := g.cli.Do(req)
	if err != nil {
		code := statusFor(err)
		http.Error(w, fmt.Sprintf("%d %v", code, err), code)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("%d 上游 %s", resp.StatusCode, resp.Status), resp.StatusCode)
		return
	}
	h := w.Header()
	for _, k := range []string{"Content-Length", "Docker-Content-Digest", "Content-Type"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("Accept-Ranges", "bytes")
	h.Set("X-ML-Cache", "MISS")
	w.WriteHeader(http.StatusOK)
}
