package runner

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 结果自相矛盾：不提交、屏蔽、按 retryOutcomeInconsistent 退避。
// 同时锁住「退避不碰 rec_failures、last_probe_at、last_success_at」。
func TestOutcomeInconsistentBlocksAndBacksOff(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	f.now = time.Now()
	gen, err := f.st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	g := store.NewRecoveryGuard()
	invalidations := 0
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", Gen: gen, BaseURL: "https://mirror.example", Repo: "r"}}
	r := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds, Repos: map[string]string{"quay.io": "r"},
		Concurrency: 1,
		Now:         func() time.Time { return f.now },
		Jobs:        func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return probe.RegistryHostOutcome{Host: "quay.io", Cap: probe.CapYes, Selected: "native"}
		},
		Guard:      g,
		Invalidate: func() { invalidations++ },
	})
	r.Once(context.Background())

	key := store.GuardKey{SourceID: f.srcID, Family: "registry", Scope: "quay.io"}
	if !g.Blocked(key) {
		t.Fatal("结果矛盾必须屏蔽该源")
	}
	if invalidations != 1 {
		t.Fatalf("应当失效路由缓存一次，实际 %d", invalidations)
	}
	failures, next, ok, err := f.st.HostProbeRetryState(f.srcID, "quay.io")
	if err != nil || !ok {
		t.Fatalf("读退避: ok=%v err=%v", ok, err)
	}
	if failures != 1 {
		t.Fatalf("矛盾结果应记 1 次退避，得到 %d", failures)
	}
	if got, want := next, f.now.Unix()+15; got != want {
		t.Fatalf("首次退避应为 15s: next=%d want=%d", got, want)
	}
	x, err := f.st.GetSource(f.srcID)
	if err != nil {
		t.Fatal(err)
	}
	if x.RegistryCap != store.RegistryCapUnknown || x.RegistryProbeAt != 0 || x.RegistryProbeError != "" {
		t.Fatalf("矛盾结果不得写 cap 诊断: cap=%d at=%d err=%q", x.RegistryCap, x.RegistryProbeAt, x.RegistryProbeError)
	}
	raw, err := sql.Open("sqlite", filepath.Join(f.dir, "hp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var rec int
	if err := raw.QueryRow(`SELECT rec_failures FROM registry_host_recovery WHERE source_id=? AND family='registry'`, f.srcID).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	if rec != 0 {
		t.Fatalf("本地退避不得污染 rec_failures，得到 %d", rec)
	}
}

// parent 被取消：在提交边界之前退出，既不提交也不重排。
// 行保持到期，下一轮扫描（新 ctx）自然会重探。
func TestParentCancelSkipsCommitAndSchedule(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	f.now = time.Now()
	gen, err := f.st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	_, beforeNext, _, _ := f.st.HostProbeRetryState(f.srcID, "quay.io")

	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", Gen: gen, BaseURL: "https://mirror.example", Repo: "r"}}
	r := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds, Repos: map[string]string{"quay.io": "r"},
		Concurrency: 1,
		Now:         func() time.Time { return f.now },
		Jobs:        func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return yesOutcome()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Once(ctx)

	x, err := f.st.GetSource(f.srcID)
	if err != nil {
		t.Fatal(err)
	}
	if x.RegistryCap != store.RegistryCapUnknown || x.RegistryProbeAt != 0 {
		t.Fatalf("取消后不得提交: cap=%d at=%d", x.RegistryCap, x.RegistryProbeAt)
	}
	failures, next, _, _ := f.st.HostProbeRetryState(f.srcID, "quay.io")
	if failures != 0 || next != beforeNext {
		t.Fatalf("取消不得写调度: failures=%d next=%d before=%d", failures, next, beforeNext)
	}
	if len(r.state) != 0 {
		t.Fatal("取消后必须释放执行锁")
	}
}

// 子 Context 超时是真实探测失败：parent 有效，必须照常提交，
// 由状态机（rec_failures）而不是 retry 阶梯承担退避。
func TestProbeTimeoutStillCommits(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	f.now = time.Now()
	gen, err := f.st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", Gen: gen, BaseURL: "https://mirror.example", Repo: "r"}}
	r := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds, Repos: map[string]string{"quay.io": "r"},
		Concurrency: 1,
		Now:         func() time.Time { return f.now },
		Jobs:        func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return probe.RegistryHostOutcome{
				Host: "quay.io", Cap: probe.CapUnknown, Reason: "transport_error",
				Transport:  probe.TransportTimeout,
				Native:     probe.PathOutcome{Mode: "native", Cap: probe.CapUnknown, Verdict: "unknown", Error: "context deadline exceeded"},
				HostPrefix: probe.PathOutcome{Mode: "host_prefix", Cap: probe.CapUnknown, Verdict: "unknown", Error: "context deadline exceeded"},
			}
		},
	})
	r.Once(context.Background())

	// 瞬时传输错误不覆盖 cap 诊断，但 recovery 必须照常提交。
	raw, err := sql.Open("sqlite", filepath.Join(f.dir, "hp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var state string
	var rec int
	if err := raw.QueryRow(`SELECT state, rec_failures FROM registry_host_recovery
		WHERE source_id=? AND family='registry'`, f.srcID).Scan(&state, &rec); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || rec != 1 {
		t.Fatalf("子 Context 超时必须提交为 failed: state=%s rec=%d", state, rec)
	}
	failures, _, _, _ := f.st.HostProbeRetryState(f.srcID, "quay.io")
	if failures != 0 {
		t.Fatalf("已提交的失败走 rec_failures，不得用 retry 阶梯: %d", failures)
	}
}

// T16：真实 SQLite 端到端 —— 退避落库、重开 Store 仍生效、
// ListDueHostProbes 按 next_probe_at 放行、再探测成功清零。
func TestRetrySurvivesReopenAndGatesListDue(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	f.now = time.Now().Truncate(time.Second)
	gen, err := f.st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", Gen: gen, BaseURL: "https://mirror.example", Repo: "r"}}
	bad := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds, Repos: map[string]string{"quay.io": "r"},
		Concurrency: 1,
		Now:         func() time.Time { return f.now },
		Jobs:        func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return probe.RegistryHostOutcome{Host: "quay.io", Cap: probe.CapYes, Selected: "native"}
		},
	})
	bad.Once(context.Background())
	_, next, _, _ := f.st.HostProbeRetryState(f.srcID, "quay.io")
	if want := f.now.Unix() + 15; next != want {
		t.Fatalf("首次失败应在 15s 后到期: next=%d want=%d", next, want)
	}

	// 重开一个独立句柄：退避是持久化状态，新进程不能回到「立刻重探」。
	st2, err := store.Open(filepath.Join(f.dir, "hp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	due, err := st2.ListDueHostProbes(f.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.SourceID == f.srcID && d.RepoHost == "quay.io" {
			t.Fatal("退避未到期的行不应入队")
		}
	}
	due, err = st2.ListDueHostProbes(next)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		if d.SourceID == f.srcID && d.RepoHost == "quay.io" {
			found = true
			if d.Gen != gen {
				t.Fatalf("入队代次=%d want %d", d.Gen, gen)
			}
		}
	}
	if !found {
		t.Fatal("到期后应重新入队")
	}

	// 到期后成功提交：清零并回到 TTL 排期。
	f.now = time.Unix(next, 0)
	good := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds, Repos: map[string]string{"quay.io": "r"},
		Concurrency: 1,
		Now:         func() time.Time { return f.now },
		Jobs:        func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return yesOutcome()
		},
	})
	good.Once(context.Background())
	failures, nextAt, _, _ := f.st.HostProbeRetryState(f.srcID, "quay.io")
	if failures != 0 {
		t.Fatalf("成功提交应清零，得到 %d", failures)
	}
	if want := f.now.Unix() + int64(store.RecoveryDefaultTTL/time.Second); nextAt != want {
		t.Fatalf("成功后应按 TTL 排期: next=%d want=%d", nextAt, want)
	}
}

// 只有四类执行失败写调度；stale/cancel/none 不写。
func TestRetryReasonSchedulingMatrix(t *testing.T) {
	for _, tc := range []struct {
		why  retryReason
		want bool
	}{
		{retryNone, false},
		{retryCredentialPending, true},
		{retryLocalExecution, true},
		{retryCommitFailed, true},
		{retryOutcomeInconsistent, true},
		{retryStaleSnapshot, false},
		{retryCanceled, false},
	} {
		if got := tc.why.schedules(); got != tc.want {
			t.Fatalf("%d.schedules()=%v want %v", tc.why, got, tc.want)
		}
	}
}
