package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// §15.3 精确键优先：同 Host 同时有精确键和 *.pkg.dev 通配键时精确键胜出。
func TestG4ResolveExactBeforeWildcard(t *testing.T) {
	r := NewHostProbeRunner(HostProbeOptions{Repos: map[string]string{
		"us-docker.pkg.dev": "exact/repo:tag",
		"*.pkg.dev":         "wildcard/repo:tag",
	}})
	got, ok := r.resolveProbeRepo("us-docker.pkg.dev")
	if !ok || got != "exact/repo:tag" {
		t.Fatalf("精确键应优先，得到 (%q,%v)", got, ok)
	}
	got, ok = r.resolveProbeRepo("europe-west1-docker.pkg.dev")
	if !ok || got != "wildcard/repo:tag" {
		t.Fatalf("未命中精确键应回退通配，得到 (%q,%v)", got, ok)
	}
}

// §15.3 相似后缀负向：evil-docker.pkg.dev.attacker.example 必须拒绝。
func TestG4ResolveArtifactSuffixNegative(t *testing.T) {
	r := NewHostProbeRunner(HostProbeOptions{Repos: map[string]string{
		"*.pkg.dev": "wildcard/repo",
	}})
	for _, h := range []string{
		"evil-docker.pkg.dev.attacker.example",
		"foo.pkg.dev",
		"a.b-docker.pkg.dev",
	} {
		if got, ok := r.resolveProbeRepo(h); ok {
			t.Fatalf("宿主 %q 应拒绝，得到 %q", h, got)
		}
	}
}

// §15.3 两路径一致性 + jobs 缺映射跳过：同一 Host，reconcileRepos 与 jobs
// 必须得出相同的解析结论；无映射的 Host 被 jobs 跳过且不兜底。
func TestG4ReconcileJobsSameResolutionAndSkip(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	f.now = time.Now()
	repos := map[string]string{
		"quay.io":   "prometheus/node-exporter",
		"*.pkg.dev": "google-samples/containers/gke/hello-app:1.0",
	}
	// 造两个 Host 的 due 行：quay.io 有映射、evil 后缀无映射。
	if err := f.st.AddSourceHost(f.srcID, "quay.io", "https://quay.io", nil); err != nil {
		t.Fatal(err)
	}
	// 相似后缀 Host 连 Source 都无法登记（store 层拒绝），
	// 因此 jobs 天然不会为它生成任务；resolveProbeRepo 层另有拒绝测试。
	if _, err := f.st.AddSource(store.Source{Name: "evil", BaseURL: "https://evil.example", Status: "usable", Enabled: true, RepoHost: "evil-docker.pkg.dev.attacker.example"}); err == nil {
		t.Fatal("evil 相似后缀 Host 应被 store 拒绝登记")
	}

	r := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds, Repos: repos,
		Concurrency: 1, Now: func() time.Time { return f.now },
	})
	jobs, err := r.jobs(context.Background(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	foundQuay, foundEvil := false, false
	for _, j := range jobs {
		if j.Host == "quay.io" {
			foundQuay = true
			want, ok := r.resolveProbeRepo("quay.io")
			if !ok || j.Repo != want {
				t.Fatalf("jobs Repo=%q 与统一解析 %q 不一致", j.Repo, want)
			}
		}
		if strings.Contains(j.Host, "attacker") {
			foundEvil = true
		}
	}
	if !foundQuay {
		t.Fatal("quay.io 有映射，jobs 应生成任务")
	}
	if foundEvil {
		t.Fatal("evil-docker.pkg.dev.attacker.example 无映射，jobs 应跳过且不兜底")
	}
}
