package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/secret"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

type hostProbeFixture struct {
	st    *store.Store
	creds *secret.Store
	srcID int64
	now   time.Time
}

func newHostProbeFixture(t *testing.T, host string) *hostProbeFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.AddSource(store.Source{Name: "mirror", BaseURL: "https://mirror.example", Status: "usable", RepoHost: host})
	if err != nil {
		t.Fatal(err)
	}
	key, err := secret.LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := secret.NewCredStore(filepath.Join(dir, "registry_creds.enc"), key, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	return &hostProbeFixture{st: st, creds: cs, srcID: id, now: time.Unix(1000, 0)}
}

func (f *hostProbeFixture) runner(t *testing.T, jobs []HostProbeJob, fn func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome) (*HostProbeRunner, *int) {
	t.Helper()
	calls := 0
	r := NewHostProbeRunner(HostProbeOptions{
		Store:       f.st,
		Creds:       f.creds,
		Repos:       map[string]string{"quay.io": "prometheus/node-exporter"},
		Concurrency: 2,
		Backoff:     15 * time.Second,
		Now:         func() time.Time { return f.now },
		Jobs: func(context.Context) ([]HostProbeJob, error) {
			return jobs, nil
		},
		Probe: func(ctx context.Context, p probe.RegistryHostProbe) probe.RegistryHostOutcome {
			calls++
			return fn(ctx, p)
		},
	})
	return r, &calls
}

func yesOutcome() probe.RegistryHostOutcome {
	return probe.RegistryHostOutcome{
		Host:       "quay.io",
		Cap:        probe.CapYes,
		Selected:   "native",
		Native:     probe.PathOutcome{Mode: "native", Cap: probe.CapYes, Verdict: "yes"},
		HostPrefix: probe.PathOutcome{Mode: "host_prefix", Cap: probe.CapNo, Verdict: "no"},
	}
}

func TestHostProbeRunnerCommitsDiagnostics(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "prometheus/node-exporter"}}
	r, calls := f.runner(t, jobs, func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
		return yesOutcome()
	})
	r.Once(context.Background())
	if *calls != 1 {
		t.Fatalf("应探测一次，实际 %d", *calls)
	}
	x, err := f.st.GetSource(f.srcID)
	if err != nil {
		t.Fatal(err)
	}
	if x.RegistryCap != store.RegistryCapYes || x.RegistryPath != store.PathNative {
		t.Fatalf("能力未写入: %+v", x)
	}
	var d probe.Diagnostics
	raw := x.RegistryProbeError
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("诊断应为完整 JSON: %v (%q)", err, raw)
	}
	if d.Version != probe.DiagnosticsVersion || d.Native.Cap != probe.CapYes || d.Selected != "native" {
		t.Fatalf("诊断内容不对: %+v", d)
	}
	if len(r.state) != 0 {
		t.Fatalf("成功后不应留下重试状态: %+v", r.state)
	}
}

func TestHostProbeRunnerStaleSnapshotBacksOff(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "r"}}
	var mu sync.Mutex
	r, calls := f.runner(t, jobs, func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
		// 模拟探测期间凭据被换掉。
		mu.Lock()
		if _, err := f.st.BumpHostGen("quay.io"); err != nil {
			t.Errorf("bump: %v", err)
		}
		mu.Unlock()
		return yesOutcome()
	})
	r.Once(context.Background())
	x, _ := f.st.GetSource(f.srcID)
	if x.RegistryCap != store.RegistryCapUnknown || x.RegistryProbeError != "" {
		t.Fatalf("stale 结果不应落库: %+v", x)
	}
	if *calls != 1 {
		t.Fatalf("应当只调用一次，实际 %d", *calls)
	}
	// 同一个时钟点再跑一轮：退避窗口内不得重排。
	r.Once(context.Background())
	if *calls != 1 {
		t.Fatalf("退避期内不应重试，实际 %d 次", *calls)
	}
	f.now = f.now.Add(16 * time.Second)
	r.Once(context.Background())
	if *calls != 2 {
		t.Fatalf("退避结束后应重排，实际 %d 次", *calls)
	}
}

func TestHostProbeRunnerDropsArchivedAndChanged(t *testing.T) {
	t.Run("archived", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		if err := f.st.SetStatus(f.srcID, "archived"); err != nil {
			t.Fatal(err)
		}
		jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "r"}}
		r, calls := f.runner(t, jobs, func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return yesOutcome()
		})
		r.Once(context.Background())
		if *calls != 1 {
			t.Fatalf("应探测一次，实际 %d", *calls)
		}
		x, _ := f.st.GetSource(f.srcID)
		if x.RegistryCap != store.RegistryCapUnknown {
			t.Fatalf("归档源不应被写能力: %+v", x)
		}
		if len(r.state) != 0 {
			t.Fatalf("归档源不应留在重试队列: %+v", r.state)
		}
	})

	t.Run("host changed", func(t *testing.T) {
		f := newHostProbeFixture(t, "lscr.io")
		jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "r"}}
		r, _ := f.runner(t, jobs, func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return yesOutcome()
		})
		r.Once(context.Background())
		x, _ := f.st.GetSource(f.srcID)
		if x.RegistryCap != store.RegistryCapUnknown {
			t.Fatalf("归属已变的旧结果不应落库: %+v", x)
		}
		if len(r.state) != 0 {
			t.Fatalf("换 Host 的源不应留在旧键上重试: %+v", r.state)
		}
	})
}

// 探测过程中不得重新读凭据：注入的 Probe 只应看到 Runner 传进来的那一份。
func TestHostProbeRunnerPassesSnapshot(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "r"}}
	r, _ := f.runner(t, jobs, func(_ context.Context, p probe.RegistryHostProbe) probe.RegistryHostOutcome {
		if p.Snapshot.Host != "quay.io" {
			t.Errorf("快照 Host 不对: %+v", p.Snapshot)
		}
		if p.Snapshot.Generation != 0 || p.Snapshot.Digest != "" {
			t.Errorf("无凭据 Host 的快照版本应为 0/空: %+v", p.Snapshot)
		}
		return yesOutcome()
	})
	r.Once(context.Background())
}
