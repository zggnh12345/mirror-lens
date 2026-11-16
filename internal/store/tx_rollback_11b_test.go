package store

import (
	"testing"
	"time"
)

// §3.1：禁用/启用失败时，status 与 policy_gen 必须一起回滚。
func TestUpdateSourceRollsBackWhenPolicyBumpFails(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_policy_bump BEFORE UPDATE ON source_realm_policy
		BEGIN SELECT RAISE(ABORT, 'inject policy bump'); END`); err != nil {
		t.Fatal(err)
	}
	off := false
	if err := st.UpdateSource(id, &off, nil, nil, nil); err == nil {
		t.Fatal("注入策略递增失败时 UpdateSource 应失败")
	}
	s, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled {
		t.Fatal("禁用失败后 enabled 应保持 1")
	}
	after, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if after.PolicyGen != before.PolicyGen {
		t.Fatalf("禁用失败后 policy_gen 应不变: %d -> %d", before.PolicyGen, after.PolicyGen)
	}
}

// §3.1：归档失败时，status 与 policy_gen 必须一起回滚。
func TestArchiveStaleRollsBackWhenPolicyBumpFails(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "dead", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	if _, err := st.db.Exec(`UPDATE sources SET status_at=? WHERE id=?`, old, id); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_policy_bump BEFORE UPDATE ON source_realm_policy
		BEGIN SELECT RAISE(ABORT, 'inject policy bump'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ArchiveStale(time.Now().Add(-14 * 24 * time.Hour).Unix()); err == nil {
		t.Fatal("注入策略递增失败时 ArchiveStale 应失败")
	}
	s, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status == "archived" {
		t.Fatal("归档失败后 status 不应变成 archived")
	}
	after, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if after.PolicyGen != before.PolicyGen {
		t.Fatalf("归档失败后 policy_gen 应不变: %d -> %d", before.PolicyGen, after.PolicyGen)
	}
}

// §3.1：删除 Source 失败时，源和策略都必须还在。
func TestDeleteSourceRollsBackWhenPolicyDeleteFails(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_policy_delete BEFORE DELETE ON source_realm_policy
		BEGIN SELECT RAISE(ABORT, 'inject policy delete'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSource(id); err == nil {
		t.Fatal("注入策略删除失败时 DeleteSource 应失败")
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM sources WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("删除失败后源应保留，实际 %d 条", n)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM source_realm_policy WHERE source_id=? AND host='quay.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("删除失败后 quay.io 策略应保留，实际 %d 条", n)
	}
}

// §1/§3.1：同一次 UpdateSource 里 enabled 与 name 必须同一事务；
// name 更新失败时 enabled 和 policy_gen 都要回滚。
func TestUpdateSourceRollsBackMetadataWhenNameFails(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_name BEFORE UPDATE OF name ON sources
		BEGIN SELECT RAISE(ABORT, 'inject name'); END`); err != nil {
		t.Fatal(err)
	}
	off := false
	newName := "renamed"
	if err := st.UpdateSource(id, &off, &newName, nil, nil); err == nil {
		t.Fatal("注入 name 更新失败时 UpdateSource 应失败")
	}
	s, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled {
		t.Fatal("name 更新失败后 enabled 应回滚为 1")
	}
	if s.Name != "q" {
		t.Fatalf("name 不应被写入，实际 %q", s.Name)
	}
	after, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if after.PolicyGen != before.PolicyGen {
		t.Fatalf("name 更新失败后 policy_gen 应回滚: %d -> %d", before.PolicyGen, after.PolicyGen)
	}
}
