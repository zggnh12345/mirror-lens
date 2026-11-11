package scheduler

import (
	"net/url"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 两种已验证路径各自生成，不允许互相兜底。
func TestRewriteRegistryPaths(t *testing.T) {
	var s Scheduler
	base := mustURL(t, "https://quay.io/v2/prometheus/node-exporter/manifests/v1.8.0")
	cases := []struct {
		name string
		kind string
		cand Candidate
		want string
		ok   bool
	}{
		{
			name: "native 原样保留 /v2 路径",
			kind: "registry:quay.io",
			cand: Candidate{BaseURL: "https://mirror-a.example", RegistryPath: "native"},
			want: "https://mirror-a.example/v2/prometheus/node-exporter/manifests/v1.8.0",
			ok:   true,
		},
		{
			name: "host_prefix 插入 Host",
			kind: "registry:quay.io",
			cand: Candidate{BaseURL: "https://mirror-b.example", RegistryPath: "host_prefix"},
			want: "https://mirror-b.example/v2/quay.io/prometheus/node-exporter/manifests/v1.8.0",
			ok:   true,
		},
		{
			name: "空路径拒绝",
			kind: "registry:quay.io",
			cand: Candidate{BaseURL: "https://mirror-a.example"},
			ok:   false,
		},
		{
			name: "未知路径拒绝",
			kind: "registry:quay.io",
			cand: Candidate{BaseURL: "https://mirror-a.example", RegistryPath: "guess"},
			ok:   false,
		},
		{
			name: "kind 的 Host 与 base 不一致拒绝",
			kind: "registry:registry.k8s.io",
			cand: Candidate{BaseURL: "https://mirror-a.example", RegistryPath: "host_prefix"},
			ok:   false,
		},
		{
			name: "裸 registry 拒绝",
			kind: "registry",
			cand: Candidate{BaseURL: "https://mirror-a.example", RegistryPath: "native"},
			ok:   false,
		},
		{
			name: "空 Host 拒绝",
			kind: "registry:",
			cand: Candidate{BaseURL: "https://mirror-a.example", RegistryPath: "native"},
			ok:   false,
		},
		{
			name: "候选地址非法拒绝",
			kind: "registry:quay.io",
			cand: Candidate{BaseURL: "://bad", RegistryPath: "native"},
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := s.Rewrite(tc.kind, base, tc.cand)
			if ok != tc.ok {
				t.Fatalf("ok=%v，期望 %v（got=%v）", ok, tc.ok, got)
			}
			if ok && got.String() != tc.want {
				t.Errorf("URL=%q，期望 %q", got.String(), tc.want)
			}
		})
	}
}

// host_prefix 必须保留 base 的 query（如 tag 列表分页参数）。
func TestRewriteRegistryKeepsQuery(t *testing.T) {
	var s Scheduler
	base := mustURL(t, "https://quay.io/v2/prometheus/node-exporter/tags/list?n=100&last=abc")
	got, ok := s.Rewrite("registry:quay.io", base, Candidate{BaseURL: "https://mirror-b.example", RegistryPath: "host_prefix"})
	if !ok {
		t.Fatal("host_prefix 应可改写")
	}
	if got.RawQuery != "n=100&last=abc" {
		t.Errorf("query=%q 丢失或改变", got.RawQuery)
	}
	if got.Path != "/v2/quay.io/prometheus/node-exporter/tags/list" {
		t.Errorf("path=%q", got.Path)
	}
}

// 旧的 docker/ghcr 改写不能受新分支影响。
func TestRewriteLegacyUnchanged(t *testing.T) {
	var s Scheduler
	base := mustURL(t, "https://registry-1.docker.io/v2/library/nginx/manifests/latest")
	got, ok := s.Rewrite("docker", base, Candidate{BaseURL: "https://mirror-a.example"})
	if !ok || got.String() != "https://mirror-a.example/v2/library/nginx/manifests/latest" {
		t.Fatalf("docker 改写被改变: ok=%v url=%v", ok, got)
	}
	base = mustURL(t, "https://ghcr.io/v2/owner/repo/manifests/latest")
	got, ok = s.Rewrite("ghcr", base, Candidate{BaseURL: "https://mirror-a.example", GhcrPrefix: true})
	if !ok || got.Path != "/v2/ghcr.io/owner/repo/manifests/latest" {
		// 现有实现：/v2 + TrimPrefix("/v2/<rest>","/v2") = /v2/ghcr.io + /v2/<rest>
		t.Fatalf("ghcr 改写被改变: ok=%v path=%q", ok, got.Path)
	}
}
