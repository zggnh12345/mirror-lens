package store

import (
	"path/filepath"
	"testing"
	"time"
)

// 同一来源在某个 Host 上连续失败，只摘掉这个 Host 的候选资格。
func TestHealthScopeHostIsolation(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 1)
	other := addRegSource(t, st, "registry.k8s.io", "native", 1, true, "usable", 0)

	for i := 0; i < HealthFails; i++ {
		if _, err := st.CommitHostProbe(HostProbeCommit{
			SourceID: id, Host: "quay.io", Cap: RegistryCapUnknown,
			Reason: probeReasonTransportError, At: int64(1000 + i), ErrText: "timeout",
			Gen: 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; ok {
		t.Fatalf("Host 连续失败后仍在候选里: %v", candidatesIDs(got))
	}
	// 另一个 Host 的源不受影响。
	got, err = st.SourceCandidates("registry:registry.k8s.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[other]; !ok {
		t.Fatalf("别的 Host 候选被牵连: %v", candidatesIDs(got))
	}
	// docker 池同样不受 registry 健康影响。
	got, err = st.SourceCandidates("docker")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; !ok {
		t.Fatalf("registry 健康污染了 docker 候选: %v", candidatesIDs(got))
	}
}

// 同一 Host 的失败计数在成功写回前保持累加。
func TestHealthScopeSuccessDoesNotClearOtherHost(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)

	for i := 0; i < HealthFails; i++ {
		if _, err := st.CommitHostProbe(HostProbeCommit{
			SourceID: id, Host: "quay.io", Cap: RegistryCapUnknown,
			Reason: probeReasonTransportError, At: int64(1000 + i), ErrText: "timeout",
			Gen: 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := st.ScopedHealthOf(id)
	if err != nil {
		t.Fatal(err)
	}
	if h["registry@quay.io"] != HealthFails {
		t.Fatalf("quay.io 失败计数丢失: %v", h)
	}
}

// 旧库只有 family 级 registry 行时不再兜底：候选准入要求 recovery 行
// 为 ok，没有这条证据就不放行（fail-closed）。legacy 的 fails=0 也不能
// 替代「本代次探测成功」。
func TestRegistryCandidateRequiresRecoveryEvidence(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	if _, err := st.db.Exec(`INSERT INTO source_health(source_id, family, fails, last_at) VALUES(?, 'registry', 0, 1)`,
		id); err != nil {
		t.Fatal(err)
	}
	// 删掉 seed 的 ok 行：只剩 sources.registry_cap=1 和 legacy family 行。
	if _, err := st.db.Exec(`DELETE FROM registry_host_recovery WHERE source_id=?`, id); err != nil {
		t.Fatal(err)
	}
	got, err := st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; ok {
		t.Fatal("没有 recovery 证据时 legacy 行不应放行")
	}
	// 探测成功写回 recovery=ok 后才进入候选。
	if _, err := st.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: time.Now().Unix(), Gen: 0,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; !ok {
		t.Fatal("recovery=ok 后仍不在候选")
	}
}

// 重启后 Host 级状态仍在，且仍然隔离。
func TestHealthScopePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	for i := 0; i < HealthFails; i++ {
		if _, err := st.CommitHostProbe(HostProbeCommit{
			SourceID: id, Host: "quay.io", Cap: RegistryCapUnknown,
			Reason: probeReasonTransportError, At: int64(1000 + i), ErrText: "timeout",
			Gen: 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	st2, err := Open(path) // 迁移/建表必须可重复执行
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	got, err := st2.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; ok {
		t.Fatal("重启后 Host 级失败状态丢失")
	}
}
