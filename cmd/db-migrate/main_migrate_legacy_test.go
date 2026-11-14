package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/zggnh12345/mirror-lens/internal/store/schema"
)

// 这两个 DDL 来自 docs/schema-inventory 中冻结的 3.4.6 / 3.5.4 真实结构，
// 不是当前 Canonical DDL。测试会先断言它们的 schema.Hash 精确命中
// inputSchemaFingerprints，再用真实 runMigrate 路径做端到端迁移。
//
//go:embed fixture_3.4.6.sql
var fixture346DDL string

//go:embed fixture_3.5.4.sql
var fixture354DDL string

const fingerprint346 = "972db548d1eac569aec7ae11d859cdfbcd9114a0ee2dd1217ca085ca5a4d835f"
const fingerprint354 = "4af3f62e273685b10493cafdeb7c6745b8d98d87e017dc1445b42ca66308cffb"

func TestMigrate346EndToEnd(t *testing.T) {
	testLegacyMigration(t, "3.4.6", fixture346DDL, fingerprint346)
}

func TestMigrate354EndToEnd(t *testing.T) {
	testLegacyMigration(t, "3.5.4", fixture354DDL, fingerprint354)
}

// testLegacyMigration 的检查顺序刻意与迁移承诺一致：
// 1) 输入结构指纹命中；2) 实际执行 runMigrate；3) 源文件字节不变；
// 4) 目标运行时校验；5) 所有表逐表比对；6) 主键/外键/完整性。
func testLegacyMigration(t *testing.T, baseline, ddl, wantFingerprint string) {
	t.Helper()
	srcPath := filepath.Join(t.TempDir(), "src-"+baseline+".db")
	buildLegacyFixture(t, srcPath, ddl, baseline)

	beforeBytes, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeSHA := sha256.Sum256(beforeBytes)
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	gotFingerprint, err := schema.Hash(src)
	if err != nil {
		src.Close()
		t.Fatal(err)
	}
	if gotFingerprint != wantFingerprint {
		src.Close()
		t.Fatalf("输入结构指纹未命中: 实际=%s 期望=%s", gotFingerprint, wantFingerprint)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "out-"+baseline+".db")
	if err := runMigrate([]string{"--in", srcPath, "--out", outPath, "--baseline", baseline}); err != nil {
		t.Fatalf("%s 迁移失败: %v", baseline, err)
	}

	afterBytes, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeSHA != sha256.Sum256(afterBytes) {
		t.Fatal("迁移修改了源库文件")
	}

	dst, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	meta, err := schema.Verify(dst)
	if err != nil {
		t.Fatalf("目标库未通过运行时 Canonical 校验: %v", err)
	}
	if meta.SchemaVersion != "3.5.7" || meta.BaselineVersion != baseline {
		t.Fatalf("目标 schema_meta 不符: %+v，期望 version=3.5.7 baseline=%s", meta, baseline)
	}
	checkTargetIntegrity(t, dst)
	compareLegacyTarget(t, baseline, srcPath, dst)
}

// buildLegacyFixture 建一个带完整旧结构、且每张业务表都有非空行的隔离源库。
// 数据只用于覆盖映射边界，不来自生产库。
func buildLegacyFixture(t *testing.T, path, ddl, baseline string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("创建 %s 旧结构失败: %v", baseline, err)
	}

	seed := []string{
		`INSERT INTO sources(base_url,name,created_at,docker_cap,ghcr_cap,github_cap,status,enabled,note) VALUES
		 ('https://docker.example/','docker',101,1,-1,-1,'usable',1,'docker'),
		 ('https://ghcr.example/','ghcr',102,0,1,-1,'usable',1,'ghcr'),
		 ('https://github.example/','github',103,0,0,1,'usable',1,'github')`,
		`INSERT INTO checks(source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,http_code,error) VALUES
		 (1,'docker',100,1,1,11,100,'d1',200,''),
		 (2,'docker',200,2,0,22,200,'d2',500,'boom'),
		 (3,'github',300,1,1,33,300,'d3',200,'')`,
		`INSERT INTO source_events(source_id,kind,ts,ok,ms,code) VALUES (1,'docker',100,1,10,200),(2,'docker',200,0,20,500),(3,'github',300,1,30,200)`,
		`INSERT INTO source_health(source_id,family,fails,ok_at,last_at) VALUES (1,'http',0,100,100),(2,'https',1,200,200),(3,'http',0,300,300)`,
		`INSERT INTO blobs(sha256,size,integrity_status) VALUES ('deadbeef',10,'ok'),('cafe',20,'unknown')`,
		`INSERT INTO cache_entries(key,sha256,size,created_at) VALUES ('cache-1','deadbeef',10,100)`,
		`INSERT INTO pending_blobs(sha,queued_at) VALUES ('cafe',100)`,
		`INSERT INTO traffic_stats(day,traffic,kind,n,bytes) VALUES ('2026-11-14','docker','',2,300)`,
		`INSERT INTO live_speed(source_id,kind,ts,bytes,window_s) VALUES (1,'docker',100,1000,5)`,
		`INSERT INTO proxy_speed(source_id,kind,ts,bytes,ms) VALUES (1,'docker',100,1000,20)`,
		`INSERT INTO audit(ts,ip,user,action,target,detail,ok) VALUES (100,'127.0.0.1','alice','login','/','ok',1)`,
		`INSERT INTO sessions(token,created_at,expires_at) VALUES ('sess-1',100,200)`,
		`INSERT INTO tokens(name,hash,prefix,created_at) VALUES ('token-1','hash-1','prefix-1',100)`,
		`INSERT INTO settings(key,value) VALUES ('k1','v1'),('k2','v2')`,
		`INSERT INTO source_groups(name,rank) VALUES ('group-1',1)`,
		`INSERT INTO trusted_relays(name,url,enabled,created_at) VALUES ('relay-1','https://relay.example/',1,100)`,
		`INSERT INTO meta(k,v) VALUES ('meta-1','value-1')`,
		`INSERT INTO blob_integrity_failures(expected_digest,actual_digest,digest_source,protocol,source_id,source_name,error_code,created_at)
		 VALUES ('expected','actual','body','http',1,'docker','E_DIGEST',100)`,
	}
	if baseline == "3.5.4" {
		seed = append(seed,
			`UPDATE sources SET repo_host='docker.io', registry_cap=1, registry_path_mode='v2', registry_probe_at=123, registry_probe_error='', registry_identity_state='identified' WHERE id=1`,
			`UPDATE sources SET repo_host='ghcr.io', registry_cap=1, registry_path_mode='v2', registry_probe_at=124, registry_probe_error='', registry_identity_state='identified' WHERE id=2`,
			`UPDATE sources SET repo_host='', registry_cap=-1, registry_path_mode='', registry_probe_at=0, registry_probe_error='', registry_identity_state='unclassified' WHERE id=3`,
			`UPDATE source_events SET neutral=1 WHERE id=2`,
			`INSERT INTO breaker_state(source_id,kind,fails,until,rev,updated_at) VALUES (1,'docker',1,300,1,100)`,
			`INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES ('docker.io',1,'cred',100)`,
			`INSERT INTO registry_host_recovery(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,retry_failures,last_error,updated_at)
			 VALUES (1,'registry','', 'ready',1,100,100,200,0,0,'',100)`,
			`INSERT INTO source_health_archive(source_id,family,fails,ok_at,last_at,archived_at,batch)
			 VALUES (1,'http',0,100,100,300,'batch-1')`,
			`INSERT INTO source_health_scope(source_id,family,scope,gen,fails,ok_at,last_at)
			 VALUES (1,'http','',1,0,100,100)`,
			`INSERT INTO schema_migrations(name,applied_at,checksum,checksum_origin) VALUES
			 ('3.5.3-phase-b-tables',100,'c1','origin'),
			 ('b1-archive-legacy-registry',200,'c2','origin'),
			 ('3.5.4-registry-identity',300,'c3','origin')`,
		)
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("写入 %s fixture 失败: %v\nSQL: %s", baseline, err, q)
		}
	}
}

// checkTargetIntegrity 显式检查 SQLite 完整性与外键；结构本身已由 schema.Verify
// 的 Canonical Hash 锁定（其中包含全部表、列、主键、索引和外键定义）。
func checkTargetIntegrity(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	names, err := tableNames(db, "main")
	if err != nil {
		t.Fatal(err)
	}
	for name := range names {
		pk, err := primaryKey(db, "main", name)
		if err != nil {
			t.Fatalf("读取 %s 主键: %v", name, err)
		}
		if len(pk) == 0 {
			t.Fatalf("目标表 %s 没有主键", name)
		}
	}
	if err := checkIntegrity(db); err != nil {
		t.Fatalf("目标库完整性检查失败: %v", err)
	}
}

// compareLegacyTarget 逐表比较期望映射结果：
//   - 普通表：源/目标全列指纹必须一致；
//   - sources：3.4.6 按能力生成 repo_host，3.5.4 原样保留 repo_host；
//   - source_events：3.4.6 的 neutral 必须补 0，3.5.4 保留原值；
//   - schema_migrations：3.5.4 搬到 schema_migrations_audit，3.4.6 输出为空；
//   - 旧库没有、Canonical 新增的表：必须为空。
func compareLegacyTarget(t *testing.T, baseline, srcPath string, dst *sql.DB) {
	t.Helper()
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	srcNames, err := tableNames(src, "main")
	if err != nil {
		t.Fatal(err)
	}
	dstNames, err := tableNames(dst, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(dstNames) == 0 {
		t.Fatal("目标库没有表")
	}
	for name := range srcNames {
		if name == "_writable" {
			continue
		}
		switch name {
		case "sources":
			checkLegacySources(t, baseline, src, dst)
		case "source_events":
			checkLegacySourceEvents(t, baseline, src, dst)
		case "schema_migrations":
			checkLegacyAudit(t, src, dst)
		default:
			ns, hs := tableFingerprint(t, src, name)
			nd, hd := tableFingerprint(t, dst, name)
			if ns != nd || hs != hd {
				t.Fatalf("表 %s 内容不一致：源(%d,%s) 目标(%d,%s)", name, ns, hs, nd, hd)
			}
		}
	}

	for name := range dstNames {
		if name == "schema_meta" || srcNames[name] {
			continue
		}
		if name == "schema_migrations_audit" {
			if !srcNames["schema_migrations"] {
				n, _ := tableFingerprint(t, dst, name)
				if n != 0 {
					t.Fatalf("旧库没有 schema_migrations 时 %s 应为空，实际 %d 行", name, n)
				}
				continue
			}
			checkLegacyAudit(t, src, dst)
			continue
		}
		n, _ := tableFingerprint(t, dst, name)
		if n != 0 {
			t.Fatalf("旧库没有的表 %s 迁移后应为空，实际 %d 行", name, n)
		}
	}
}

func selectedFingerprint(t *testing.T, db *sql.DB, table string, cols []string) (int, string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
	}
	colsSQL := strings.Join(quoted, ",")
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return 0, ""
	}
	h, err := hashRows(tx, "main", table, colsSQL, len(cols), quoted)
	if err != nil {
		t.Fatal(err)
	}
	return n, h
}

func checkLegacySources(t *testing.T, baseline string, src, dst *sql.DB) {
	t.Helper()
	// 先逐列验证所有共有字段；sources 的特殊映射字段再单独断言。
	common, err := columns(src, "main", "sources")
	if err != nil {
		t.Fatal(err)
	}
	filtered := common[:0]
	for _, c := range common {
		if c != "registry_identity_state" {
			filtered = append(filtered, c)
		}
	}
	ns, hs := selectedFingerprint(t, src, "sources", filtered)
	nd, hd := selectedFingerprint(t, dst, "sources", filtered)
	if ns != nd || hs != hd {
		t.Fatalf("sources 共有字段不一致：源(%d,%s) 目标(%d,%s)", ns, hs, nd, hd)
	}

	cols, err := columns(dst, "main", "sources")
	if err != nil {
		t.Fatal(err)
	}
	if containsString(cols, "registry_identity_state") {
		t.Fatal("Canonical sources 不应包含 registry_identity_state")
	}

	type row struct {
		id                            int64
		baseURL, name                 string
		createdAt                     int64
		dockerCap, ghcrCap, githubCap int64
		repoHost, pathMode, probeErr  string
		registryCap, probeAt          int64
	}
	rows, err := dst.Query(`SELECT id,base_url,name,created_at,docker_cap,ghcr_cap,github_cap,repo_host,registry_cap,registry_path_mode,registry_probe_at,registry_probe_error FROM sources ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.baseURL, &r.name, &r.createdAt, &r.dockerCap, &r.ghcrCap, &r.githubCap, &r.repoHost, &r.registryCap, &r.pathMode, &r.probeAt, &r.probeErr); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []row{
		{1, "https://docker.example/", "docker", 101, 1, -1, -1, "docker.io", "", "", -1, 0},
		{2, "https://ghcr.example/", "ghcr", 102, 0, 1, -1, "ghcr.io", "", "", -1, 0},
		{3, "https://github.example/", "github", 103, 0, 0, 1, "", "", "", -1, 0},
	}
	if baseline == "3.5.4" {
		want[0] = row{1, "https://docker.example/", "docker", 101, 1, -1, -1, "docker.io", "v2", "", 1, 123}
		want[1] = row{2, "https://ghcr.example/", "ghcr", 102, 0, 1, -1, "ghcr.io", "v2", "", 1, 124}
		want[2] = row{3, "https://github.example/", "github", 103, 0, 0, 1, "", "", "", -1, 0}
	}
	if len(got) != len(want) {
		t.Fatalf("sources 行数=%d，期望=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sources 第 %d 行=%+v，期望=%+v", i+1, got[i], want[i])
		}
	}
}

func checkLegacySourceEvents(t *testing.T, baseline string, src, dst *sql.DB) {
	t.Helper()
	type event struct {
		id, sourceID, ts, ok, ms, code, neutral int64
		kind                                    string
	}
	read := func(db *sql.DB, withNeutral bool) []event {
		t.Helper()
		q := `SELECT id,source_id,kind,ts,ok,ms,code`
		if withNeutral {
			q += `,neutral`
		}
		q += ` FROM source_events ORDER BY id`
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []event
		for rows.Next() {
			var e event
			if withNeutral {
				if err := rows.Scan(&e.id, &e.sourceID, &e.kind, &e.ts, &e.ok, &e.ms, &e.code, &e.neutral); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := rows.Scan(&e.id, &e.sourceID, &e.kind, &e.ts, &e.ok, &e.ms, &e.code); err != nil {
					t.Fatal(err)
				}
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	srcEvents := read(src, baseline == "3.5.4")
	dstEvents := read(dst, true)
	if len(srcEvents) != len(dstEvents) {
		t.Fatalf("source_events 行数=%d，期望=%d", len(dstEvents), len(srcEvents))
	}
	for i := range srcEvents {
		if baseline != "3.5.4" {
			srcEvents[i].neutral = 0
		}
		if srcEvents[i] != dstEvents[i] {
			t.Fatalf("source_events 第 %d 行=%+v，期望=%+v", i+1, dstEvents[i], srcEvents[i])
		}
	}
}

func checkLegacyAudit(t *testing.T, src, dst *sql.DB) {
	t.Helper()
	ns, hs := tableFingerprint(t, src, "schema_migrations")
	nd, hd := tableFingerprint(t, dst, "schema_migrations_audit")
	if ns != nd || hs != hd {
		t.Fatalf("schema_migrations_audit 不一致：源(%d,%s) 目标(%d,%s)", ns, hs, nd, hd)
	}
}

// 失败路径：输入指纹被破坏时必须在创建输出前拒绝，且源文件字节不变。
func TestMigrateLegacyFailureLeavesNoOutput(t *testing.T) {
	cases := []struct {
		baseline string
		ddl      string
	}{
		{"3.4.6", fixture346DDL},
		{"3.5.4", fixture354DDL},
	}
	for _, c := range cases {
		t.Run(c.baseline, func(t *testing.T) {
			srcPath := filepath.Join(t.TempDir(), "src.db")
			buildLegacyFixture(t, srcPath, c.ddl, c.baseline)
			db, err := sql.Open("sqlite", srcPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`ALTER TABLE sources ADD COLUMN unexpected_col TEXT`); err != nil {
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
			if err := runMigrate([]string{"--in", srcPath, "--out", outPath, "--baseline", c.baseline}); err == nil {
				t.Fatal("输入指纹被破坏时迁移应失败")
			}
			if _, err := os.Stat(outPath); !os.IsNotExist(err) {
				t.Fatalf("失败后仍发布目标文件: %v", err)
			}
			after, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("失败迁移修改了源文件")
			}
		})
	}
}
