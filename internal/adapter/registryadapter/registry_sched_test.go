package registryadapter

import (
	"net/http"
	"testing"
	"time"
)

func schedOf(t *testing.T, a *Adapter, path string) (kind, schedKey string, official bool, up string) {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://gw"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Plan(r)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return p.Kind, p.SchedKey, p.OfficialOnly, p.Upstream.Path
}

// 空范围：逐字保持 3.4.6 的 Kind/SchedKey/OfficialOnly，不因为多源能力存在而改变。
func TestSchedEmptyScopeUnchanged(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	a.MultiOriginHosts = nil

	cases := []struct {
		path, kind, up string
		official       bool
	}{
		{"/v2/library/nginx/manifests/latest", "docker", "/v2/library/nginx/manifests/latest", false},
		{"/v2/quay.io/prometheus/node-exporter/manifests/v1.8.0", "docker", "/v2/prometheus/node-exporter/manifests/v1.8.0", true},
		{"/v2/ghcr.io/owner/repo/manifests/latest", "ghcr", "/v2/owner/repo/manifests/latest", false},
	}
	for _, tc := range cases {
		kind, key, official, up := schedOf(t, a, tc.path)
		if kind != tc.kind || key != "" || official != tc.official || up != tc.up {
			t.Errorf("%s: kind=%q key=%q official=%v up=%q，期望 %q/\"\"/%v/%q",
				tc.path, kind, key, official, up, tc.kind, tc.official, tc.up)
		}
	}
}

// 范围列表打开：只有 MultiOriginHosts 范围内的 foreign、非 ghcr Host 进入独立调度族。
func TestSchedMultiOriginOn(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	a.MultiOriginHosts = []string{
		"gcr.io", "registry.k8s.io", "*.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	}

	cases := []struct {
		path, kind, key string
	}{
		{"/v2/quay.io/prometheus/node-exporter/manifests/v1.8.0", "registry", "registry:quay.io"},
		{"/v2/registry.k8s.io/pause/blobs/sha256:" + repeatHex(64), "registry", "registry:registry.k8s.io"},
		{"/v2/gcr.io/google-containers/pause/manifests/3.9", "registry", "registry:gcr.io"},
		{"/v2/us-docker.pkg.dev/library/pause/manifests/latest", "registry", "registry:us-docker.pkg.dev"},
		{"/v2/mcr.microsoft.com/dotnet/runtime/manifests/8.0", "registry", "registry:mcr.microsoft.com"},
		{"/v2/registry.gitlab.com/group/project/manifests/latest", "registry", "registry:registry.gitlab.com"},
		{"/v2/public.ecr.aws/nginx/nginx/manifests/latest", "registry", "registry:public.ecr.aws"},
		{"/v2/cgr.dev/chainguard/static/manifests/latest", "registry", "registry:cgr.dev"},
	}
	for _, tc := range cases {
		kind, key, official, _ := schedOf(t, a, tc.path)
		if kind != tc.kind || key != tc.key || official {
			t.Errorf("%s: kind=%q key=%q official=%v，期望 %q/%q/false", tc.path, kind, key, official, tc.kind, tc.key)
		}
	}

	// ghcr 维持原独立处理，不被并入新族。
	if kind, key, _, _ := schedOf(t, a, "/v2/ghcr.io/owner/repo/manifests/latest"); kind != "ghcr" || key != "" {
		t.Errorf("ghcr: kind=%q key=%q，期望 ghcr/\"\"", kind, key)
	}
	// Docker Hub 裸名不变。
	if kind, key, official, _ := schedOf(t, a, "/v2/library/nginx/manifests/latest"); kind != "docker" || key != "" || official {
		t.Errorf("hub: kind=%q key=%q official=%v，期望 docker/\"\"/false", kind, key, official)
	}
}

// 不在范围时保持旧行为，不能顺手激活 Quay/MCR/LSCR。
func TestSchedMultiOriginScope(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	a.MultiOriginHosts = []string{"gcr.io", "registry.k8s.io", "*.pkg.dev"}

	kind, key, official, _ := schedOf(t, a, "/v2/quay.io/prometheus/node-exporter/manifests/v1.8.0")
	if kind != "docker" || key != "" || !official {
		t.Fatalf("范围外 quay: kind=%q key=%q official=%v，期望 docker/\"\"/true", kind, key, official)
	}
	// 空范围表示没有 Host 使用多源。
	a.MultiOriginHosts = nil
	kind, key, official, _ = schedOf(t, a, "/v2/gcr.io/google-containers/pause/manifests/3.9")
	if kind != "docker" || key != "" || !official {
		t.Fatalf("空范围 gcr: kind=%q key=%q official=%v，期望 docker/\"\"/true", kind, key, official)
	}
}

func TestSchedKeysIsolatedPerHost(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	a.MultiOriginHosts = []string{
		"gcr.io", "registry.k8s.io", "*.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	}
	paths := map[string]string{
		"gcr.io":              "/v2/gcr.io/foo/bar/manifests/latest",
		"registry.k8s.io":     "/v2/registry.k8s.io/foo/bar/manifests/latest",
		"us-docker.pkg.dev":   "/v2/us-docker.pkg.dev/foo/bar/manifests/latest",
		"quay.io":             "/v2/quay.io/foo/bar/manifests/latest",
		"mcr.microsoft.com":   "/v2/mcr.microsoft.com/foo/bar/manifests/latest",
		"registry.gitlab.com": "/v2/registry.gitlab.com/foo/bar/manifests/latest",
		"public.ecr.aws":      "/v2/public.ecr.aws/foo/bar/manifests/latest",
		"cgr.dev":             "/v2/cgr.dev/foo/bar/manifests/latest",
	}
	seen := map[string]bool{}
	for host, path := range paths {
		_, key, _, _ := schedOf(t, a, path)
		if key != "registry:"+host {
			t.Fatalf("%s 的调度键=%q", host, key)
		}
		if seen[key] {
			t.Fatalf("调度键重复: %s", key)
		}
		seen[key] = true
	}
}

func TestLscrNotInFirstBatchScope(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	a.MultiOriginHosts = []string{
		"gcr.io", "registry.k8s.io", "*.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	}
	kind, key, official, _ := schedOf(t, a, "/v2/lscr.io/linuxserver/nginx/manifests/latest")
	if kind != "docker" || key != "" || !official {
		t.Fatalf("lscr.io 暂不应进入多源: kind=%q key=%q official=%v", kind, key, official)
	}
}

func TestArtifactRegistryScopeRejectsLookalikes(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	a.MultiOriginHosts = []string{"*.pkg.dev"}
	for _, h := range []string{"us-docker.pkg.dev", "europe-west1-docker.pkg.dev"} {
		if !a.multiOriginHost(h) {
			t.Errorf("%s 应在 Artifact Registry 范围内", h)
		}
	}
	for _, h := range []string{"foo.pkg.dev", "a.b-docker.pkg.dev", "foo--docker.pkg.dev"} {
		if a.multiOriginHost(h) {
			t.Errorf("%s 不应在 Artifact Registry 范围内", h)
		}
	}
}

func repeatHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
