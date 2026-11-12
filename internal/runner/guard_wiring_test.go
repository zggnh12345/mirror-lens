package runner

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 成功提交：必须使原始候选缓存失效，且不留下屏蔽。
func TestRunnerInvalidatesCacheOnCommitSuccess(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	g := store.NewRecoveryGuard()
	if err := f.st.SetRecoveryGuard(g); err != nil {
		t.Fatal(err)
	}
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "r"}}
	invalidations := 0
	r := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds, Repos: map[string]string{"quay.io": "r"},
		Concurrency: 1,
		Now:         func() time.Time { return f.now },
		Jobs:        func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return yesOutcome()
		},
		Guard:      g,
		Invalidate: func() { invalidations++ },
	})
	r.Once(context.Background())

	if invalidations != 1 {
		t.Fatalf("成功提交后应失效缓存一次，实际 %d", invalidations)
	}
	key := store.GuardKey{SourceID: f.srcID, Family: "registry", Scope: "quay.io"}
	if g.Blocked(key) {
		t.Fatal("成功提交后不应留下屏蔽")
	}
}

// 写库失败：屏蔽该源并失效缓存，直到下一次成功提交。
func TestRunnerBlocksOnCommitFailure(t *testing.T) {
	f := newHostProbeFixture(t, "quay.io")
	g := store.NewRecoveryGuard()
	if err := f.st.SetRecoveryGuard(g); err != nil {
		t.Fatal(err)
	}
	// 让写事务必然失败，但快照读取仍然成功。
	raw, err := sql.Open("sqlite", filepath.Join(f.dir, "hp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TABLE registry_host_recovery`); err != nil {
		t.Fatal(err)
	}
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", BaseURL: "https://mirror.example", Repo: "r"}}
	invalidations := 0
	r := NewHostProbeRunner(HostProbeOptions{
		Store: f.st, Creds: f.creds, Repos: map[string]string{"quay.io": "r"},
		Concurrency: 1,
		Now:         func() time.Time { return f.now },
		Jobs:        func(context.Context) ([]HostProbeJob, error) { return jobs, nil },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return yesOutcome()
		},
		Guard:      g,
		Invalidate: func() { invalidations++ },
	})
	r.Once(context.Background())

	key := store.GuardKey{SourceID: f.srcID, Family: "registry", Scope: "quay.io"}
	if !g.Blocked(key) {
		t.Fatal("写库失败必须屏蔽该源")
	}
	if invalidations != 1 {
		t.Fatalf("失败后应失效缓存一次，实际 %d", invalidations)
	}
	if len(r.state) != 0 {
		t.Fatalf("失败后必须释放执行锁，退避由数据库负责: %v", r.state)
	}
}

// token 生命周期：解除必须精确匹配当前条目，旧代次不得清除新屏蔽。
func TestRunnerGuardTokenPairing(t *testing.T) {
	g := store.NewRecoveryGuard()
	r := &HostProbeRunner{o: HostProbeOptions{Guard: g}}
	j := HostProbeJob{SourceID: 7, Host: "quay.io"}
	key := store.GuardKey{SourceID: 7, Family: "registry", Scope: "quay.io"}

	r.blockRoute(j, 5)
	old := r.guardTok[key]
	r.blockRoute(j, 6) // 凭据推进后的新失败，seq 更新

	if g.UnblockIfMatch(key, old) {
		t.Fatal("旧 token 不应解除新屏蔽")
	}
	if !g.Blocked(key) {
		t.Fatal("新屏蔽被误删")
	}
	r.unblockRoute(j) // 用 map 里最新 token
	if g.Blocked(key) {
		t.Fatal("最新 token 应能解除")
	}
}

// 断言 36（Runner 侧）：Runner 保存的必须是注入的同一指针。
func TestRunnerSharesInjectedGuardIdentity(t *testing.T) {
	g := store.NewRecoveryGuard()
	r := &HostProbeRunner{o: HostProbeOptions{Guard: g}}
	if r.o.Guard == nil {
		t.Fatal("Guard 未注入")
	}
	if r.o.Guard != g {
		t.Fatal("Runner 保存的 Guard 与注入的不是同一指针")
	}
}
