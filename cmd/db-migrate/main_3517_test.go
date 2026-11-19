package main

import (
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store/schema"
)

// 3.5.14 → 3.5.17 迁移与回滚演练（只在隔离临时库，不触碰生产）。
//
// 3.5.14 fixture 的构造方式：先建 3.5.17 Canonical 空库，再删掉 F08 三张表
// 与其索引，并把 schema_meta 写回 3.5.14 的版本/hash。这样得到的结构
// 必须与已冻结的 3.5.14 指纹逐字节一致——否则测试自己就先失败。
const (
	fingerprint3514 = "7e5d71a711609eeb3d5e0cc883f1b5163268ff84e904a79370a2af93e0c1e28c"
)

func build3514Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := schema.EnsureFresh(db, "test-3.5.17"); err != nil {
		db.Close()
		t.Fatalf("建空库: %v", err)
	}
	stmts := []string{
		`DROP INDEX idx_trusted_mapping_audit_host`,
		`DROP INDEX idx_trusted_source_mappings_lookup`,
		`DROP TABLE trusted_mapping_audit`,
		`DROP TABLE trusted_mapping_generation`,
		`DROP TABLE trusted_source_mappings`,
		// 3.5.17 的跨 Registry 副本三表同样是本轮新增，3.5.14 结构里没有。
		`DROP INDEX idx_trusted_replica_audit_authority`,
		`DROP INDEX idx_trusted_registry_replicas_lookup`,
		`DROP TABLE trusted_replica_audit`,
		`DROP TABLE trusted_replica_generation`,
		`DROP TABLE trusted_registry_replicas`,
		`UPDATE schema_meta SET schema_version='3.5.14', schema_hash=?,
			baseline_version='test-3.5.14'`,
	}
	for i, q := range stmts {
		var err error
		if i == len(stmts)-1 {
			_, err = db.Exec(q, fingerprint3514)
		} else {
			_, err = db.Exec(q)
		}
		if err != nil {
			db.Close()
			t.Fatalf("构造 3.5.14 fixture: %v（%s）", err, q)
		}
	}
	// 造一点业务数据，验证迁移后逐表保留。
	if _, err := db.Exec(`INSERT INTO sources(id, base_url, name, created_at) VALUES(1,'https://mirror.example.org','m',1)`); err != nil {
		db.Close()
		t.Fatalf("写源: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO source_host_caps
		(source_id, host, enabled, last_verdict, effective_status, registry_path_mode, registry_prefix, diagnostics)
		VALUES(1,'quay.io',1,'Supported','Supported','native',0,'')`); err != nil {
		db.Close()
		t.Fatalf("写能力行: %v", err)
	}
	h, err := schema.Hash(db)
	if err != nil {
		db.Close()
		t.Fatalf("算指纹: %v", err)
	}
	if h != fingerprint3514 {
		db.Close()
		t.Fatalf("fixture 结构不是 3.5.14：实际=%s 期望=%s", h, fingerprint3514)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrate3514To3517AndRollback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-3.5.14.db")
	build3514Fixture(t, src)

	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	beforeSHA := sha256.Sum256(before)

	out := filepath.Join(dir, "out-3.5.17.db")
	if err := runMigrate([]string{"--in", src, "--out", out, "--baseline", "3.5.14"}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 源库必须一个字节都不变（回滚就是继续用它）。
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if beforeSHA != sha256.Sum256(after) {
		t.Fatal("迁移修改了源库文件")
	}

	dst, err := sql.Open("sqlite", out)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	dst.SetMaxOpenConns(1)
	meta, err := schema.Verify(dst)
	if err != nil {
		t.Fatalf("目标库未通过 Canonical 校验: %v", err)
	}
	if meta.SchemaVersion != "3.5.17" {
		t.Fatalf("目标版本应为 3.5.17，实得 %s", meta.SchemaVersion)
	}
	// 业务数据保留。
	var n int
	if err := dst.QueryRow(`SELECT count(*) FROM sources WHERE id=1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sources 未保留: n=%d err=%v", n, err)
	}
	if err := dst.QueryRow(`SELECT count(*) FROM source_host_caps WHERE host='quay.io'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("source_host_caps 未保留: n=%d err=%v", n, err)
	}
	// F08：Generation 按已配置 Host 建行；映射与审计为空（显式关系不能推断）。
	if err := dst.QueryRow(`SELECT generation FROM trusted_mapping_generation WHERE registry_host='quay.io'`).Scan(&n); err != nil {
		t.Fatalf("缺 Generation 行: %v", err)
	}
	if n < 1 {
		t.Fatalf("Generation 应 >=1，实得 %d", n)
	}
	if err := dst.QueryRow(`SELECT count(*) FROM trusted_source_mappings`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("映射迁移后应为空: n=%d err=%v", n, err)
	}
	if err := dst.QueryRow(`SELECT count(*) FROM trusted_mapping_audit`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("审计迁移后应为空: n=%d err=%v", n, err)
	}

	// 回滚演练：源库仍是 3.5.14 结构，新二进制对它会 fail closed（版本不匹配），
	// 但数据可读、结构指纹未变——这就是「继续用旧库」的回滚路径。
	old, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	h, err := schema.Hash(old)
	if err != nil {
		t.Fatal(err)
	}
	if h != fingerprint3514 {
		t.Fatalf("回滚后结构指纹变化: %s", h)
	}
	if _, err := schema.Verify(old); err == nil {
		t.Fatal("新二进制不应接受 3.5.14 库，必须 fail closed")
	}
	if err := old.QueryRow(`SELECT count(*) FROM sources`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("旧库数据不可读: n=%d err=%v", n, err)
	}
}
