package main

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/zggnh12345/mirror-lens/internal/store/schema"
)

// canonical357DDL 是 3.5.7 的冻结输入结构，不能跟着
// internal/store/schema/canonical.sql 一起升级。它是 11b 迁移测试的旧库夹具。
//
//go:embed canonical_3.5.7.sql
var canonical357DDL string

const canonical357Hash = "7b984881e7875f712b9a71162cc9d3615690efe42a6f5db1185c1e5138e8f51e"

// TestMigrate357To3511 是 3.5.7 -> 3.5.11 的迁移骨架。
//
// 11b 尚未落地时跳过；当 schema.Version 变成 3.5.11 后自动启用。
// 它只验证迁移主链和策略覆盖，不替代验收文档里的真实备份、故障注入和旧二进制检查。
func TestMigrate357To3511(t *testing.T) {
	if schema.Version != "3.5.11" {
		t.Skipf("11b 尚未落地：当前 schema.Version=%s", schema.Version)
	}

	srcPath := filepath.Join(t.TempDir(), "src-3.5.7.db")
	build357Fixture(t, srcPath)
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeSum := sha256.Sum256(before)

	outPath := filepath.Join(t.TempDir(), "out-3.5.11.db")
	if err := runMigrate([]string{"--in", srcPath, "--out", outPath, "--baseline", "3.5.7"}); err != nil {
		t.Fatalf("3.5.7 迁移失败: %v", err)
	}

	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256.Sum256(after); got != beforeSum {
		t.Fatal("迁移不得修改 3.5.7 源库")
	}

	db, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := schema.Verify(db); err != nil {
		t.Fatalf("目标库未通过 Canonical 校验: %v", err)
	}
	if err := checkIntegrity(db); err != nil {
		t.Fatalf("目标库完整性检查失败: %v", err)
	}

	var version string
	if err := db.QueryRow(`SELECT schema_version FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "3.5.11" {
		t.Fatalf("目标 schema_version=%q，期望 3.5.11", version)
	}

	// 已知 Host 必须种子化。自定义 Host 按 1.6 口径不自动补策略，
	// 后续由迁移报告/11c 处理；这里先锁住“不能静默补官方 Realm”。
	assertPolicyRow(t, db, 1, "quay.io")
	assertPolicyRow(t, db, 2, "public.ecr.aws")
	assertPolicyRow(t, db, 3, "lscr.io")
	if hasPolicyRow(t, db, 4, "custom.example") {
		t.Fatal("自定义 Host 不应被静默补成策略")
	}
	assertPolicyRealmContains(t, db, 3, "https://ghcr.io")
}

// build357Fixture 构造一个最小但结构完整的 3.5.7 源库。
// 结构必须来自冻结 DDL，而不是当前的 schema.DDL()。
func build357Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(canonical357DDL); err != nil {
		t.Fatalf("创建 3.5.7 结构失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_meta(schema_version,schema_hash,baseline_version,applied_at)
		VALUES('3.5.7',?,'prod-copy-3.5.7',1700000000)`, canonical357Hash); err != nil {
		t.Fatalf("写 schema_meta: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sources(base_url,name,status,enabled,repo_host,registry_cap,created_at) VALUES
		('https://quay.example/','quay','usable',1,'quay.io',1,100),
		('https://daocloud.example/','daocloud','usable',1,'public.ecr.aws',1,101),
		('https://lscr.example/','lscr','usable',1,'lscr.io',1,102),
		('https://custom.example/','custom','usable',1,'custom.example',1,103)`); err != nil {
		t.Fatalf("写 3.5.7 源数据: %v", err)
	}
}

func hasPolicyRow(t *testing.T, db *sql.DB, sourceID int64, host string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM source_realm_policy WHERE source_id=? AND host=?`, sourceID, host).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func assertPolicyRow(t *testing.T, db *sql.DB, sourceID int64, host string) {
	t.Helper()
	var primary, extra string
	var gen int64
	if err := db.QueryRow(`SELECT primary_realm, extra_realms, policy_gen
		FROM source_realm_policy WHERE source_id=? AND host=?`, sourceID, host).Scan(&primary, &extra, &gen); err != nil {
		t.Fatalf("source_id=%d host=%s 缺少策略: %v", sourceID, host, err)
	}
	if primary == "" || !strings.HasPrefix(primary, "https://") {
		t.Fatalf("source_id=%d host=%s 的 primary_realm 非法: %q", sourceID, host, primary)
	}
	if gen <= 0 {
		t.Fatalf("source_id=%d host=%s 的 policy_gen 必须 >0，实际 %d", sourceID, host, gen)
	}
	if !json.Valid([]byte(extra)) {
		t.Fatalf("source_id=%d host=%s 的 extra_realms 不是合法 JSON: %q", sourceID, host, extra)
	}
}

func assertPolicyRealmContains(t *testing.T, db *sql.DB, sourceID int64, want string) {
	t.Helper()
	var primary, extra string
	if err := db.QueryRow(`SELECT primary_realm, extra_realms FROM source_realm_policy WHERE source_id=?`, sourceID).
		Scan(&primary, &extra); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(primary, want) && !strings.Contains(extra, want) {
		t.Fatalf("source_id=%d 的 primary=%q extra=%q 都不包含 %s", sourceID, primary, extra, want)
	}
}
