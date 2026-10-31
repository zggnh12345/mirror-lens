package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// CDN 边缘缓存策略（阶段 8）。
//
// 这里只做一件事：把「这份内容能缓存多久」用标准头说清楚，让任何一层共享缓存
// （Cloudflare、Fastly、Varnish、nginx proxy_cache）都能照着做。不自建 CDN，
// 也不调用任何厂商 API——换 CDN 不用改代码。
//
// 三条判断依据，按可信度从高到低：
//  1. 按内容摘要寻址的（docker blob）永不改变 → 一年 + immutable，缓存多久都对。
//  2. 按标签/路径寻址但会变的（docker manifest、release 资产、raw 文件）→ 跟随
//     条目 TTL，并且不超过 cdn.max_age 的上限，过期就得回源问一次。
//  3. 引用类响应（git info/refs）→ no-cache，由 serveGitCached 单独处理。
const (
	// immutableTTL 是 entryTTL 给「内容不可变」条目写的值（10 年）。超过这个
	// 门槛的条目一定是摘要寻址的，可以直接按 immutable 发。
	immutableTTL = int64(10 * 365 * 24 * 3600)
	// blobMaxAge 是摘要寻址内容的边缘 TTL：一年。RFC 9111 允许更久，一年够用。
	blobMaxAge = 365 * 24 * 3600
	// manifestMaxAge 给 tag 寻址的 manifest。tag 随时可能被重新推送，不能长缓存。
	manifestMaxAge = 60
	// staleWindow 让边缘在回源失败时还能顶一阵，和网关自己的 STALE 行为对齐。
	staleWindow = 86400
)

// cacheHeaders 按条目类型写 Cache-Control / Vary / Age / 缓存标签。
// 调用方已经写好 Content-Type 和 ETag，这里只管缓存语义。
func (g *Gateway) cacheHeaders(h http.Header, e *store.CacheEntry, now time.Time) {
	if e == nil {
		return
	}
	age := now.Unix() - e.CreatedAt
	if age < 0 {
		age = 0
	}

	switch {
	case strings.HasPrefix(e.Key, "docker:blob:"):
		// 摘要就是内容本身，字节永远不会变。
		h.Set("Cache-Control", "public, max-age="+strconv.Itoa(blobMaxAge)+", immutable")
	case strings.HasPrefix(e.Key, "docker:manifest:"):
		// manifest 的缓存键里带了 Accept，边缘必须按 Accept 分开存，
		// 否则会把 manifest list 发给要单架构镜像的客户端。
		h.Set("Cache-Control", "public, max-age="+strconv.Itoa(manifestMaxAge))
		h.Set("Vary", "Accept")
	case e.TTL >= immutableTTL:
		h.Set("Cache-Control", "public, max-age="+strconv.Itoa(blobMaxAge)+", immutable")
	default:
		ttl := e.TTL - age
		if ttl < 0 {
			ttl = 0
		}
		if g.cdn.Enabled {
			// 只在开了 CDN 时才把边缘 TTL 放到条目 TTL：默认 60 秒是给
			// 浏览器和中间缓存的安全值，放开要用户明确同意。
			if g.cdn.MaxAge > 0 && int64(g.cdn.MaxAge) < ttl {
				ttl = int64(g.cdn.MaxAge)
			}
			h.Set("Cache-Control", "public, max-age="+strconv.FormatInt(ttl, 10)+
				", stale-while-revalidate=60, stale-if-error="+strconv.Itoa(staleWindow))
		} else {
			if ttl > 60 {
				ttl = 60
			}
			h.Set("Cache-Control", "public, max-age="+strconv.FormatInt(ttl, 10))
		}
	}

	if age > 0 {
		// Age 告诉下游「我这份已经放了多久」，边缘据此扣减自己的 TTL。
		h.Set("Age", strconv.FormatInt(age, 10))
	}
	if g.cdn.Enabled {
		// 按标签清理用：Cloudflare 认 Cache-Tag，Fastly/Varnish 认 Surrogate-Key。
		// 只按类型打标，不按 URL——标签基数必须是封闭集合。
		tag := "ml-" + store.KindOfKey(e.Key)
		h.Set("Cache-Tag", tag)
		h.Set("Surrogate-Key", tag)
	}
}

// cacheHeadersPlan 给流式响应套同一套缓存语义。
//
// 流式填充在写响应头的时候内容还没落盘，没有 CacheEntry 可查；但边缘缓存
// 恰恰最需要大文件的那份头。按 plan 的缓存键和 TTL 生成等价的条目即可——
// 唯一的差别是没有 Age（刚生成，本来就是 0）。
func (g *Gateway) cacheHeadersPlan(h http.Header, plan *adapter.Plan) {
	if plan == nil {
		return
	}
	now := g.now()
	e := &store.CacheEntry{Key: plan.CacheKey, TTL: entryTTL(plan), CreatedAt: now.Unix()}
	g.cacheHeaders(h, e, now)
}

// noStore 用于错误响应：401/403/5xx 绝不能进共享缓存，否则一次故障会被
// 边缘放大成所有人的故障。
func noStore(h http.Header) {
	h.Set("Cache-Control", "no-store")
}
