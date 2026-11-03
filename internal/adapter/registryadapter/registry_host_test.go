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
	return &planT{host: p.Upstream.Host, path: p.Upstream.Path, key: p.CacheKey, label: p.Label}
}

type planT struct{ host, path, key, label string }

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
		if got.key != "" && !contains(got.key, c.host) {
			t.Fatalf("%s 的缓存键没带站点：%s", c.path, got.key)
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
