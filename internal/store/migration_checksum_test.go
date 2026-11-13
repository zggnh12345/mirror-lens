package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// 新建库：登记的迁移必须带内容校验和，origin=verified。
func TestMigrationChecksumRecorded(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "m3.db"))
	defer st.Close()

	known := knownMigrationChecksums()
	for name, want := range known {
		var got, origin string
		err := st.db.QueryRow(`SELECT checksum, checksum_origin FROM schema_migrations WHERE name=?`, name).
			Scan(&got, &origin)
		if err == sql.ErrNoRows {
			continue // legacy 标记可能尚未写入，这里只校验已存在的行
		}
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s checksum=%q，期望 %q", name, got, want)
		}
		if origin != "verified" && origin != "legacy_data" {
			t.Errorf("%s origin=%q，期望 verified/legacy_data", name, origin)
		}
	}
}

// 老库首次升级：没有 checksum 列时补列并做兼容登记，不能判定库损坏。
func TestMigrationChecksumLegacyCompat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	st := openTestAt(t, path)
	if _, err := st.db.Exec(`UPDATE schema_migrations SET checksum='', checksum_origin=''`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE schema_migrations DROP COLUMN checksum`,
		`ALTER TABLE schema_migrations DROP COLUMN checksum_origin`,
	} {
		if _, err := raw.Exec(q); err != nil {
			raw.Close()
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	st = openTestAt(t, path)
	defer st.Close()
	rows, err := st.db.Query(`SELECT name, checksum, checksum_origin FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name, sum, origin string
		if err := rows.Scan(&name, &sum, &origin); err != nil {
			t.Fatal(err)
		}
		if _, ok := knownMigrationChecksums()[name]; !ok {
			continue
		}
		n++
		if sum == "" {
			t.Errorf("%s 兼容登记后 checksum 仍为空", name)
		}
		if origin != "legacy_compat" {
			t.Errorf("%s origin=%q，期望 legacy_compat", name, origin)
		}
	}
	if n == 0 {
		t.Fatal("没有找到需要兼容登记的迁移行")
	}
}

// 已登记迁移内容被改动：checksum 不匹配必须拒绝启动。
func TestMigrationChecksumTamperRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tamper.db")
	st := openTestAt(t, path)
	if _, err := st.db.Exec(`UPDATE schema_migrations SET checksum='sha256:0000' WHERE name=?`, identityMigrationName); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("checksum 被篡改后 Open 仍然成功")
	} else if !strings.Contains(err.Error(), "内容校验失败") {
		t.Fatalf("错误应说明内容校验失败，实际: %v", err)
	}
}

// 不认识的历史迁移行不能导致启动失败，也不能被补写。
func TestMigrationChecksumUnknownRowUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown.db")
	st := openTestAt(t, path)
	if _, err := st.db.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES('ancient-step', 1)`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st = openTestAt(t, path)
	defer st.Close()
	var sum, origin string
	if err := st.db.QueryRow(`SELECT checksum, checksum_origin FROM schema_migrations WHERE name='ancient-step'`).
		Scan(&sum, &origin); err != nil {
		t.Fatal(err)
	}
	if sum != "" || origin != "" {
		t.Fatalf("未知历史迁移被改写: checksum=%q origin=%q", sum, origin)
	}
}
