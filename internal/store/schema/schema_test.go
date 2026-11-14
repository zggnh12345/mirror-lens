package schema

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func open(t *testing.T) (*sql.DB, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, p
}

func TestEnsureFreshAndVerify(t *testing.T) {
	db, _ := open(t)
	if err := EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	m, err := Verify(db)
	if err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != Version || m.BaselineVersion != "fresh" {
		t.Fatalf("meta=%+v", m)
	}
	h1, err := Hash(db)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != m.SchemaHash {
		t.Fatalf("hash=%s meta=%s", h1, m.SchemaHash)
	}
	if h1 != CanonicalSchemaHash {
		t.Fatalf("Canonical 新库 hash=%s，冻结值=%s", h1, CanonicalSchemaHash)
	}
	// 再开一次 hash 必须稳定。
	m2, err := Verify(db)
	if err != nil || m2.SchemaHash != h1 {
		t.Fatalf("第二次校验失败: %+v %v", m2, err)
	}
}

func TestEnsureFreshRefusesNonEmpty(t *testing.T) {
	db, _ := open(t)
	if _, err := db.Exec(`CREATE TABLE x(id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFresh(db, "fresh"); err == nil {
		t.Fatal("非空库不应执行 EnsureFresh")
	}
}

func TestHashDetectsSchemaChange(t *testing.T) {
	db, _ := open(t)
	if err := EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE sources ADD COLUMN unexpected TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(db); err == nil {
		t.Fatal("结构变化后 Verify 应失败")
	}
}

// 结构被改动后，即使把 schema_meta.schema_hash 也改成新的自洽值，也必须拒绝。
// 这正是 overlay 发现过的运行时缺口：自洽不等于 Canonical。
func TestVerifyRejectsSelfConsistentNonCanonical(t *testing.T) {
	db, _ := open(t)
	if err := EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE sources ADD COLUMN extra_col TEXT`); err != nil {
		t.Fatal(err)
	}
	h, err := Hash(db)
	if err != nil {
		t.Fatal(err)
	}
	if h == CanonicalSchemaHash {
		t.Fatal("测试前提错误：改动结构后 hash 不应等于 Canonical")
	}
	if _, err := db.Exec(`UPDATE schema_meta SET schema_hash=?`, h); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(db); err == nil {
		t.Fatal("自洽但非 Canonical 的结构必须被 Verify 拒绝")
	}
}

// 实际结构是 Canonical，但 schema_meta 声明的 hash 被篡改时，必须拒绝。
func TestVerifyRejectsTamperedMetadataHash(t *testing.T) {
	db, _ := open(t)
	if err := EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_meta SET schema_hash='deadbeef'`); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(db); err == nil {
		t.Fatal("schema_meta.schema_hash 不是 Canonical 时必须拒绝")
	}
}

// 实际结构和 metadata hash 都正确，但版本错误时，必须拒绝。
func TestVerifyRejectsWrongVersion(t *testing.T) {
	db, _ := open(t)
	if err := EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_meta SET schema_version='3.5.6'`); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(db); err == nil {
		t.Fatal("版本不是 3.5.7 时必须拒绝")
	}
}

func TestCanonicalHasNoIdentityOrWritable(t *testing.T) {
	db, _ := open(t)
	if err := EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('sources') WHERE name='registry_identity_state'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("canonical 不应包含 registry_identity_state")
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='_writable'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("canonical 不应包含 _writable")
	}
	// 新库必须保留审计表的空表结构；不能因为“没有旧历史”就把它当成
	// schema_migrations 继续执行。
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations_audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fresh 库的审计表应为空，实际 %d 行", n)
	}
	// 新库不能再出现旧的迁移执行表名；审计事实只进 schema_migrations_audit。
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='schema_migrations'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("Canonical 不应包含旧的 schema_migrations 执行表")
	}
}

func TestReadMetaRequiresExactlyOneRow(t *testing.T) {
	db, _ := open(t)
	if err := EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeta(db); err != nil {
		t.Fatalf("单记录应可读: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_meta`); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeta(db); err == nil {
		t.Fatal("零条 schema_meta 必须拒绝启动")
	}
	// DDL 的 CHECK(id=1) 保证正常库不能出现第二行；这里重建一个不带
	// 约束的同名表，专门验证 ReadMeta 自身的行数检查（而不是只依赖 DDL）。
	if _, err := db.Exec(`DROP TABLE schema_meta`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_meta (schema_version TEXT NOT NULL, schema_hash TEXT NOT NULL, baseline_version TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_meta VALUES ('a','b','c',1),('d','e','f',2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeta(db); err == nil {
		t.Fatal("多条 schema_meta 必须拒绝启动")
	}
}

func TestSchemaMetaSingleRowConstraint(t *testing.T) {
	db, _ := open(t)
	if err := EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_meta(id,schema_version,schema_hash,baseline_version,applied_at) VALUES (2,'x','y','z',0)`); err == nil {
		t.Fatal("schema_meta 的 id=1 约束必须拒绝第二行")
	}
}

func TestEnsureFreshIsAtomic(t *testing.T) {
	db, _ := open(t)
	// 用同名 view 让 Canonical DDL 在事务中途失败；若 DDL 与 meta 不在同一
	// 事务，残留的 Canonical 表会污染后续启动。
	if _, err := db.Exec(`CREATE VIEW schema_meta AS SELECT 1 AS x`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFresh(db, "fresh"); err == nil {
		t.Fatal("DDL 冲突时 EnsureFresh 应失败")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sources'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("初始化失败后不应留下半成品 Canonical 表")
	}
}
