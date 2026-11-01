package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// CDN 边缘缓存语义：只用标准头说清「能缓存多久」，不绑定任何厂商 API。
// 摘要寻址（docker blob）永不改变 → 一年 + immutable；
// 标签/路径寻址（manifest、release、raw）→ 条目 TTL 且不超 cdn.max_age；
// 引用类（git info/refs）→ no-cache，由 serveGitCached 处理。
const (
	// immutableTTL 是 entryTTL 给不可变条目写的值；超过它必是摘要寻址。
	immutableTTL = int64(10 * 365 * 24 * 3600)
	// blobMaxAge：摘要寻址内容的边缘 TTL，一年（RFC 9111 允许更久）。
	blobMaxAge = 365 * 24 * 3600
	// manifestMaxAge：tag 会被重新推送，manifest 不能长缓存。
	manifestMaxAge = 60
	// staleWindow 让边缘在回源失败时顶一阵，和 STALE 行为对齐。
	staleWindow = 86400
)

// cacheHeaders 只管缓存语义（Cache-Control/Vary/Age/标签）。
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
		// manifest 缓存键带了 Accept，边缘必须按 Accept 分开存，
		// 否则会把 manifest list 发给要单架构的客户端。
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
		// 按标签清理用：Cloudflare 认 Cache-Tag，
		// Fastly/Varnish 认 Surrogate-Key。
		// 只按类型打标，不按 URL——标签基数必须是封闭集合。
		tag := "ml-" + store.KindOfKey(e.Key)
		h.Set("Cache-Tag", tag)
		h.Set("Surrogate-Key", tag)
	}
}

// cacheHeadersPlan 用 plan 造等价条目：流式写头时还没落盘、查不到 CacheEntry，
// 但边缘最需要大文件那份头。差别只是没有 Age（刚生成本来就是 0）。
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
