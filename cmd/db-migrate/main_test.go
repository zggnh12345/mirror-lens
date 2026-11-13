package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// 输入指纹是冻结值，不能被空值/测试 fixture 意外替代。
func TestInputSchemaFingerprintsFrozen(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for baseline, h := range inputSchemaFingerprints {
		if !re.MatchString(h) {
			t.Errorf("baseline=%s 的输入指纹不是 64 位 sha256: %q", baseline, h)
		}
	}
	if len(inputSchemaFingerprints) != 2 {
		t.Fatalf("只允许冻结 3.4.6/3.5.4 两个输入指纹，实际 %d 个", len(inputSchemaFingerprints))
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
