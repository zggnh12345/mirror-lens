package store

import "testing"

// §15.4 AddSourceHost 独立事务：任一写入失败，前面已写的
// source_host_caps / health_scope / recovery / 事件全部回滚。
// 用 SQLite BEFORE INSERT 触发器在 source_realm_policy 注入失败
// （该表写入在 source_host_caps 之后，可验证前序写入被回滚）。
func TestG4AddSourceHostRollsBackAllWrites(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example", Name: "tx", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// 触发器：策略行写入时 ABORT，制造事务中途失败。
	if _, err := st.db.Exec(`CREATE TRIGGER fail_policy BEFORE INSERT ON source_realm_policy
		BEGIN SELECT RAISE(ABORT, 'injected policy failure'); END;`); err != nil {
		t.Fatal(err)
	}
	defer st.db.Exec(`DROP TRIGGER fail_policy`)

	if err := st.AddSourceHost(id, "quay.io", "https://quay.io", nil); err == nil {
		t.Fatal("注入失败后 AddSourceHost 应报错")
	}

	// source_host_caps 在触发器之前已写；回滚后不应残留。
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_host_caps WHERE source_id=? AND host='quay.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("事务失败后 source_host_caps 不应残留，得到 %d 行", n)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_realm_policy WHERE source_id=? AND host='quay.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("事务失败后 source_realm_policy 不应残留，得到 %d 行", n)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM registry_host_recovery WHERE source_id=? AND scope='quay.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("事务失败后 registry_host_recovery 不应残留，得到 %d 行", n)
	}
}

// 同一注入在事件写入阶段：验证事件表也不留半成品。
func TestG4AddSourceHostRollsBackEvent(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example", Name: "tx2", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON source_events
		BEGIN SELECT RAISE(ABORT, 'injected event failure'); END;`); err != nil {
		t.Fatal(err)
	}
	defer st.db.Exec(`DROP TRIGGER fail_event`)
	if err := st.AddSourceHost(id, "quay.io", "https://quay.io", nil); err == nil {
		t.Fatal("事件写入注入失败后应报错")
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_host_caps WHERE source_id=? AND host='quay.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("事件失败后 source_host_caps 不应残留，得到 %d 行", n)
	}
}
