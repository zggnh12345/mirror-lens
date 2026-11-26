package registryadapter

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const dhTestAccept = "application/vnd.docker.distribution.manifest.v2+json"

// TestManifestCacheKeyDockerHubCanonicalHost 锁定 §5.6：Docker Hub 的逻辑缓存
// 身份必须是规范化后的 CanonicalHost=docker.io，而不是实际回源路由 Host
// registry-1.docker.io。三种合法入口（裸名 / docker.io 前缀 / registry-1 前缀）
// 必须产生同一个 Manifest CacheKey，否则同一 tag 换入口就丢命中。
//
// P2-1：registry.go:343 之前直接把 ParsedPath.Host（=registry-1.docker.io）
// 传给 ManifestCacheKey，绕过 registryhost.Normalize，产生非规范身份。
func TestManifestCacheKeyDockerHubCanonicalHost(t *testing.T) {
	paths := []string{
		"/v2/library/nginx/manifests/latest",
		"/v2/docker.io/library/nginx/manifests/latest",
		"/v2/registry-1.docker.io/library/nginx/manifests/latest",
	}
	// 路由身份（实际回源 Host）保持 registry-1.docker.io，不能被缓存身份
	// 规范化影响；否则会破坏 rememberedHost / GHCR fallback 的调度。
	const routeHost = "registry-1.docker.io"
	// 缓存身份用规范 CanonicalHost=docker.io。
	const canonicalPrefix = "https://docker.io:443/library/nginx:latest|"

	var first string
	a := New(routeHost, 0, time.Minute)
	for _, path := range paths {
		r, _ := http.NewRequest(http.MethodGet, "http://gw"+path, nil)
		r.Header.Set("Accept", dhTestAccept)
		p, err := a.Plan(r)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if p.Upstream.Host != routeHost {
			t.Errorf("%s: 回源 Host=%q，期望保持 %q", path, p.Upstream.Host, routeHost)
		}
		if !strings.HasPrefix(p.CacheKey, "docker:manifest:"+canonicalPrefix) {
			t.Errorf("%s: CacheKey=%q，期望规范 CanonicalHost 前缀 %q",
				path, p.CacheKey, "docker:manifest:"+canonicalPrefix)
		}
		if strings.Contains(p.CacheKey, routeHost) {
			t.Errorf("%s: CacheKey=%q 含非规范回源 Host %q", path, p.CacheKey, routeHost)
		}
		if first == "" {
			first = p.CacheKey
		} else if p.CacheKey != first {
			t.Errorf("%s: CacheKey=%q，与首个入口 %q 不一致", path, p.CacheKey, first)
		}
	}
}

// digest 拉清单走同一个 CacheKey 构造点，规范身份同样必须是 docker.io。
// 这里额外锁定 immutable SHA 入口，防止将来有人只对 tag 分支规范化。
func TestManifestCacheKeyDockerHubDigestCanonicalHost(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	a := New("registry-1.docker.io", 0, time.Minute)
	r, _ := http.NewRequest(http.MethodGet,
		"http://gw/v2/library/nginx/manifests/"+digest, nil)
	r.Header.Set("Accept", dhTestAccept)
	p, err := a.Plan(r)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Immutable || p.ExpectSHA != strings.TrimPrefix(digest, "sha256:") {
		t.Fatalf("digest plan 未标记为 immutable：Immutable=%v ExpectSHA=%q", p.Immutable, p.ExpectSHA)
	}
	want := "docker:manifest:https://docker.io:443/library/nginx:" + digest + "|"
	if !strings.HasPrefix(p.CacheKey, want) {
		t.Errorf("CacheKey=%q，期望规范 CanonicalHost 前缀 %q", p.CacheKey, want)
	}
	if strings.Contains(p.CacheKey, "registry-1.docker.io") {
		t.Errorf("CacheKey=%q 含非规范回源 Host", p.CacheKey)
	}
}
