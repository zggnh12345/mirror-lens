package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/zggnh12345/mirror-lens/internal/store/schema"
)

//go:embed canonical_3.5.5.sql
var canonical355DDL string

// 输入指纹是冻结值，不能被空值/测试 fixture 意外替代。
func TestInputSchemaFingerprintsFrozen(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for baseline, h := range inputSchemaFingerprints {
		if !re.MatchString(h) {
			t.Errorf("baseline=%s 的输入指纹不是 64 位 sha256: %q", baseline, h)
		}
	}
	if len(inputSchemaFingerprints) != 3 {
		t.Fatalf("只允许冻结 3.4.6/3.5.4/3.5.5 三个输入指纹，实际 %d 个", len(inputSchemaFingerprints))
	}
	want := map[string]string{
		"3.4.6": "972db548d1eac569aec7ae11d859cdfbcd9114a0ee2dd1217ca085ca5a4d835f",
		"3.5.4": "4af3f62e273685b10493cafdeb7c6745b8d98d87e017dc1445b42ca66308cffb",
		"3.5.5": "0842cd1efa4b7330cf53a7f9d98491597481cfcec80d1b5560bf8f9f0ef81170",
	}
	for baseline, expected := range want {
		if got := inputSchemaFingerprints[baseline]; got != expected {
			t.Errorf("baseline=%s 的输入指纹不等于冻结值：实际=%s，期望=%s", baseline, got, expected)
		}
	}
	if _, ok := inputSchemaFingerprints["3.5.6"]; ok {
		t.Fatal("3.5.6 不应作为 migrate 输入指纹出现；3.5.6 走独立的 metadata-only 升级路径（upgrade356）")
	}
}

// M1：3.5.4 的 repo_host 必须原样复制（空串也是事实），不能按 Docker Hub
// 旧语义改写成 docker.io；3.4.6 没有该列时按能力显式映射：
// docker_cap=1 -> docker.io，仅 ghcr_cap=1 -> ghcr.io，其余空串。
func TestColumnExprPreservesRepoHostPerBaseline(t *testing.T) {
	expr, ok := columnExpr("3.5.4", "sources", "repo_host", map[string]bool{"repo_host": true}, nil)
	if !ok || expr != `"repo_host"` {
		t.Fatalf("3.5.4 repo_host 应原样复制，实际 expr=%q ok=%v", expr, ok)
	}
	expr, ok = columnExpr("3.4.6", "sources", "repo_host", map[string]bool{}, nil)
	if !ok {
		t.Fatal("3.4.6 缺列 repo_host 必须有显式映射")
	}
	if !strings.Contains(expr, "docker_cap") || !strings.Contains(expr, "'docker.io'") ||
		!strings.Contains(expr, "ghcr_cap") || !strings.Contains(expr, "'ghcr.io'") {
		t.Fatalf("3.4.6 repo_host 应显式按能力映射，实际 expr=%q", expr)
	}
}

// 验证 3.4.6 缺列映射的实际取值：34 条 docker 能力源必须成为 docker.io，
// 2 条仅 GHCR 源成为 ghcr.io，其余没有 Registry 归属。
func TestMissingRepoHostMapsByCapability(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sources (docker_cap INTEGER, ghcr_cap INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources(docker_cap,ghcr_cap) VALUES (1,1),(1,0),(-1,1),(-1,-1),(0,1)`); err != nil {
		t.Fatal(err)
	}
	expr, ok := missingTargetExpr("3.4.6", "sources", "repo_host")
	if !ok {
		t.Fatal("3.4.6 sources.repo_host 缺少映射")
	}
	rows, err := db.Query(`SELECT ` + expr + ` FROM sources ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	want := []string{"docker.io", "docker.io", "ghcr.io", "", "ghcr.io"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("3.4.6 repo_host 映射=%v，期望=%v", got, want)
	}
}

func TestColumnExprNormalizesKnownRegistryAlias(t *testing.T) {
	expr, ok := columnExpr("3.5.4", "sources", "repo_host", map[string]bool{"repo_host": true}, map[string]string{"registry-1.docker.io": "docker.io"})
	if !ok || !strings.Contains(expr, "CASE") || !strings.Contains(expr, "'registry-1.docker.io' THEN 'docker.io'") {
		t.Fatalf("已知别名应在复制时显式规范化，实际 expr=%q ok=%v", expr, ok)
	}
}

// M3：内容 hash 必须随值变化，不能只靠行数；这是 copyAll 的比对基础。
func TestHashRowsIncludesContent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t(id,v) VALUES (1,'a'),(2,'b')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	h1, err := hashRows(tx, "main", "t", `"id","v"`, 2, []string{`"id"`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE t SET v='changed' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	h2, err := hashRows(tx, "main", "t", `"id","v"`, 2, []string{`"id"`})
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("内容 hash 未随值变化，M3 校验失效")
	}
	tx.Rollback()
}

// M3：NULL、空串、整数、浮点、TEXT、BLOB 必须有不同编码；否则“内容哈希”
// 会在不同类型/空值之间产生歧义。
func TestHashRowsTypeEncodingIsUnambiguous(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v)`); err != nil {
		t.Fatal(err)
	}
	values := []any{
		nil,
		"",
		int64(0),
		float64(0),
		[]byte{},
		[]byte("0"),
		"0",
	}
	seen := make(map[string]any, len(values))
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, v := range values {
		if _, err := tx.Exec(`DELETE FROM t`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO t(id,v) VALUES (1,?)`, v); err != nil {
			t.Fatal(err)
		}
		got, err := hashRows(tx, "main", "t", `"id","v"`, 2, []string{`"id"`})
		if err != nil {
			t.Fatal(err)
		}
		if prev, ok := seen[got]; ok {
			t.Fatalf("值 %#v 与 %#v 的哈希碰撞: %s", v, prev, got)
		}
		seen[got] = v
	}
}

// M4：空 Host 合法；未知/非法 Host 必须在建输出库之前拒绝。
func TestValidateRegistryHosts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sources (repo_host TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources(repo_host) VALUES (''),('docker.io'),('ghcr.io')`); err != nil {
		t.Fatal(err)
	}
	hosts, err := validateRegistryHosts(db, "3.5.4")
	if err != nil {
		t.Fatalf("合法的空/ docker.io / ghcr.io 应通过: %v", err)
	}
	if len(hosts) != 2 || hosts["docker.io"] != "docker.io" || hosts["ghcr.io"] != "ghcr.io" {
		t.Fatalf("host 映射错误: %#v", hosts)
	}
	if _, err := db.Exec(`INSERT INTO sources(repo_host) VALUES ('unknown.example')`); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRegistryHosts(db, "3.5.4"); err == nil {
		t.Fatal("未知 Host 必须拒绝迁移")
	}
}

// 3.5.3 必须在打开输入、创建输出之前被拒绝；--force-baseline 不得绕过。
func TestMigrateRejects353BeforeWritingOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "must-not-exist.db")
	err := runMigrate([]string{
		"--in", filepath.Join(t.TempDir(), "does-not-matter.db"),
		"--out", out,
		"--baseline", "3.5.3",
		"--force-baseline",
	})
	if err == nil {
		t.Fatal("3.5.3 + --force-baseline 应被拒绝，实际返回 nil")
	}
	if !strings.Contains(err.Error(), "3.5.3") || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("错误信息没有说明 3.5.3 不支持: %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("拒绝发生在输出创建之后: stat=%v", statErr)
	}
}

// ---- 3.5.5 → 3.5.7 结构升级 + 数据原样复制保真测试 ----

// build355Fixture 用冻结的 3.5.5 Canonical DDL 构建一个真实输入库，并写入一行
// schema_meta（version=3.5.5，baseline_version 取可被验证的源值）和若干样本业务数据。
func build355Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(canonical355DDL); err != nil {
		t.Fatalf("创建 3.5.5 schema 失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_meta(schema_version,schema_hash,baseline_version,applied_at) VALUES('3.5.5','0842cd1efa4b7330cf53a7f9d98491597481cfcec80d1b5560bf8f9f0ef81170','src-baseline-3.5.5',1700000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources(base_url,name,created_at,repo_host) VALUES ('https://a.example/','A',1,''),('https://b.example/','B',2,'docker.io'),('https://c.example/','C',3,'ghcr.io')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO checks(source_id,ts,tier,ok) VALUES (1,100,1,1),(2,200,2,0),(3,300,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blobs(sha256) VALUES ('deadbeef'),('cafe')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO source_health(source_id,family) VALUES (1,'http'),(2,'https')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO breaker_state(source_id,kind) VALUES (1,'http'),(2,'https')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES ('k1','v1'),('k2','v2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO meta(k,v) VALUES ('m1','w1')`); err != nil {
		t.Fatal(err)
	}
}

// columnList 通过 PRAGMA 读取表的列名，用于构造 hashRows 的 SELECT 列表与排序键。
func columnList(t *testing.T, tx *sql.Tx, table string) []string {
	t.Helper()
	rows, err := tx.Query(`PRAGMA main.table_info("` + table + `")`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cols) == 0 {
		t.Fatalf("表 %s 无列", table)
	}
	return cols
}

// tableFingerprint 返回表的行数和内容 hash（列级 + 值级），用于源/目标保真比对。
// 用所有列作为排序键保证确定性，不依赖不存在的辅助函数。
func tableFingerprint(t *testing.T, db *sql.DB, table string) (int, string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	raw := columnList(t, tx, table)
	quoted := make([]string, len(raw))
	for i, c := range raw {
		quoted[i] = `"` + c + `"`
	}
	colsSQL := strings.Join(quoted, ",")
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return 0, ""
	}
	h, err := hashRows(tx, "main", table, colsSQL, len(raw), quoted)
	if err != nil {
		t.Fatal(err)
	}
	return n, h
}

// TestMigrate355Fidelity 验证 3.5.5 → 3.5.7：结构升级通过，所有业务表内容保真，
// 源 baseline_version 保留，schema_migrations_audit 为空，且不发生旧迁移转换。
func TestMigrate355Fidelity(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src355.db")
	build355Fixture(t, srcPath)
	outPath := filepath.Join(t.TempDir(), "out357.db")
	if err := runMigrate([]string{"--in", srcPath, "--out", outPath, "--baseline", "3.5.5"}); err != nil {
		t.Fatalf("3.5.5 迁移失败: %v", err)
	}
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	// 终态 schema_meta 映射
	var version, hash, baseline string
	var applied int64
	if err := dst.QueryRow(`SELECT schema_version, schema_hash, baseline_version, applied_at FROM schema_meta`).Scan(&version, &hash, &baseline, &applied); err != nil {
		t.Fatal(err)
	}
	if version != "3.5.7" {
		t.Fatalf("schema_version 应为 3.5.7，实际 %q", version)
	}
	if hash != canonicalSchemaHash {
		t.Fatalf("schema_hash 应为 Canonical %s，实际 %s", canonicalSchemaHash, hash)
	}
	if baseline != "src-baseline-3.5.5" {
		t.Fatalf("baseline_version 必须保留源值，实际 %q", baseline)
	}
	if applied <= 1700000000 {
		t.Fatalf("applied_at 应更新为本次应用时间，实际 %d", applied)
	}

	// 新审计表必须为空，不能凭空构造历史
	var auditN int
	if err := dst.QueryRow(`SELECT count(*) FROM schema_migrations_audit`).Scan(&auditN); err != nil {
		t.Fatalf("读取 schema_migrations_audit: %v", err)
	}
	if auditN != 0 {
		t.Fatalf("schema_migrations_audit 不应凭空构造，实际 %d 行", auditN)
	}

	// 所有业务表内容保真（空表与有数据表都要逐表比对）
	names, err := tableNames(src, "main")
	if err != nil {
		t.Fatal(err)
	}
	for tbl := range names {
		if tbl == "schema_meta" {
			continue
		}
		nSrc, hSrc := tableFingerprint(t, src, tbl)
		nDst, hDst := tableFingerprint(t, dst, tbl)
		if nSrc != nDst {
			t.Fatalf("表 %s 行数不一致：源 %d 目标 %d", tbl, nSrc, nDst)
		}
		if hSrc != hDst {
			t.Fatalf("表 %s 内容 hash 不一致：源 %s 目标 %s", tbl, hSrc, hDst)
		}
	}
}

// TestMigrate355PreservesEmptyBaseline 是 F1 的端到端回归：
// baseline_version 是 TEXT NOT NULL，空字符串是合法源值，迁移后必须原样保留，
// 不能被 EnsureFresh 的默认值 3.5.5 覆盖；源文件不得被修改。
func TestMigrate355PreservesEmptyBaseline(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src355-empty-baseline.db")
	build355Fixture(t, srcPath)
	db, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_meta SET baseline_version=''`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)

	outPath := filepath.Join(t.TempDir(), "out357.db")
	if err := runMigrate([]string{"--in", srcPath, "--out", outPath, "--baseline", "3.5.5"}); err != nil {
		t.Fatalf("3.5.5 空 baseline 迁移失败: %v", err)
	}
	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash != sha256.Sum256(after) {
		t.Fatal("迁移不得修改 3.5.5 源文件")
	}

	dst, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var version, hash, baseline string
	var applied int64
	if err := dst.QueryRow(`SELECT schema_version, schema_hash, baseline_version, applied_at FROM schema_meta`).Scan(&version, &hash, &baseline, &applied); err != nil {
		t.Fatal(err)
	}
	if version != "3.5.7" {
		t.Fatalf("schema_version 应为 3.5.7，实际 %q", version)
	}
	if hash != canonicalSchemaHash {
		t.Fatalf("schema_hash 应为 Canonical %s，实际 %s", canonicalSchemaHash, hash)
	}
	if baseline != "" {
		t.Fatalf("源 baseline_version 为空串时必须原样保留，实际 %q", baseline)
	}
	if applied <= 1700000000 {
		t.Fatalf("applied_at 应更新为本次应用时间，实际 %d", applied)
	}
	var n int
	if err := dst.QueryRow(`SELECT count(*) FROM sources`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("sources 数据应保留 3 行，实际 %d", n)
	}
}

// TestMigrate355RejectsWrongMetaRowCount 覆盖 3.5.5 源 schema_meta 0 行/2 行：
// 必须拒绝，不得继续建输出库，源文件不变。
func TestMigrate355RejectsWrongMetaRowCount(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *sql.DB)
	}{
		{
			name: "zero",
			mutate: func(t *testing.T, db *sql.DB) {
				t.Helper()
				if _, err := db.Exec(`DELETE FROM schema_meta`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "two",
			mutate: func(t *testing.T, db *sql.DB) {
				t.Helper()
				if _, err := db.Exec(`INSERT INTO schema_meta(schema_version,schema_hash,baseline_version,applied_at) VALUES('3.5.4','0842cd1efa4b7330cf53a7f9d98491597481cfcec80d1b5560bf8f9f0ef81170','other',1)`); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcPath := filepath.Join(t.TempDir(), "src355-meta-"+tc.name+".db")
			build355Fixture(t, srcPath)
			db, err := sql.Open("sqlite", srcPath)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(t.TempDir(), "must-not-exist.db")
			err = runMigrate([]string{"--in", srcPath, "--out", outPath, "--baseline", "3.5.5"})
			if err == nil {
				t.Fatalf("schema_meta %s 行时必须拒绝迁移", tc.name)
			}
			if !strings.Contains(err.Error(), "schema_meta 必须恰好一行") {
				t.Fatalf("错误信息应说明 schema_meta 行数异常: %v", err)
			}
			if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
				t.Fatalf("拒绝后不应产生目标文件: %v", statErr)
			}
			after, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("拒绝路径不得修改源文件")
			}
		})
	}
}

// ---- 3.5.6 → 3.5.7 metadata-only 升级测试 ----

// build356Fixture 复用 Canonical DDL（3.5.6 与 3.5.7 结构一致）构建库，再把版本标签
// 改回 3.5.6，并保留一个可被验证的 baseline_version，证明 metadata-only 路径不覆盖它。
func build356Fixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := schema.EnsureFresh(db, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_meta SET schema_version='3.5.6', baseline_version='keep-me-356'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources(base_url,name,created_at,repo_host) VALUES ('https://x.example/','X',1,'docker.io')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blobs(sha256) VALUES ('feed')`); err != nil {
		t.Fatal(err)
	}
}

// TestUpgrade356MetadataOnly 验证 3.5.6 → 3.5.7 只改版本标签：源库只读不被修改，
// schema_hash / baseline_version / applied_at 原值保留，业务数据原样复制。
func TestUpgrade356MetadataOnly(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src356.db")
	build356Fixture(t, srcPath)
	srcDB, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	var srcApplied int64
	if err := srcDB.QueryRow(`SELECT applied_at FROM schema_meta WHERE id=1`).Scan(&srcApplied); err != nil {
		srcDB.Close()
		t.Fatal(err)
	}
	if err := srcDB.Close(); err != nil {
		t.Fatal(err)
	}
	if srcApplied <= 0 {
		t.Fatalf("测试前提错误：源 applied_at 应为正数，实际 %d", srcApplied)
	}
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "out357.db")
	if err := runUpgrade356([]string{"--in", srcPath, "--out", outPath}); err != nil {
		t.Fatalf("upgrade356 失败: %v", err)
	}

	// 源库只读，必须未被修改
	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("upgrade356 修改了只读源库")
	}

	dst, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	var version, hash, baseline string
	var applied int64
	if err := dst.QueryRow(`SELECT schema_version, schema_hash, baseline_version, applied_at FROM schema_meta`).Scan(&version, &hash, &baseline, &applied); err != nil {
		t.Fatal(err)
	}
	if version != "3.5.7" {
		t.Fatalf("schema_version 应为 3.5.7，实际 %q", version)
	}
	if hash != canonicalSchemaHash {
		t.Fatalf("schema_hash 必须保持不变（%s），实际 %s", canonicalSchemaHash, hash)
	}
	if baseline != "keep-me-356" {
		t.Fatalf("baseline_version 必须原值保留，实际 %q", baseline)
	}
	if applied != srcApplied {
		t.Fatalf("applied_at 必须与源值完全相等：源 %d，目标 %d", srcApplied, applied)
	}

	// 业务数据原样保留
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, tbl := range []string{"sources", "blobs"} {
		nSrc, hSrc := tableFingerprint(t, src, tbl)
		nDst, hDst := tableFingerprint(t, dst, tbl)
		if nSrc != nDst || hSrc != hDst {
			t.Fatalf("表 %s 升级后内容不一致：源(%d,%s) 目标(%d,%s)", tbl, nSrc, hSrc, nDst, hDst)
		}
	}
}

// TestUpgrade356CopyFailureCleansTmp 是 F2 的确定性回归：
// 通过测试钩子制造“发生部分写入后失败”，验证本次 *.tmp-<pid> 被清理、
// 正式目标不存在、源文件 SHA-256 不变；不依赖 ulimit。
func TestUpgrade356CopyFailureCleansTmp(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src356-copyfail.db")
	build356Fixture(t, srcPath)
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) <= 4096 {
		t.Fatalf("测试前提错误：源库应大于 4 KiB，实际 %d 字节", len(before))
	}
	beforeHash := sha256.Sum256(before)

	outPath := filepath.Join(t.TempDir(), "out357.db")
	tmpPath := outPath + ".tmp-" + strconv.Itoa(os.Getpid())

	var sawPartial bool
	copyFileWriteHook = func(written int64) error {
		if written >= 4096 {
			sawPartial = true
			return errors.New("injected copy failure")
		}
		return nil
	}
	defer func() { copyFileWriteHook = nil }()

	err = runUpgrade356([]string{"--in", srcPath, "--out", outPath})
	if err == nil {
		t.Fatal("注入复制失败后 upgrade356 不应成功")
	}
	if !sawPartial {
		t.Fatal("测试前提未满足：钩子未观察到部分写入")
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("失败路径不应产生正式目标文件: %v", statErr)
	}
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Fatalf("失败路径必须清理本次临时文件 %s: %v", tmpPath, statErr)
	}
	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash != sha256.Sum256(after) {
		t.Fatal("失败路径不得修改源文件")
	}
}

// TestCopyHookWriterHonorsWriterContract 验证测试 writer 不违反 io.Writer 契约：
// 未注入错误时必须写满当前输入缓冲区；只有注入错误时才返回短写加实际错误。
func TestCopyHookWriterHonorsWriterContract(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 10*1024)

	var buf bytes.Buffer
	w := &copyHookWriter{dst: &buf}
	copyFileWriteHook = func(int64) error { return nil }
	defer func() { copyFileWriteHook = nil }()
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("未注入错误时不应返回错误: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("未注入错误时必须写满整个缓冲区：n=%d，期望=%d", n, len(payload))
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("写入内容与输入不一致")
	}

	buf.Reset()
	w = &copyHookWriter{dst: &buf}
	copyFileWriteHook = func(written int64) error {
		if written >= 4096 {
			return errors.New("injected copy failure")
		}
		return nil
	}
	n, err = w.Write(payload)
	if err == nil {
		t.Fatal("注入错误时必须返回错误")
	}
	if n != 4096 {
		t.Fatalf("注入错误时应返回已写入的 4096 字节，实际 %d", n)
	}
	if buf.Len() != 4096 {
		t.Fatalf("注入错误时底层只应写入 4096 字节，实际 %d", buf.Len())
	}
}

// TestUpgrade356HandlesWalModeSource 覆盖 WAL 模式 3.5.6 源库：
// 在源库 checkpoint 完成、没有未合并 WAL 的前提下，metadata-only 升级仍应成功，
// 源文件 SHA-256 不变，目标元数据语义保持。
func TestUpgrade356HandlesWalModeSource(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src356-wal.db")
	build356Fixture(t, srcPath)
	db, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if mode != "wal" {
		db.Close()
		t.Fatalf("测试前提错误：journal_mode 应为 wal，实际 %q", mode)
	}
	var srcApplied int64
	if err := db.QueryRow(`SELECT applied_at FROM schema_meta WHERE id=1`).Scan(&srcApplied); err != nil {
		db.Close()
		t.Fatal(err)
	}
	var busy, logFrames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if busy != 0 {
		db.Close()
		t.Fatalf("测试前提错误：wal_checkpoint 仍 busy=%d", busy)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(srcPath + "-wal"); err == nil && info.Size() > 0 {
		t.Fatalf("测试前提错误：checkpoint 后仍有非空 -wal，%d 字节", info.Size())
	}
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)

	outPath := filepath.Join(t.TempDir(), "out357.db")
	if err := runUpgrade356([]string{"--in", srcPath, "--out", outPath}); err != nil {
		t.Fatalf("WAL 模式 3.5.6 源升级失败: %v", err)
	}
	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash != sha256.Sum256(after) {
		t.Fatal("升级不得修改 WAL 模式源文件")
	}

	dst, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var version, hash, baseline string
	var applied int64
	if err := dst.QueryRow(`SELECT schema_version, schema_hash, baseline_version, applied_at FROM schema_meta`).Scan(&version, &hash, &baseline, &applied); err != nil {
		t.Fatal(err)
	}
	if version != "3.5.7" {
		t.Fatalf("schema_version 应为 3.5.7，实际 %q", version)
	}
	if hash != canonicalSchemaHash {
		t.Fatalf("schema_hash 应为 Canonical %s，实际 %s", canonicalSchemaHash, hash)
	}
	if baseline != "keep-me-356" {
		t.Fatalf("baseline_version 必须原值保留，实际 %q", baseline)
	}
	if applied != srcApplied {
		t.Fatalf("applied_at 必须与源值完全相等：源 %d，目标 %d", srcApplied, applied)
	}
}

// TestUpgrade356RejectsWrongVersion 验证 upgrade356 只接受 3.5.6 源库；
// 用 3.5.5 源库调用必须被拒绝且不产生目标文件（原子性/失败可回滚）。
func TestUpgrade356RejectsWrongVersion(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src355.db")
	build355Fixture(t, srcPath)
	outPath := filepath.Join(t.TempDir(), "should-not-exist.db")
	err := runUpgrade356([]string{"--in", srcPath, "--out", outPath})
	if err == nil {
		t.Fatal("upgrade356 不应接受 3.5.5 源库")
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("拒绝后仍产生了目标文件: %v", statErr)
	}
}

// TestRequireCanonicalOutputHash 覆盖 3.5.5 输出结构校验：
// 规范结构的输出通过；结构被改动（哈希不等于冻结值）的输出被拒绝。
func TestRequireCanonicalOutputHash(t *testing.T) {
	// 规范结构：由 Canonical DDL 创建，哈希应等于冻结值。
	canonPath := filepath.Join(t.TempDir(), "canon.db")
	cdb, err := sql.Open("sqlite", canonPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.EnsureFresh(cdb, "fresh"); err != nil {
		cdb.Close()
		t.Fatal(err)
	}
	h, err := schema.Hash(cdb)
	cdb.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := requireCanonicalOutputHash(h); err != nil {
		t.Fatalf("规范结构应通过: %v", err)
	}

	// 被改动的结构：加一列后哈希必然不等于冻结值，必须被拒绝。
	altPath := filepath.Join(t.TempDir(), "alt.db")
	adb, err := sql.Open("sqlite", altPath)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()
	if err := schema.EnsureFresh(adb, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := adb.Exec(`ALTER TABLE sources ADD COLUMN extra_col TEXT`); err != nil {
		t.Fatal(err)
	}
	altHash, err := schema.Hash(adb)
	if err != nil {
		t.Fatal(err)
	}
	if altHash == canonicalSchemaHash {
		t.Fatal("测试前提错误：改动结构后哈希不应等于 Canonical")
	}
	if err := requireCanonicalOutputHash(altHash); err == nil {
		t.Fatal("非 Canonical 输出结构必须被拒绝发布")
	}
}

// TestUpgrade356RejectsWrongActualStructure 覆盖“元数据 schema_hash 正确、实际结构错误”：
// 必须由 VerifyStructure 拦截，不能只相信元数据声明；失败不产生输出、源文件不变。
func TestUpgrade356RejectsWrongActualStructure(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src356-badstructure.db")
	build356Fixture(t, srcPath)
	db, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE sources ADD COLUMN extra_col TEXT`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "must-not-exist.db")
	err = runUpgrade356([]string{"--in", srcPath, "--out", outPath})
	if err == nil {
		t.Fatal("实际结构不是 Canonical 时必须拒绝迁移")
	}
	if !strings.Contains(err.Error(), "源结构不是 Canonical") {
		t.Fatalf("错误信息应说明实际结构不是 Canonical: %v", err)
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("失败路径不应产生目标文件: %v", statErr)
	}
	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("失败路径不得改动源文件")
	}
}

// TestUpgrade356RejectsWrongMetadataHash 覆盖 3.5.6 元数据声明校验：
// 实际结构正确但元数据 schema_hash 被篡改时，必须拒绝迁移，且不产生输出、源文件字节不变。
func TestUpgrade356RejectsWrongMetadataHash(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src356-badhash.db")
	build356Fixture(t, srcPath)
	// 篡改元数据声明的结构哈希；实际结构仍是 Canonical。
	db, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_meta SET schema_hash='deadbeef' WHERE id=1`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "must-not-exist.db")
	if err := runUpgrade356([]string{"--in", srcPath, "--out", outPath}); err == nil {
		t.Fatal("元数据 schema_hash 错误时必须拒绝迁移")
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("失败路径不应产生目标文件: %v", statErr)
	}
	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("失败路径不得改动源文件")
	}
}
