package store

import (
	"context"
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

// 新建的 Canonical 库里 registry_host_recovery 必须是三列主键并带 idx_recovery_due。
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

// 已写入的 scoped 数据在第二次 Open 后必须原样可读，不被运行时改动。
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
