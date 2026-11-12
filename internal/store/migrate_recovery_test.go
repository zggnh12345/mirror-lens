package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// downgradeScoped 把新库退回 3.5.1 的三列主键形态，并插入 n 行历史数据。
func downgradeScoped(t *testing.T, s *Store, n int) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO sources(base_url,name,created_at) VALUES(?,?,?)`,
		"https://old.example", "old", 1)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last id: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE source_health_scope`); err != nil {
		t.Fatalf("drop scoped: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE source_health_scope (
		source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		family TEXT NOT NULL,
		scope TEXT NOT NULL DEFAULT '',
		fails INTEGER NOT NULL DEFAULT 0,
		ok_at INTEGER NOT NULL DEFAULT 0,
		last_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(source_id, family, scope))`); err != nil {
		t.Fatalf("create legacy scoped: %v", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX idx_scoped_legacy ON source_health_scope(family)`); err != nil {
		t.Fatalf("create legacy index: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := s.db.Exec(`INSERT INTO source_health_scope VALUES(?,?,?,?,?,?)`,
			id, "registry", fmt.Sprintf("scope-%d", i), i+1, 100+i, 200+i); err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
	}
	return id
}

func TestMigrateScopedGenLegacy(t *testing.T) {
	s, _ := openTestStore(t)
	downgradeScoped(t, s, 3)

	if err := s.migrateScopedGen(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	cols, pk, err := pkColumns(ctx, s.db, "source_health_scope")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["gen"] || pk != "source_id,family,scope,gen" {
		t.Fatalf("主键不符: cols=%v pk=%q", cols, pk)
	}

	var total, gen0 int
	s.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope`).Scan(&total)
	s.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope WHERE gen=0`).Scan(&gen0)
	if total != 3 || gen0 != 3 {
		t.Fatalf("行数/gen 不符: total=%d gen0=%d", total, gen0)
	}

	var idx int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_scoped_legacy'`).Scan(&idx)
	if idx != 1 {
		t.Fatal("旧二级索引未保留")
	}

	// 外键仍然生效：删源应级联清 scoped。
	if _, err := s.db.Exec(`DELETE FROM sources`); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope`).Scan(&total)
	if total != 0 {
		t.Fatalf("级联删除失效，剩余 %d 行", total)
	}
}

func TestMigrateScopedGenIdempotent(t *testing.T) {
	s, _ := openTestStore(t)
	downgradeScoped(t, s, 2)

	if err := s.migrateScopedGen(); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='source_health_scope'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateScopedGen(); err != nil {
		t.Fatalf("第二次迁移: %v", err)
	}
	var after string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='source_health_scope'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("第二次迁移重建了表:\nbefore=%s\nafter=%s", before, after)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope`).Scan(&n)
	if n != 2 {
		t.Fatalf("数据丢失，剩 %d 行", n)
	}
}

func TestMigrateScopedGenRollback(t *testing.T) {
	s, _ := openTestStore(t)
	downgradeScoped(t, s, 4)

	scopedMigrationHook = func(stage string) error {
		if stage == "before_drop" {
			return errors.New("injected failure")
		}
		return nil
	}
	defer func() { scopedMigrationHook = nil }()

	if err := s.migrateScopedGen(); err == nil {
		t.Fatal("注入失败后仍返回成功")
	}

	ctx := context.Background()
	_, pk, err := pkColumns(ctx, s.db, "source_health_scope")
	if err != nil {
		t.Fatal(err)
	}
	if pk != "source_id,family,scope" {
		t.Fatalf("回滚后旧表结构被改: pk=%q", pk)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope`).Scan(&n)
	if n != 4 {
		t.Fatalf("回滚后数据不一致: %d 行", n)
	}
	var leftovers int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='source_health_scope_new'`).Scan(&leftovers)
	if leftovers != 0 {
		t.Fatal("回滚后残留 source_health_scope_new")
	}
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("失败路径未恢复 foreign_keys: %d", fk)
	}
}

func TestMigrateRecoverySchemaFresh(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	_, pk, err := pkColumns(ctx, s.db, "registry_host_recovery")
	if err != nil {
		t.Fatal(err)
	}
	if pk != "source_id,family,scope" {
		t.Fatalf("recovery 主键不符: %q", pk)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_recovery_due'`).Scan(&n)
	if n != 1 {
		t.Fatal("缺 idx_recovery_due")
	}
}

func TestOpenTwiceKeepsScopedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s1.db.Exec(`INSERT INTO sources(base_url,name,created_at) VALUES(?,?,?)`,
		"https://twice.example", "twice", 1)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := s1.db.Exec(`INSERT INTO source_health_scope(source_id,family,scope,gen,fails,ok_at,last_at)
		VALUES(?,?,?,?,?,?,?)`, id, "registry", "", 7, 1, 10, 20); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("第二次 Open: %v", err)
	}
	defer s2.Close()
	var gen, fails int
	if err := s2.db.QueryRow(`SELECT gen,fails FROM source_health_scope WHERE source_id=? AND scope=''`, id).
		Scan(&gen, &fails); err != nil {
		t.Fatalf("第二次启动后读不到数据: %v", err)
	}
	if gen != 7 || fails != 1 {
		t.Fatalf("数据被改: gen=%d fails=%d", gen, fails)
	}
}
