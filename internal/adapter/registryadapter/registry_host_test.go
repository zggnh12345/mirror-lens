package registryadapter

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func plan(t *testing.T, path string) *planT {
	t.Helper()
	a := New("registry-1.docker.io", 0, time.Minute)
	r, _ := http.NewRequest(http.MethodGet, "http://gw"+path, nil)
	p, err := a.Plan(r)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return &planT{host: p.Upstream.Host, path: p.Upstream.Path, key: p.CacheKey, label: p.Label, official: p.OfficialOnly}
}

type planT struct {
	host, path, key, label string
	official               bool
}

// dpanel/1Panel 把完整镜像地址塞进路径：ghcr.io 这类前缀要跟着回源，
// 但只有白名单里的站才认，别的仍按 Docker Hub 仓库名处理（防 SSRF）。
func TestPlanRoutesRegistryHostPrefix(t *testing.T) {
	cases := []struct{ path, host, up string }{
		{"/v2/ghcr.io/stackia/rtp2httpd/manifests/latest", "ghcr.io", "/v2/stackia/rtp2httpd/manifests/latest"},
		{"/v2/quay.io/coreos/etcd/manifests/v3.5.0", "quay.io", "/v2/coreos/etcd/manifests/v3.5.0"},
		{"/v2/docker.io/library/nginx/manifests/latest", "registry-1.docker.io", "/v2/library/nginx/manifests/latest"},
		{"/v2/library/nginx/manifests/latest", "registry-1.docker.io", "/v2/library/nginx/manifests/latest"},
		// 不在白名单：不再静默当成 Docker Hub 仓库名
		// （见 TestPlanRejectsUnknownHostPrefix）
	}
	for _, c := range cases {
		got := plan(t, c.path)
		if got.host != c.host || got.path != c.up {
			t.Fatalf("%s → %s%s，期望 %s%s", c.path, got.host, got.path, c.host, c.up)
		}
		// ghcr.io 有镜像源，不再只回官方；其余 foreign 站点仍 OfficialOnly。
		official := c.host != "registry-1.docker.io" && c.host != "ghcr.io"
		if got.official != official {
			t.Fatalf("%s 的 OfficialOnly=%v，不对", c.path, got.official)
		}
		if got.key != "" && !contains(got.key, c.host) {
			t.Fatalf("%s 的缓存键没带站点：%s", c.path, got.key)
		}
	}
}

func TestHostAllowedDynamicWithNonNilHosts(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	// 生产装配可能把自定义 Hosts 设成非 nil；动态 Artifact Registry
	// 不能因为这张旧 map 里没有它就被重新关掉。
	a.Hosts = map[string]bool{"gcr.io": true}
	for _, h := range []string{"gcr.io", "us-docker.pkg.dev", "europe-west1-docker.pkg.dev"} {
		if !a.hostAllowed(h) {
			t.Errorf("%s 应通过", h)
		}
	}
	if a.hostAllowed("ghcr.io") {
		t.Error("自定义 Hosts 不能绕过全局白名单逻辑之外的任意 Host")
	}
	if a.hostAllowed("evil.example.com") {
		t.Error("未知 Host 不应通过")
	}
}

func TestParsePathSingleIdentity(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	cases := []struct {
		path       string
		kind       Kind
		host, name string
		ref        string
	}{
		{"/v2/gcr.io/foo/bar/manifests/latest", KindManifest, "gcr.io", "foo/bar", "latest"},
		{"/v2/us-docker.pkg.dev/foo/bar/manifests/latest", KindManifest, "us-docker.pkg.dev", "foo/bar", "latest"},
		{"/v2/foo/bar/manifests/latest", KindManifest, "registry-1.docker.io", "foo/bar", "latest"},
		{"/v2/gcr.io/foo/bar/blobs/sha256:" + strings.Repeat("a", 64), KindBlob, "gcr.io", "foo/bar", "sha256:" + strings.Repeat("a", 64)},
		{"/v2/gcr.io/foo/bar/referrers/sha256:" + strings.Repeat("b", 64), KindReferrers, "gcr.io", "foo/bar", "sha256:" + strings.Repeat("b", 64)},
		{"/v2/gcr.io/foo/bar/blobs/uploads/", KindWrite, "gcr.io", "foo/bar", ""},
	}
	for _, c := range cases {
		p, err := a.ParsePath(c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		if p.Kind != c.kind || p.Host != c.host || p.Name != c.name || p.Ref != c.ref {
			t.Errorf("%s → %+v，期望 %v/%s/%s/%s", c.path, p, c.kind, c.host, c.name, c.ref)
		}
	}
}

func TestBlobCacheKeySharedAcrossOrigins(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	digest := "sha256:" + strings.Repeat("d", 64)
	paths := []string{
		"/v2/gcr.io/foo/bar/blobs/" + digest,
		"/v2/registry.k8s.io/foo/bar/blobs/" + digest,
		"/v2/us-docker.pkg.dev/foo/bar/blobs/" + digest,
	}
	var first string
	for _, path := range paths {
		r, _ := http.NewRequest(http.MethodGet, "http://gw"+path, nil)
		p, err := a.Plan(r)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if p.CacheKey != "docker:blob:"+digest {
			t.Fatalf("%s 的 blob 键没按内容寻址: %s", path, p.CacheKey)
		}
		if p.ExpectSHA != strings.TrimPrefix(digest, "sha256:") {
			t.Fatalf("%s 未绑定 digest 校验", path)
		}
		if first == "" {
			first = p.CacheKey
		} else if p.CacheKey != first {
			t.Fatalf("跨 Host 的同一 digest 键不一致: %s vs %s", first, p.CacheKey)
		}
	}
}

func TestPlanRejectsUnknownHostPrefix(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	r, _ := http.NewRequest(http.MethodGet, "http://gw/v2/evil.example.com/a/b/manifests/latest", nil)
	if _, err := a.Plan(r); err == nil {
		t.Fatal("未知 Host 前缀应被拒绝，不能回落成 Docker Hub 仓库名")
	}
	if _, err := a.ParsePath("/v2/evil.example.com/a/b/manifests/latest"); err == nil {
		t.Fatal("ParsePath 也应拒绝未知 Host 前缀")
	}
}

// ghcr 对 schema1 / */* / 纯 docker 类型直接 404，老客户端重试时就发这些；
// 归一后必须都带上 OCI 类型，否则回源必挂。
func TestPlanNormalizesAccept(t *testing.T) {
	for _, acc := range []string{"*/*", "application/vnd.docker.distribution.manifest.v1+prettyjws", "",
		"application/vnd.docker.distribution.manifest.list.v2+json"} {
		r, _ := http.NewRequest(http.MethodGet, "http://gw/v2/ghcr.io/stackia/rtp2httpd/manifests/latest", nil)
		if acc != "" {
			r.Header.Set("Accept", acc)
		}
		p, err := New("registry-1.docker.io", 0, time.Minute).Plan(r)
		if err != nil {
			t.Fatal(err)
		}
		got := p.Header.Get("Accept")
		if !strings.Contains(got, "application/vnd.oci.image.index.v1+json") {
			t.Fatalf("Accept=%q 归一成 %q，缺 OCI 索引类型", acc, got)
		}
		if strings.Contains(got, "prettyjws") || strings.Contains(got, "*/*") {
			t.Fatalf("Accept=%q 归一成 %q，仍带无用类型", acc, got)
		}
	}
}

func TestPlanBlobKeepsHost(t *testing.T) {
	got := plan(t, "/v2/ghcr.io/stackia/rtp2httpd/blobs/sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if got.host != "ghcr.io" || got.path != "/v2/stackia/rtp2httpd/blobs/sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("blob 路径不对：%s%s", got.host, got.path)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ghcr 没有 library 命名空间：Docker Hub 官方镜像别挂 ghcr 备链。
// 挂了不仅白打 404，还会把主链拆成两条并行链，主链内部就没法对冲了。
func TestGhcrFallbackSkipsDockerHubOfficial(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	for _, c := range []struct {
		path string
		want int
	}{
		{"/v2/library/golang/manifests/1.23-alpine", 0},
		{"/v2/fluxcd/flux2/manifests/latest", 1},
	} {
		r, _ := http.NewRequest(http.MethodGet, "http://gw"+c.path, nil)
		p, err := a.Plan(r)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		if len(p.Fallbacks) != c.want {
			t.Fatalf("%s 备链 %d 条，期望 %d", c.path, len(p.Fallbacks), c.want)
		}
	}
}

// 记住备用站点后，同一 tag 的缓存键不能跟着源站变：否则这次命中
// 南京大学、下次命中凤凰城，会各算一个 key，每次都得回源。
func TestManifestCacheKeyStableAfterFallback(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	path := "/v2/star/portainer/manifests/latest"
	req := func() *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "http://gw"+path, nil)
		return r
	}
	first, err := a.Plan(req())
	if err != nil {
		t.Fatal(err)
	}
	a.rememberHost("star/portainer", "mirror-nju.example.com")
	second, err := a.Plan(req())
	if err != nil {
		t.Fatal(err)
	}
	if second.Upstream.Host != "mirror-nju.example.com" {
		t.Fatalf("第二次应换到记住的站点，实际 %s", second.Upstream.Host)
	}
	if first.CacheKey != second.CacheKey {
		t.Fatalf("换源后缓存键变了：%s → %s", first.CacheKey, second.CacheKey)
	}
}
