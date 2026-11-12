package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	dir   string
	key   []byte
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
	id, err := st.AddSource(store.Source{Name: "mirror", BaseURL: "https://mirror.example", Status: "usable", Enabled: true, RepoHost: host})
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
	return &hostProbeFixture{st: st, creds: cs, srcID: id, dir: dir, key: key, now: time.Unix(1000, 0)}
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

func TestReconcileReposClearsChangedRepo(t *testing.T) {
	note := func(repo string) string {
		return `{"version":2,"repo":"` + repo + `","tag":"latest","native":{"mode":"native","cap":1},` +
			`"host_prefix":{"mode":"host_prefix","cap":0},"selected":"native"}`
	}
	seed := func(t *testing.T, f *hostProbeFixture, repo string) {
		t.Helper()
		res, err := f.st.SetHostCaps(f.srcID, "quay.io", store.RegistryCapYes, store.PathNative, 100, note(repo), 0, "")
		if err != nil || res != store.HostCapsCommitted {
			t.Fatalf("seed: %v %v", res, err)
		}
	}

	t.Run("changed", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		seed(t, f, "old/repo")
		r := NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds, Now: func() time.Time { return f.now },
			Repos: map[string]string{"quay.io": "new/repo"},
		})
		r.reconcileRepos()
		x, _ := f.st.GetSource(f.srcID)
		if x.RegistryCap != store.RegistryCapUnknown || x.RegistryPath != "" {
			t.Fatalf("旧仓库结论应失效: %+v", x)
		}
		if x.RegistryProbeError != note("old/repo") {
			t.Fatalf("诊断不应被清掉: %q", x.RegistryProbeError)
		}
	})

	t.Run("same", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		seed(t, f, "prometheus/node-exporter")
		r := NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds, Now: func() time.Time { return f.now },
			Repos: map[string]string{"quay.io": "prometheus/node-exporter"},
		})
		r.reconcileRepos()
		x, _ := f.st.GetSource(f.srcID)
		if x.RegistryCap != store.RegistryCapYes || x.RegistryPath != store.PathNative {
			t.Fatalf("映射未变不应清结论: %+v", x)
		}
	})

	t.Run("mapping removed", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		seed(t, f, "prometheus/node-exporter")
		r := NewHostProbeRunner(HostProbeOptions{Store: f.st, Creds: f.creds, Now: func() time.Time { return f.now }})
		r.reconcileRepos()
		x, _ := f.st.GetSource(f.srcID)
		if x.RegistryCap != store.RegistryCapUnknown {
			t.Fatalf("映射被删后旧结论应失效: %+v", x)
		}
	})
}

// 凭据版本变化后，退避必须回到起点，不能被旧版本的连续失败继续抬高。
func TestBackoffResetsOnNewSnapshot(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "r"}}
	r, _ := f.runner(t, jobs, func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
		if _, err := f.st.BumpHostGen("quay.io"); err != nil {
			t.Errorf("bump: %v", err)
		}
		return yesOutcome()
	})
	key := jobKey(jobs[0])
	backoff := func() time.Duration {
		r.mu.Lock()
		defer r.mu.Unlock()
		if st := r.state[key]; st != nil {
			return st.backoff
		}
		return 0
	}

	r.Once(context.Background())
	if got := backoff(); got != 15*time.Second {
		t.Fatalf("首次 stale 退避应为 15s，得到 %v", got)
	}
	f.now = f.now.Add(16 * time.Second)
	r.Once(context.Background())
	if got := backoff(); got != 15*time.Second {
		t.Fatalf("快照版本已变，退避应重置为 15s，得到 %v", got)
	}
}

// stale 提交绝不能覆盖「最近一次有效 Probe」的诊断与时间。
func TestStaleDoesNotTouchDiagnostics(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	note := `{"version":2,"repo":"prometheus/node-exporter","tag":"latest","native":{"mode":"native","cap":1},` +
		`"host_prefix":{"mode":"host_prefix","cap":0},"selected":"native"}`
	res, err := f.st.SetHostCaps(f.srcID, "quay.io", store.RegistryCapYes, store.PathNative, 100, note, 0, "")
	if err != nil || res != store.HostCapsCommitted {
		t.Fatalf("seed: %v %v", res, err)
	}
	before, _ := f.st.GetSource(f.srcID)

	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "r"}}
	r, _ := f.runner(t, jobs, func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
		if _, err := f.st.BumpHostGen("quay.io"); err != nil {
			t.Errorf("bump: %v", err)
		}
		return yesOutcome()
	})
	r.Once(context.Background())

	after, _ := f.st.GetSource(f.srcID)
	if after.RegistryProbeError != before.RegistryProbeError || after.RegistryProbeAt != before.RegistryProbeAt {
		t.Fatalf("stale 改动了诊断: 前 %q@%d 后 %q@%d",
			before.RegistryProbeError, before.RegistryProbeAt, after.RegistryProbeError, after.RegistryProbeAt)
	}
	// 能力由「凭据代次变更」清掉是预期；这里只固定诊断不被 stale 覆盖。
	if after.RegistryCap != store.RegistryCapUnknown {
		t.Fatalf("变更代次后能力应回未知: %+v", after)
	}
}

// v1 诊断没有 repo 字段，未知版本更不能当可信证据：一律清能力。
func TestReconcileReposClearsUnverifiableDiagnostics(t *testing.T) {
	for name, note := range map[string]string{
		"v1":      `{"version":1,"native":{"mode":"native","cap":1}}`,
		"future":  `{"version":9,"repo":"prometheus/node-exporter"}`,
		"garbage": `boom`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newHostProbeFixture(t, "quay.io")
			res, err := f.st.SetHostCaps(f.srcID, "quay.io", store.RegistryCapYes, store.PathNative, 100, note, 0, "")
			if err != nil || res != store.HostCapsCommitted {
				t.Fatalf("seed: %v %v", res, err)
			}
			r := NewHostProbeRunner(HostProbeOptions{
				Store: f.st, Creds: f.creds, Now: func() time.Time { return f.now },
				Repos: map[string]string{"quay.io": "prometheus/node-exporter"},
			})
			r.reconcileRepos()
			x, _ := f.st.GetSource(f.srcID)
			if x.RegistryCap != store.RegistryCapUnknown {
				t.Fatalf("无法核实仓库的诊断应清能力: %+v", x)
			}
			if x.RegistryProbeError != note {
				t.Fatalf("诊断应保留")
			}
		})
	}
}

// TestJobsComeFromDurableRecovery 验证待探测集合来自 recovery 表的
// next_probe_at，而不是进程内状态：同一 DB 上新建 Runner（模拟重启）
// 仍能捞回到期任务。
func TestJobsComeFromDurableRecovery(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	repos := map[string]string{"quay.io": "prometheus/node-exporter"}

	// 把 next_probe_at 固定到 at+TTL，绕开 AddSource 写入的真实时间。
	if res, err := f.st.CommitHostProbe(store.HostProbeCommit{
		SourceID: f.srcID, Host: "quay.io", Cap: store.RegistryCapYes,
		PathMode: store.PathNative, At: 1000, TTL: time.Hour,
	}); err != nil || res != store.HostCapsCommitted {
		t.Fatalf("seed commit: %v %v", res, err)
	}

	mk := func(now int64) *HostProbeRunner {
		return NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds, Repos: repos,
			Now: func() time.Time { return time.Unix(now, 0) },
		})
	}

	r := mk(2000)
	jobs, err := r.jobs(context.Background(), r.o.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("未到期不应入队: %+v", jobs)
	}

	due := mk(5000) // 4600 到期
	jobs, err = due.jobs(context.Background(), due.o.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].SourceID != f.srcID || jobs[0].Host != "quay.io" {
		t.Fatalf("到期任务缺失: %+v", jobs)
	}

	// 重启：新实例没有任何内存状态，仍从 DB 捞到同一条。
	restarted := mk(5000)
	jobs, err = restarted.jobs(context.Background(), restarted.o.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].SourceID != f.srcID {
		t.Fatalf("重启后丢失待重验任务: %+v", jobs)
	}
}

// TestProbeStaleAfterCredChangeIsRejected：探测进行中凭据变化，旧探测
// 携带的 gen 已过期，整笔写回被拒——cap、scoped、recovery 都不动。
func TestProbeStaleAfterCredChangeIsRejected(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "prometheus/node-exporter"}}
	r, _ := f.runner(t, jobs, func(ctx context.Context, p probe.RegistryHostProbe) probe.RegistryHostOutcome {
		d := "rotated"
		if err := f.st.ApplyCredChanges([]store.CredChange{{Host: "quay.io", Digest: &d}}); err != nil {
			t.Errorf("apply creds: %v", err)
		}
		return yesOutcome()
	})
	r.Once(context.Background())

	v, err := f.st.RecoveryState(f.srcID, "registry", "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if v.Gen != 1 || v.State != store.RecoveryInitial || v.LastProbeAt != 0 {
		t.Fatalf("旧探测不应写入 recovery: %+v", v)
	}
}

// TestHostProbeScanDoesNotDuplicateInflight：同一 (source,host) 在探测
// 未完成时不能被下一轮扫描重复启动；finish 后 claim 必须释放。
func TestHostProbeScanDoesNotDuplicateInflight(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "prometheus/node-exporter"}}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls int64

	r := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds,
		Repos:       map[string]string{"quay.io": "prometheus/node-exporter"},
		Concurrency: 2, Backoff: 15 * time.Second,
		Now:  func() time.Time { return f.now },
		Jobs: func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(ctx context.Context, p probe.RegistryHostProbe) probe.RegistryHostOutcome {
			atomic.AddInt64(&calls, 1)
			started <- struct{}{}
			<-release
			return yesOutcome()
		},
	})

	done := make(chan struct{})
	go func() { r.Once(context.Background()); close(done) }()
	<-started

	// 探测还在飞：第二轮扫描必须跳过。
	r.Once(context.Background())
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("探测未完成时被重复启动: calls=%d", n)
	}

	close(release)
	<-done

	// 第一轮已 finish，claim 必须释放；再扫一轮应能重新启动。
	r.Once(context.Background())
	if n := atomic.LoadInt64(&calls); n != 2 {
		t.Fatalf("finish 后 claim 未释放: calls=%d", n)
	}
}

// TestCredSnapshotGenerationMatchesDBAfterRestart：代次权威在
// registry_host_gen；CredStore 不缓存 gen，重启后新建实例读到的
// Generation 必须与数据库一致，不能出现内存 5 / 数据库 6。
func TestCredSnapshotGenerationMatchesDBAfterRestart(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	if _, err := f.creds.Put("quay.io", "u", "p"); err != nil {
		t.Fatal(err)
	}

	cs2, err := secret.NewCredStore(filepath.Join(f.dir, "registry_creds.enc"), f.key, f.st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs2.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap, err := cs2.Snapshot("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	gen, _, err := f.st.HostGenState("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 || snap.Generation != gen {
		t.Fatalf("快照代次与数据库不一致: snap=%d db=%d", snap.Generation, gen)
	}
}
