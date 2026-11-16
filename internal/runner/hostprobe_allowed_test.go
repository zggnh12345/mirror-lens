package runner

import (
	"context"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 关闭的 Host 不得继续探测，也不得写能力/健康状态。
func TestHostProbeSkipsClosedHost(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	jobs := []HostProbeJob{{
		SourceID: f.srcID,
		Host:     "quay.io",
		BaseURL:  "https://mirror.example",
		Repo:     "prometheus/node-exporter",
	}}
	calls := 0
	r := NewHostProbeRunner(HostProbeOptions{
		Store:        f.st,
		Creds:        f.creds,
		Repos:        map[string]string{"quay.io": "prometheus/node-exporter"},
		AllowedHosts: []string{"gcr.io"},
		Concurrency:  2,
		Now:          func() time.Time { return f.now },
		Jobs:         func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			calls++
			return yesOutcome()
		},
	})
	r.Once(context.Background())

	if calls != 0 {
		t.Fatalf("关闭的 Host 不应探测，实际调用 %d 次", calls)
	}
	x, err := f.st.GetSource(f.srcID)
	if err != nil {
		t.Fatal(err)
	}
	if x.RegistryCap != store.RegistryCapUnknown {
		t.Fatalf("关闭的 Host 不应写能力，实际 cap=%d", x.RegistryCap)
	}
}

func TestHostProbeRunsOpenHost(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	jobs := []HostProbeJob{{
		SourceID: f.srcID,
		Host:     "quay.io",
		BaseURL:  "https://mirror.example",
		Repo:     "prometheus/node-exporter",
	}}
	calls := 0
	r := NewHostProbeRunner(HostProbeOptions{
		Store:        f.st,
		Creds:        f.creds,
		Repos:        map[string]string{"quay.io": "prometheus/node-exporter"},
		AllowedHosts: []string{"quay.io"},
		Concurrency:  2,
		Now:          func() time.Time { return f.now },
		Jobs:         func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			calls++
			return yesOutcome()
		},
	})
	r.Once(context.Background())

	if calls != 1 {
		t.Fatalf("白名单内的 Host 应探测一次，实际 %d", calls)
	}
}

// Artifact Registry 用具体区域 Host，白名单用 *.pkg.dev 时必须命中；
// 非三段式或非 -docker 后缀的 Host 不得借通配符混入。
func TestHostProbeAllowlistArtifactRegistryPattern(t *testing.T) {
	r := NewHostProbeRunner(HostProbeOptions{AllowedHosts: []string{"*.pkg.dev"}})
	in := []HostProbeJob{
		{Host: "asia-east1-docker.pkg.dev"},
		{Host: "us-docker.pkg.dev"},
		{Host: "evil.pkg.dev"},
		{Host: "gcr.io"},
	}
	out := r.filterAllowed(in)
	if len(out) != 2 {
		t.Fatalf("应只保留两个合法 Artifact Registry Host，得到 %+v", out)
	}
	for _, j := range out {
		if j.Host == "evil.pkg.dev" || j.Host == "gcr.io" {
			t.Fatalf("不该放行 %q", j.Host)
		}
	}
}

func TestHostProbeAllowlistEmptyDeniesAll(t *testing.T) {
	r := NewHostProbeRunner(HostProbeOptions{AllowedHosts: []string{}})
	if got := r.filterAllowed([]HostProbeJob{{Host: "gcr.io"}}); len(got) != 0 {
		t.Fatalf("空白名单应拒绝全部，得到 %+v", got)
	}
}

func TestHostProbeAllowlistNilKeepsLegacy(t *testing.T) {
	r := NewHostProbeRunner(HostProbeOptions{})
	if got := r.filterAllowed([]HostProbeJob{{Host: "gcr.io"}}); len(got) != 1 {
		t.Fatalf("nil 白名单应按兼容语义放行，得到 %+v", got)
	}
}
