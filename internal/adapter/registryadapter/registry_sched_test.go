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

// 开关关闭：逐字保持 3.4.6 的 Kind/SchedKey/OfficialOnly，不因为多源能力存在而改变。
func TestSchedMultiOriginOffUnchanged(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	a.MultiOrigin = false

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

// 开关打开：只有真正的 foreign、非 ghcr Host 进入独立调度族。
func TestSchedMultiOriginOn(t *testing.T) {
	a := New("registry-1.docker.io", 0, time.Minute)
	a.MultiOrigin = true

	cases := []struct {
		path, kind, key string
	}{
		{"/v2/quay.io/prometheus/node-exporter/manifests/v1.8.0", "registry", "registry:quay.io"},
		{"/v2/registry.k8s.io/pause/blobs/sha256:" + repeatHex(64), "registry", "registry:registry.k8s.io"},
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
	// 非白名单的伪 Host 仍被当成 Hub 仓库名，不能借此拿到 registry 调度身份。
	if kind, key, _, _ := schedOf(t, a, "/v2/evil.example/x/y/manifests/latest"); kind != "docker" || key != "" {
		t.Errorf("pseudo-host: kind=%q key=%q，期望 docker/\"\"", kind, key)
	}
}

func TestMultiOriginEnvExact(t *testing.T) {
	cases := map[string]bool{
		"": false, "0": false, "true": false, "yes": false, "01": false, "1": true, " 1 ": true,
	}
	for val, want := range cases {
		t.Setenv("ML_REGISTRY_MULTI_ORIGIN", val)
		if got := multiOriginFromEnv(); got != want {
			t.Errorf("值 %q 得到 %v，期望 %v", val, got, want)
		}
	}
	// 未设置
	t.Setenv("ML_REGISTRY_MULTI_ORIGIN", "")
	if multiOriginFromEnv() {
		t.Error("空值应保持关闭")
	}
}

func repeatHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
