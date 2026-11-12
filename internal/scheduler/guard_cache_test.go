package scheduler

import (
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 缓存命中必须按当前时间重算 TTL：SQL 的时间条件只在查询时生效。
func TestRegistryCandidateCacheHonorsTTL(t *testing.T) {
	st := newStore(t)
	g := store.NewRecoveryGuard()
	if err := st.SetRecoveryGuard(g); err != nil {
		t.Fatal(err)
	}
	addRegSource(t, st, "quay.io", "native")

	sc := New(st, Config{Enabled: true, ListTTL: 100 * time.Hour})
	base := time.Now()
	sc.now = func() time.Time { return base }

	if n := len(sc.Candidates("registry:quay.io")); n != 1 {
		t.Fatalf("初始候选数=%d", n)
	}
	// 超过 RecoveryTTL 但仍在 ListTTL 内：必须命中缓存并被实时闸门排除。
	sc.now = func() time.Time { return base.Add(25 * time.Hour) }
	if n := len(sc.Candidates("registry:quay.io")); n != 0 {
		t.Fatalf("缓存命中绕过了 TTL，仍返回 %d 个候选", n)
	}
}

// 三个入口（Store 直调、缓存未命中、缓存命中）必须使用同一份 Guard 快照。
func TestRegistryEntrypointsShareGuard(t *testing.T) {
	st := newStore(t)
	g := store.NewRecoveryGuard()
	if err := st.SetRecoveryGuard(g); err != nil {
		t.Fatal(err)
	}
	id := addRegSource(t, st, "quay.io", "native")

	sc := New(st, Config{Enabled: true, ListTTL: time.Hour})
	key := store.GuardKey{SourceID: id, Family: "registry", Scope: "quay.io"}
	if n := len(sc.Candidates("registry:quay.io")); n != 1 {
		t.Fatalf("预热候选数=%d", n)
	}

	tok := g.Block(key, 0)
	if n := len(sc.Candidates("registry:quay.io")); n != 0 {
		t.Fatalf("缓存命中路径未过滤 Guard：%d", n)
	}
	if n := len(sc.Candidates("registry:quay.io")); n != 0 {
		t.Fatalf("二次命中路径未过滤 Guard：%d", n)
	}
	got, err := st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("API 直调路径未过滤 Guard：%d", len(got))
	}

	// 解除后缓存必须能恢复该源：证明缓存保留的是原始列表，没有被就地过滤。
	if !g.UnblockIfMatch(key, tok) {
		t.Fatal("解除失败")
	}
	if n := len(sc.Candidates("registry:quay.io")); n != 1 {
		t.Fatalf("解除后缓存无法恢复：%d", n)
	}
}

// 断言 36（Scheduler 侧）：调度器现取的 Guard 必须是 Store 注入的同一个指针。
func TestSchedulerSharesStoreGuardIdentity(t *testing.T) {
	st := newStore(t)
	g := store.NewRecoveryGuard()
	if err := st.SetRecoveryGuard(g); err != nil {
		t.Fatal(err)
	}
	sc := New(st, Config{Enabled: true, ListTTL: time.Hour})
	if sc.st == nil || sc.st.RecoveryGuard() == nil {
		t.Fatal("Scheduler 未接上 Store 的 Guard")
	}
	if sc.st.RecoveryGuard() != g {
		t.Fatal("Scheduler 看到的 Guard 与装配点不是同一指针")
	}
}
