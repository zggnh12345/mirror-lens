package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

func headersFor(g *Gateway, e *store.CacheEntry, now time.Time) http.Header {
	h := http.Header{}
	g.cacheHeaders(h, e, now)
	return h
}

func TestCacheHeadersDigestBlobIsImmutable(t *testing.T) {
	g := &Gateway{cdn: CDNOptions{Enabled: true, MaxAge: 300}}
	now := time.Unix(1_800_000_000, 0)
	e := &store.CacheEntry{
		Key:       "docker:blob:sha256:abc",
		TTL:       immutableTTL,
		CreatedAt: now.Unix() - 3600,
	}
	h := headersFor(g, e, now)
	if got := h.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("blob Cache-Control = %q", got)
	}
	if got := h.Get("Age"); got != "3600" {
		t.Fatalf("Age = %q, want 3600", got)
	}
	if got := h.Get("Cache-Tag"); got != "ml-docker" {
		t.Fatalf("Cache-Tag = %q", got)
	}
	// 摘要内容不该带 Vary：不同 Accept 拿到的字节完全一样。
	if got := h.Get("Vary"); got != "" {
		t.Fatalf("blob 不该有 Vary，得到 %q", got)
	}
}

func TestCacheHeadersManifestVariesOnAccept(t *testing.T) {
	g := &Gateway{cdn: CDNOptions{Enabled: true}}
	now := time.Unix(1_800_000_000, 0)
	e := &store.CacheEntry{
		Key:       "docker:manifest:registry-1.docker.io/library/nginx:latest|accept=...",
		TTL:       300,
		CreatedAt: now.Unix(),
	}
	h := headersFor(g, e, now)
	if got := h.Get("Vary"); got != "Accept" {
		t.Fatalf("manifest Vary = %q, want Accept", got)
	}
	// tag 会被重新推送，manifest 绝不能按 immutable 发。
	if got := h.Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("manifest Cache-Control = %q", got)
	}
}

func TestCacheHeadersShortTTLWhenCDNDisabled(t *testing.T) {
	g := &Gateway{}
	now := time.Unix(1_800_000_000, 0)
	e := &store.CacheEntry{
		Key:       "gh:BurntSushi/ripgrep@v14.1.1/tool.tar.gz",
		TTL:       24 * 3600,
		CreatedAt: now.Unix() - 30,
	}
	h := headersFor(g, e, now)
	// 没开 CDN：沿用原来的保守值，最多 60 秒。
	if got := h.Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q, want max-age=60", got)
	}
	if got := h.Get("Cache-Tag"); got != "" {
		t.Fatalf("没开 CDN 不该发 Cache-Tag，得到 %q", got)
	}
	if got := h.Get("Age"); got != "30" {
		t.Fatalf("Age = %q, want 30", got)
	}
}

func TestCacheHeadersCDNCapsMaxAge(t *testing.T) {
	g := &Gateway{cdn: CDNOptions{Enabled: true, MaxAge: 300}}
	now := time.Unix(1_800_000_000, 0)
	e := &store.CacheEntry{
		Key:       "gh:BurntSushi/ripgrep@v14.1.1/tool.tar.gz",
		TTL:       24 * 3600,
		CreatedAt: now.Unix() - 100,
	}
	h := headersFor(g, e, now)
	// 条目还能放 86300 秒，但边缘只允许 300 秒。
	if got := h.Get("Cache-Control"); got != "public, max-age=300, stale-while-revalidate=60, stale-if-error=86400" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := h.Get("Surrogate-Key"); got != "ml-gh" {
		t.Fatalf("Surrogate-Key = %q", got)
	}
}

func TestCacheHeadersExpiredEntryGetsZero(t *testing.T) {
	g := &Gateway{cdn: CDNOptions{Enabled: true, MaxAge: 300}}
	now := time.Unix(1_800_000_000, 0)
	e := &store.CacheEntry{
		Key:       "raw:o/r/main/a.txt",
		TTL:       60,
		CreatedAt: now.Unix() - 600,
	}
	h := headersFor(g, e, now)
	if got := h.Get("Cache-Control"); got != "public, max-age=0, stale-while-revalidate=60, stale-if-error=86400" {
		t.Fatalf("过期条目 Cache-Control = %q", got)
	}
}

func TestNoStoreOnUnauthorized(t *testing.T) {
	g := &Gateway{requireToken: true}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/raw/o/r/main/a.txt", nil)
	called := false
	g.withAuth(func(http.ResponseWriter, *http.Request) { called = true })(rec, req)
	if called {
		t.Fatal("没有凭据却放行了")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("401 的 Cache-Control = %q, want no-store", got)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}
