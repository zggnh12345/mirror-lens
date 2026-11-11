package store

import (
	"path/filepath"
	"testing"
	"time"
)

// 升级后的老库：source_events 多个 neutral 列（默认 0），并出现 breaker_state。
func TestSchemaAddsNeutralAndBreakerState(t *testing.T) {
	st := openTest(t)

	var neutral string
	rows, err := st.db.Query(`PRAGMA table_info(source_events)`)
	if err != nil {
		t.Fatalf("读表结构: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("扫描表结构: %v", err)
		}
		if name == "neutral" {
			neutral = typ
		}
	}
	if neutral == "" {
		t.Fatal("source_events 缺 neutral 列")
	}

	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='breaker_state'`).Scan(&n); err != nil {
		t.Fatalf("查表: %v", err)
	}
	if n != 1 {
		t.Fatal("缺 breaker_state 表")
	}
}

// 旧版 3.4.6 的写入只给显式列名，不带 neutral：必须仍能写入，且默认按非中性处理。
func TestLegacyInsertWithoutNeutralColumn(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://a.example", Name: "a", DockerCap: 1, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO source_events(source_id, kind, ts, ok, ms, code)
		VALUES(?,?,?,?,?,?)`, id, "docker", time.Now().Unix(), 1, 12, 404); err != nil {
		t.Fatalf("旧版写入失败（多出的列必须可默认）: %v", err)
	}
	var neutral int
	if err := st.db.QueryRow(`SELECT neutral FROM source_events WHERE source_id=?`, id).Scan(&neutral); err != nil {
		t.Fatalf("读回: %v", err)
	}
	if neutral != 0 {
		t.Fatalf("历史事件 neutral 应为 0，实际 %d", neutral)
	}

	// 历史 404 不能恢复成失败。
	got, err := st.SourceFailedRecently("docker", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("查失败事件: %v", err)
	}
	if _, bad := got[id]; bad {
		t.Fatalf("历史 404 不该被当成失败: %v", got)
	}
}

// 事件与状态必须同生共死：状态写失败时，事件也要回滚。
func TestEventRollsBackWhenBreakerWriteFails(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://a.example", Name: "a", DockerCap: 1, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	if _, err := st.db.Exec(`DROP TABLE breaker_state`); err != nil {
		t.Fatalf("造故障: %v", err)
	}
	err = st.RecordOutcome(OutcomeRecord{
		SourceID: id, Kind: "docker", OK: false, Code: 429,
		Breaker: &BreakerState{SourceID: id, Kind: "docker", Fails: 1, Until: time.Now().Add(time.Minute).Unix(), UpdatedAt: time.Now().UnixNano()},
	})
	if err == nil {
		t.Fatal("状态表缺失时应返回错误")
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM source_events WHERE source_id=?`, id).Scan(&n); err != nil {
		t.Fatalf("查事件: %v", err)
	}
	if n != 0 {
		t.Fatalf("状态写失败后事件应回滚，实际留下 %d 条", n)
	}
}

// 旧库若此前已写好 breaker_state 之外的表，Open 必须幂等。
func TestOpenTwiceIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.db")
	st, err := Open(p)
	if err != nil {
		t.Fatalf("首次打开: %v", err)
	}
	st.Close()
	st2, err := Open(p)
	if err != nil {
		t.Fatalf("二次打开: %v", err)
	}
	defer st2.Close()
	if _, err := st2.LoadBreakerStates(); err != nil {
		t.Fatalf("读熔断状态: %v", err)
	}
}
