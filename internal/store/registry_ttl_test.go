package store

import (
	"testing"
	"time"
)

// 成功结论的 TTL 边界：恰好 >= now-TTL 有效，超过一秒失效。
// C6 的 expired 派生必须沿用同一比较方式。
func TestRegistryCandidatesTTLBoundary(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)

	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)

	st.SetClock(func() time.Time { return base.Add(24 * time.Hour) })
	got, err := st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; !ok {
		t.Fatal("恰好到 TTL 边界应仍然有效")
	}

	st.SetClock(func() time.Time { return base.Add(24*time.Hour + time.Second) })
	got, err = st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; ok {
		t.Fatal("超过 TTL 后不得继续获得候选资格")
	}
}

// recovery.gen 与当前凭据代次不匹配时，旧成功结论不得复用。
func TestRegistryCandidatesRejectStaleGen(t *testing.T) {
	st, _ := newTestStore(t)
	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	if _, err := st.db.Exec(`INSERT INTO registry_host_gen(host, gen, cred_digest, updated_at)
		VALUES('quay.io', 3, '', 1) ON CONFLICT(host) DO UPDATE SET gen=3`); err != nil {
		t.Fatalf("seed gen: %v", err)
	}
	got, err := st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; ok {
		t.Fatal("recovery.gen=0 落后于当前代次，不应进入候选")
	}

	if _, err := st.db.Exec(`UPDATE registry_host_recovery SET gen=3 WHERE source_id=?`, id); err != nil {
		t.Fatal(err)
	}
	got, err = st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; !ok {
		t.Fatal("代次对齐后应进入候选")
	}
}

// API 直调入口必须经过 Guard：被屏蔽的源即使 SQL 条件全部满足也不返回。
func TestRegistryCandidatesApplyGuard(t *testing.T) {
	st, _ := newTestStore(t)
	g := NewRecoveryGuard()
	if err := st.SetRecoveryGuard(g); err != nil {
		t.Fatal(err)
	}
	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)

	tok := g.Block(GuardKey{SourceID: id, Family: "registry", Scope: "quay.io"}, 0)
	got, err := st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; ok {
		t.Fatal("被 Guard 屏蔽的源不应出现在 API 直调候选里")
	}

	if !g.UnblockIfMatch(GuardKey{SourceID: id, Family: "registry", Scope: "quay.io"}, tok) {
		t.Fatal("解除失败")
	}
	got, err = st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; !ok {
		t.Fatal("解除屏蔽后应恢复候选资格")
	}
}

// 装配约束：nil Guard 必须报错，不允许静默不过滤。
func TestSetRecoveryGuardRejectsNil(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.SetRecoveryGuard(nil); err == nil {
		t.Fatal("nil Guard 应返回错误")
	}
}
