package registryadapter

import (
	"net/http"
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
		// 不在白名单：整段都当 Docker Hub 仓库名，回源站不变
		{"/v2/evil.example.com/a/b/manifests/latest", "registry-1.docker.io", "/v2/evil.example.com/a/b/manifests/latest"},
	}
	for _, c := range cases {
		got := plan(t, c.path)
		if got.host != c.host || got.path != c.up {
			t.Fatalf("%s → %s%s，期望 %s%s", c.path, got.host, got.path, c.host, c.up)
		}
		if got.official != (c.host != "registry-1.docker.io") {
			t.Fatalf("%s 的 OfficialOnly=%v，不对", c.path, got.official)
		}
		if got.key != "" && !contains(got.key, c.host) {
			t.Fatalf("%s 的缓存键没带站点：%s", c.path, got.key)
		}
	}
}

// ghcr 对 schema1 / */* 直接 404，老客户端重试时就发这些；归一后照样能取到。
func TestPlanNormalizesAccept(t *testing.T) {
	for _, acc := range []string{"*/*", "application/vnd.docker.distribution.manifest.v1+prettyjws", "", "application/vnd.oci.image.index.v1+json"} {
		r, _ := http.NewRequest(http.MethodGet, "http://gw/v2/ghcr.io/stackia/rtp2httpd/manifests/latest", nil)
		if acc != "" {
			r.Header.Set("Accept", acc)
		}
		p, err := New("registry-1.docker.io", 0, time.Minute).Plan(r)
		if err != nil {
			t.Fatal(err)
		}
		want := DefaultAccept
		if acc == "application/vnd.oci.image.index.v1+json" {
			want = acc
		}
		if got := p.Header.Get("Accept"); got != want {
			t.Fatalf("Accept=%q 归一成了 %q", acc, got)
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
