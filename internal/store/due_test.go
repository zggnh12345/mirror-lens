package store

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func atomicSeed() int64 { return atomic.AddInt64(&seedSeq, 1) }

func addSourceAt(t *testing.T, s *Store, host string, enabled bool, capv int) int64 {
	t.Helper()
	n := atomicSeed()
	id, err := s.AddSource(Source{
		Name:        "due-" + strconv.FormatInt(n, 10),
		BaseURL:     "https://mirror-" + strconv.FormatInt(n, 10) + ".example",
		Status:      "usable",
		RepoHost:    host,
		Enabled:     enabled,
		RegistryCap: capv,
		CreatedAt:   1000,
	})
	if err != nil {
		t.Fatalf("AddSource(%s): %v", host, err)
	}
	return id
}

func setNextProbe(t *testing.T, s *Store, id int64, host string, next int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE registry_host_recovery SET next_probe_at=?
WHERE source_id=? AND family='registry' AND scope=?`, next, id, host); err != nil {
		t.Fatal(err)
	}
}

func dueSet(t *testing.T, s *Store, now int64) map[int64]bool {
	t.Helper()
	rows, err := s.ListDueHostProbes(now)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]bool{}
	for _, x := range rows {
		out[x.SourceID] = true
	}
	return out
}

func TestListDueHostProbes(t *testing.T) {
	s, _ := openTestStore(t)
	const now = int64(10_000)

	due := addSourceAt(t, s, "quay.io", true, RegistryCapUnknown)
	setNextProbe(t, s, due, "quay.io", now-1)

	future := addSourceAt(t, s, "quay.io", true, RegistryCapUnknown)
	setNextProbe(t, s, future, "quay.io", now+100)

	missing := addSourceAt(t, s, "quay.io", true, RegistryCapYes)
	if _, err := s.db.Exec(`DELETE FROM registry_host_recovery WHERE source_id=?`, missing); err != nil {
		t.Fatal(err)
	}

	disabled := addSourceAt(t, s, "quay.io", false, RegistryCapUnknown)
	setNextProbe(t, s, disabled, "quay.io", now-1)

	hardCap := addSourceAt(t, s, "quay.io", true, RegistryCapMismatch)
	setNextProbe(t, s, hardCap, "quay.io", now-1)

	hardState := addSourceAt(t, s, "quay.io", true, RegistryCapUnknown)
	if _, err := s.db.Exec(`UPDATE registry_host_recovery SET state='hard-excluded', next_probe_at=?
WHERE source_id=? AND scope='quay.io'`, now-1, hardState); err != nil {
		t.Fatal(err)
	}

	other := addSourceAt(t, s, "registry.k8s.io", true, RegistryCapUnknown)
	setNextProbe(t, s, other, "registry.k8s.io", now-1)

	archived := addSourceAt(t, s, "quay.io", true, RegistryCapUnknown)
	setNextProbe(t, s, archived, "quay.io", now-1)
	if _, err := s.db.Exec(`UPDATE sources SET status='archived' WHERE id=?`, archived); err != nil {
		t.Fatal(err)
	}

	hub := addSourceAt(t, s, "", true, RegistryCapUnknown)
	ghcr := addSourceAt(t, s, "ghcr.io", true, RegistryCapUnknown)

	got := dueSet(t, s, now)
	for _, id := range []int64{due, missing, other} {
		if !got[id] {
			t.Fatalf("源 %d 应到期却没返回: %v", id, got)
		}
	}
	for _, id := range []int64{future, disabled, hardCap, hardState, archived, hub, ghcr} {
		if got[id] {
			t.Fatalf("源 %d 不应返回: %v", id, got)
		}
	}
}

func TestListDueHostProbesCarriesGen(t *testing.T) {
	s, _ := openTestStore(t)
	id := addSourceAt(t, s, "quay.io", true, RegistryCapUnknown)
	setGen(t, s, "quay.io", 4, "d4")
	setNextProbe(t, s, id, "quay.io", 1)

	rows, err := s.ListDueHostProbes(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Gen != 4 || rows[0].RepoHost != "quay.io" {
		t.Fatalf("due=%+v", rows)
	}
}

func TestAddSourceCreatesRecoveryRow(t *testing.T) {
	s, _ := openTestStore(t)
	id := addSourceAt(t, s, "quay.io", true, RegistryCapUnknown)

	v, err := s.RecoveryState(id, "registry", "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Exists || v.State != RecoveryInitial || v.Gen != 0 || v.NextProbeAt == 0 {
		t.Fatalf("新源 recovery=%+v", v)
	}

	// 已有代次行时，新源直接绑当前代次。
	setGen(t, s, "quay.io", 3, "d3")
	id2 := addSourceAt(t, s, "quay.io", true, RegistryCapUnknown)
	v2, _ := s.RecoveryState(id2, "registry", "quay.io")
	if v2.Gen != 3 {
		t.Fatalf("新源应绑当前代次 3, got %d", v2.Gen)
	}

	// Hub（显式 docker.io 与历史空 Host）及 ghcr 不参与 Host 探测，不建行。
	for _, h := range []string{"docker.io", "ghcr.io"} {
		id3 := addSourceAt(t, s, h, true, RegistryCapUnknown)
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM registry_host_recovery WHERE source_id=?`, id3).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("host=%q 不应建 recovery 行", h)
		}
	}
}

func TestApplyCredChangesResetsAllSourcesOnHost(t *testing.T) {
	s, _ := openTestStore(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		id := addSourceAt(t, s, "quay.io", true, RegistryCapYes)
		ids = append(ids, id)
	}
	other := addSourceAt(t, s, "registry.k8s.io", true, RegistryCapYes)

	setGen(t, s, "quay.io", 5, "d5")
	setGen(t, s, "registry.k8s.io", 5, "k5")
	for _, id := range append(append([]int64{}, ids...), other) {
		host, digest := "quay.io", "d5"
		if id == other {
			host, digest = "registry.k8s.io", "k5"
		}
		if res := mustCommit(t, s, HostProbeCommit{
			SourceID: id, Host: host, Cap: RegistryCapYes, PathMode: PathNative,
			At: 1000, Gen: 5, CredDigest: digest, TTL: 3600,
		}); res != HostCapsCommitted {
			t.Fatalf("seed commit %d: %v", id, res)
		}
	}

	d6 := "d6"
	if err := s.ApplyCredChanges([]CredChange{{Host: "quay.io", Digest: &d6}}); err != nil {
		t.Fatal(err)
	}

	for _, id := range ids {
		v, err := s.RecoveryState(id, "registry", "quay.io")
		if err != nil {
			t.Fatal(err)
		}
		if v.State != RecoveryInitial || v.Gen != 6 || v.LastProbeAt != 0 || v.NextProbeAt == 0 {
			t.Fatalf("源 %d 未被重置: %+v", id, v)
		}
	}
	v, _ := s.RecoveryState(other, "registry", "registry.k8s.io")
	if v.State != RecoveryOK || v.Gen != 5 {
		t.Fatalf("其他 Host 被误重置: %+v", v)
	}
}

func TestSetRepoHostUsesNewHostGen(t *testing.T) {
	s, _ := openTestStore(t)
	id := addSourceAt(t, s, "quay.io", true, RegistryCapYes)
	setGen(t, s, "quay.io", 9, "d9")
	mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 1000, Gen: 9, CredDigest: "d9", TTL: 3600,
	})

	setGen(t, s, "registry.k8s.io", 4, "k4")
	if err := s.SetRepoHost(id, "registry.k8s.io"); err != nil {
		t.Fatal(err)
	}
	v, err := s.RecoveryState(id, "registry", "registry.k8s.io")
	if err != nil {
		t.Fatal(err)
	}
	if v.Gen != 4 {
		t.Fatalf("新 Host 必须用新 Host 的代次 4，不能沿用旧 Host 的 9: %+v", v)
	}

	// 新 Host 没有代次行时按缺行规则记 0，不能伪造一个有效代次。
	id2 := addSourceAt(t, s, "quay.io", true, RegistryCapYes)
	if err := s.SetRepoHost(id2, "gcr.io"); err != nil {
		t.Fatal(err)
	}
	v2, _ := s.RecoveryState(id2, "registry", "gcr.io")
	if v2.Gen != 0 || v2.State != RecoveryInitial {
		t.Fatalf("新 Host 无代次行应为 initial/gen=0: %+v", v2)
	}
}

func TestReenableSourceMakesRecoveryDue(t *testing.T) {
	s, _ := openTestStore(t)
	id := addSourceAt(t, s, "quay.io", false, RegistryCapUnknown)
	far := time.Now().Unix() + 86400
	if _, err := s.db.Exec(`UPDATE registry_host_recovery SET next_probe_at=? WHERE source_id=?`, far, id); err != nil {
		t.Fatal(err)
	}
	on := true
	if err := s.UpdateSource(id, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !dueSet(t, s, time.Now().Unix())[id] {
		t.Fatal("启用后应立即可探测，不能等完退避期")
	}
}

// TestUnsupportedProbeSchedule：cap=0 不是永久排除，但要按 6h 周期重探，
// 不能因为 next_probe_at 缺失或停在 now 而变成 30s 一轮。
func TestUnsupportedProbeSchedule(t *testing.T) {
	s, _ := openTestStore(t)
	id := addSourceAt(t, s, "quay.io", true, RegistryCapUnknown)
	setGen(t, s, "quay.io", 0, "")

	if res := mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapNo, At: 1000, TTL: time.Hour,
	}); res != HostCapsCommitted {
		t.Fatalf("commit: %v", res)
	}
	v, err := s.RecoveryState(id, "registry", "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != RecoveryUnsupported {
		t.Fatalf("cap=0 应派生 unsupported: %+v", v)
	}
	if v.NextProbeAt != 1000+6*3600 {
		t.Fatalf("unsupported 应排到 6h 后, got %d", v.NextProbeAt)
	}
	if dueSet(t, s, 1000)[id] {
		t.Fatal("unsupported 未到期不应入队")
	}
	if !dueSet(t, s, 1000+6*3600)[id] {
		t.Fatal("unsupported 到期后应入队重探")
	}
}
