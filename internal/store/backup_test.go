package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupToProducesUsableCopy(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	out := filepath.Join(dir, "snap.db")
	if err := st.BackupTo(out); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("快照没生成: %v size=%d", err, fi.Size())
	}
	b, err := sql.Open("sqlite", "file:"+out+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var name string
	if err := b.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name LIMIT 1`).Scan(&name); err != nil {
		t.Fatalf("快照不是可读数据库: %v", err)
	}
	// 目标已存在时必须报错：VACUUM INTO 不会覆盖，交给调用方处理。
	if err := st.BackupTo(out); err == nil {
		t.Fatal("目标已存在时应报错")
	}
}
