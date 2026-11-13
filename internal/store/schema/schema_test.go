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
}
