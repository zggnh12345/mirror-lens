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
	got, err := st.SourceFailureRuns("docker", 64)
	if err != nil {
		t.Fatalf("查失败事件: %v", err)
	}
	if r, bad := got[id]; bad && r.Fails > 0 {
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

// rev 守卫：旧版本号不能覆盖新状态，即使墙上时钟更晚。
func TestBreakerRevGuardRejectsStaleWrite(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://a.example", Name: "a", DockerCap: 1, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	now := time.Now()
	write := func(fails int, rev, at int64) {
		if err := st.RecordOutcome(OutcomeRecord{SourceID: id, Kind: "docker", OK: false, Code: 429,
			Breaker: &BreakerState{SourceID: id, Kind: "docker", Fails: fails, Until: now.Add(time.Minute).Unix(), Rev: rev, UpdatedAt: at}}); err != nil {
			t.Fatalf("写状态: %v", err)
		}
	}
	write(2, 7, now.Add(-time.Hour).UnixNano()) // 新状态，但时钟被回拨
	write(9, 3, now.UnixNano())                 // 旧状态，时钟却更晚
	rows, err := st.LoadBreakerStates()
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	if len(rows) != 1 || rows[0].Fails != 2 || rows[0].Rev != 7 {
		t.Fatalf("旧 revision 不应覆盖，实际 %+v", rows)
	}
}

// 墓碑（fails=0）也要把 rev 保留下来，重启后的新写入才不会被挡住。
func TestTombstoneKeepsRev(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://a.example", Name: "a", DockerCap: 1, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	now := time.Now()
	for _, rw := range []struct {
		fails int
		rev   int64
	}{{0, 4}, {1, 5}} {
		if err := st.RecordOutcome(OutcomeRecord{SourceID: id, Kind: "docker", OK: rw.fails == 0, Code: 429,
			Breaker: &BreakerState{SourceID: id, Kind: "docker", Fails: rw.fails, Rev: rw.rev, UpdatedAt: now.UnixNano()}}); err != nil {
			t.Fatalf("写状态: %v", err)
		}
	}
	rows, _ := st.LoadBreakerStates()
	if len(rows) != 1 || rows[0].Fails != 1 || rows[0].Rev != 5 {
		t.Fatalf("重启后 rev 应从墓碑续上，实际 %+v", rows)
	}
}

// 旧版把 404 写成 ok=0 时，也不能进入失败连击。
func TestLegacyFailFlagWithNeutralCode(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://a.example", Name: "a", DockerCap: 1, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	for _, code := range []int{401, 403, 404, 410} {
		st.RecordSourceEvent(id, "docker", false, 5, code)
	}
	runs, err := st.SourceFailureRuns("docker", 64)
	if err != nil {
		t.Fatalf("查连击: %v", err)
	}
	if r := runs[id]; r.Fails != 0 {
		t.Fatalf("ok=0 的中性状态码不该算失败，实际 %+v", r)
	}
	st.RecordSourceEvent(id, "docker", false, 5, 429)
	if runs, _ = st.SourceFailureRuns("docker", 64); runs[id].Fails != 1 {
		t.Fatalf("429 应算一次失败，实际 %+v", runs[id])
	}
}

// 历史事件没有 neutral 列，三态统计要按状态码兼容。
func TestSourceEventsLegacyCounts(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://a.example", Name: "a", DockerCap: 1, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	st.RecordSourceEvent(id, "docker", false, 5, 404)
	st.RecordSourceEvent(id, "docker", false, 5, 401)
	st.RecordSourceEvent(id, "docker", true, 5, 404)
	st.RecordSourceEvent(id, "docker", true, 5, 200)
	st.RecordSourceEvent(id, "docker", false, 5, 500)
	okN, neutralN, failN, _, err := st.SourceEvents(id, 20)
	if err != nil {
		t.Fatalf("统计: %v", err)
	}
	if okN != 1 || neutralN != 3 || failN != 1 {
		t.Fatalf("三态计数应为 1/3/1，实际 %d/%d/%d", okN, neutralN, failN)
	}
}
