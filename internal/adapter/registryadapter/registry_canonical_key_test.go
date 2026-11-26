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

// TestLocalUpstreamWithCacheOrigin 锁定 3.5.24 测试缝：本地 httptest 上游
// （127.0.0.1:port）不能作为逻辑缓存身份（registryhost.Normalize 会拒绝
// IP+端口）；测试专用字段 CacheOrigin 提供合法域名身份给 cache-key，
// 回源 host 与调度身份保持本地地址原样。
//
// 这是 gateway 34 个基准失败的根因修复规格：修复前 PlanParsed 直接
// Normalize(127.0.0.1:port) 报 ErrBadPath，RED；修复后规划成功、回源 host
// 仍是本地地址、cache-key 用 CacheOrigin 规范域名身份，GREEN。
func TestLocalUpstreamWithCacheOrigin(t *testing.T) {
	const localHost = "127.0.0.1:38241"
	const cacheOrigin = "registry.test"

	// 不注入 CacheOrigin 时，本地地址作逻辑身份必须被拒绝（冻结地址模型）。
	a0 := New(localHost, 0, time.Minute)
	a0.Scheme = "http"
	r, _ := http.NewRequest(http.MethodGet, "http://gw/v2/library/alpine/manifests/latest", nil)
	r.Header.Set("Accept", dhTestAccept)
	if p, err := a0.Plan(r); err == nil {
		t.Fatalf("未注入 CacheOrigin 时本地地址应被拒，却规划成功：%+v", p)
	}

	// 注入 CacheOrigin 后：规划成功、回源 host 不变、cache-key 用合法域名身份。
	a := New(localHost, 0, time.Minute)
	a.Scheme = "http"
	a.CacheOrigin = cacheOrigin
	p, err := a.Plan(r)
	if err != nil {
		t.Fatalf("注入 CacheOrigin 后应规划成功: %v", err)
	}
	if p.Upstream.Host != localHost {
		t.Errorf("回源 Host=%q，期望保持本地地址 %q", p.Upstream.Host, localHost)
	}
	wantPrefix := "docker:manifest:http://registry.test:80/library/alpine:latest|"
	if !strings.HasPrefix(p.CacheKey, wantPrefix) {
		t.Errorf("CacheKey=%q，期望规范域名身份前缀 %q", p.CacheKey, wantPrefix)
	}
	if strings.Contains(p.CacheKey, localHost) {
		t.Errorf("CacheKey=%q 不应含本地回源地址 %q", p.CacheKey, localHost)
	}
}
