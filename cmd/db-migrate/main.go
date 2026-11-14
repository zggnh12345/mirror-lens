// db-migrate 是 Mirror Lens 的离线数据库搬迁工具。
//
// 它只读打开旧库，新建 Canonical 目标库，按明确映射复制数据并校验。
// 运行时不会调用这里的任何逻辑；服务启动只接受带 schema_meta 的 Canonical 库。
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
	"github.com/zggnh12345/mirror-lens/internal/store/schema"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "migrate":
		err = runMigrate(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "inspect":
		err = runInspect(os.Args[2:])
	case "init":
		err = runInit(os.Args[2:])
	case "checkpoint":
		err = runCheckpoint(os.Args[2:])
	case "upgrade356":
		err = runUpgrade356(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "db-migrate:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法:
  db-migrate init       --out 新库 [--baseline fresh]
  db-migrate checkpoint --db 副本数据库
  db-migrate migrate    --in 旧库 --out 新库 --baseline 3.4.6|3.5.4|3.5.5
  db-migrate upgrade356 --in 3.5.6库 --out 新库
  db-migrate verify     --db 数据库
  db-migrate inspect    --db 数据库

迁移前必须停服务，并确保旧库没有未合并的 -wal。工具会拒绝覆盖已存在的目标文件。
migrate 的输入库必须匹配已冻结的 3.4.6/3.5.4/3.5.5 Schema 指纹；--force-baseline 不再绕过结构校验。
upgrade356 要求源库为 3.5.6 Canonical（结构 hash 已等于 3.5.7 Canonical），只改版本标签。`)
	os.Exit(2)
}

func runCheckpoint(args []string) error {
	fs := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	path := fs.String("db", "", "数据库路径（会写 WAL，仅用于副本）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("checkpoint 需要 --db")
	}
	db, err := sql.Open("sqlite", "file:"+*path+"?mode=rw")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var busy, logFrames, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("wal_checkpoint 被占用：busy=%d", busy)
	}
	fmt.Printf("checkpoint ok: %s frames=%d checkpointed=%d\n", *path, logFrames, checkpointed)
	return nil
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	out := fs.String("out", "", "新数据库路径")
	baseline := fs.String("baseline", "fresh", "baseline 审计标识")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("init 需要 --out")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("目标已存在，拒绝覆盖: %s", *out)
	} else if !os.IsNotExist(err) {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+*out+"?mode=rwc")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := schema.EnsureFresh(db, *baseline); err != nil {
		return err
	}
	if _, err := schema.Verify(db); err != nil {
		return err
	}
	h, err := schema.Hash(db)
	if err != nil {
		return err
	}
	fmt.Printf("created %s schema_version=%s schema_hash=%s baseline=%s\n", *out, schema.Version, h, *baseline)
	return nil
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	in := fs.String("in", "", "旧数据库路径（只读）")
	out := fs.String("out", "", "新数据库路径")
	baseline := fs.String("baseline", "", "旧库版本标识，例如 3.4.6 / 3.5.4 / 3.5.5")
	force := fs.Bool("force-baseline", false, "已废弃；不再绕过输入 Schema 指纹校验")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = force // 保留参数以拒绝旧脚本的“绕过”期待，但绝不改变校验路径。
	if *in == "" || *out == "" || *baseline == "" {
		return errors.New("migrate 需要 --in、--out、--baseline")
	}
	// 3.5.3 的 docker_cap=1 + repo_host='' 记录没有可恢复的身份事实。
	// 这条拒绝必须先于任何输入/输出操作，且 --force-baseline 不能绕过。
	if *baseline == "3.5.3" {
		return errors.New("3.5.3 是不支持的输入版本：缺少独立身份快照，docker_cap=1 且 repo_host='' 的 34 条记录会被静默归为无归属；--force-baseline 不能绕过")
	}
	if expected, ok := inputSchemaFingerprints[*baseline]; !ok {
		return fmt.Errorf("未识别的 baseline %q；只接受已冻结 Schema 指纹的 3.4.6 / 3.5.4 / 3.5.5", *baseline)
	} else if expected == "" {
		return fmt.Errorf("baseline %q 的 Schema 指纹尚未冻结", *baseline)
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("目标已存在，拒绝覆盖: %s", *out)
	} else if !os.IsNotExist(err) {
		return err
	}
	if wal, err := os.Stat(*in + "-wal"); err == nil && wal.Size() > 0 {
		return fmt.Errorf("旧库存在未合并的 WAL（%s，%d 字节）；请先停服务并在副本上 checkpoint", *in+"-wal", wal.Size())
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}

	src, err := sql.Open("sqlite", "file:"+*in+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return err
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	if err := src.Ping(); err != nil {
		return fmt.Errorf("打开旧库: %w", err)
	}
	// 旧库只读：显式再开一次 query_only，并在整个搬迁期间保持该连接。
	if _, err := src.Exec("PRAGMA query_only=ON"); err != nil {
		return err
	}
	actualFingerprint, err := schema.Hash(src)
	if err != nil {
		return fmt.Errorf("计算输入 Schema 指纹: %w", err)
	}
	expectedFingerprint := inputSchemaFingerprints[*baseline]
	if actualFingerprint != expectedFingerprint {
		return fmt.Errorf("输入 Schema 指纹不匹配 baseline=%s：实际=%s，期望=%s；拒绝迁移未知结构（--force-baseline 不能绕过）",
			*baseline, actualFingerprint, expectedFingerprint)
	}
	hostMap, err := validateRegistryHosts(src, *baseline)
	if err != nil {
		return err
	}

	// 3.5.5 源库需要读取并保留其真实 baseline_version，不能让 EnsureFresh 的默认值覆盖。
	var srcBaselineVersion string
	if *baseline == "3.5.5" {
		srcMeta, err := readMetaRaw(src)
		if err != nil {
			return fmt.Errorf("读取 3.5.5 源元数据: %w", err)
		}
		if srcMeta.SchemaVersion != "3.5.5" {
			return fmt.Errorf("3.5.5 升级要求源 schema_version=3.5.5，实际 %q", srcMeta.SchemaVersion)
		}
		srcBaselineVersion = srcMeta.BaselineVersion
	}

	tmp := fmt.Sprintf("%s.tmp-%d", *out, os.Getpid())
	_ = os.Remove(tmp)
	target, err := sql.Open("sqlite", "file:"+tmp+"?mode=rwc")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		target.Close()
		if !done {
			os.Remove(tmp)
		}
	}()
	target.SetMaxOpenConns(1)
	if _, err := target.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "step: canonical ddl")
	if err := schema.EnsureFresh(target, *baseline); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "step: attach")
	if _, err := target.Exec("ATTACH DATABASE ? AS src", "file:"+*in+"?mode=ro"); err != nil {
		return fmt.Errorf("只读挂载旧库: %w", err)
	}
	fmt.Fprintln(os.Stderr, "step: attached")
	defer target.Exec("DETACH DATABASE src")

	plan, err := buildPlan(target, src, *baseline, hostMap)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "step: plan", len(plan.Tables))
	if err := plan.validate(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "step: copy")
	if err := copyAll(target, plan); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "step: sequences")
	if err := rebuildSequences(target, plan); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "step: fk")
	if _, err := target.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	if err := checkIntegrity(target); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "step: hash")
	h, err := schema.Hash(target)
	if err != nil {
		return err
	}
	// 3.5.5 路径：实际输出结构必须等于冻结的 Canonical 哈希，
	// 而不只是与 schema_meta 自洽（后者只能证明结构与元数据一致，不能证明符合 Canonical）。
	if *baseline == "3.5.5" {
		if err := requireCanonicalOutputHash(h); err != nil {
			return err
		}
	}
	if err := finalizeTargetMeta(target, h, srcBaselineVersion); err != nil {
		return err
	}
	if _, err := schema.Verify(target); err != nil {
		return fmt.Errorf("迁移后校验失败: %w", err)
	}
	if err := target.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, *out); err != nil {
		return fmt.Errorf("发布迁移结果: %w", err)
	}
	done = true
	report := map[string]any{
		"ok": true, "baseline": *baseline, "schema_hash": h,
		"tables": plan.Counts, "skipped": plan.Skipped,
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(b))
	return nil
}

// readMetaRaw 读取 schema_meta 的当前值，不强制目标版本；用于读取尚未升级的源库元数据。
func readMetaRaw(db *sql.DB) (schema.Meta, error) {
	var m schema.Meta
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_meta`).Scan(&n); err != nil {
		return m, fmt.Errorf("数据库缺少 schema_meta: %w", err)
	}
	if n != 1 {
		return m, fmt.Errorf("schema_meta 必须恰好一行，实际 %d 行", n)
	}
	if err := db.QueryRow(`SELECT schema_version, schema_hash, baseline_version, applied_at FROM schema_meta`).
		Scan(&m.SchemaVersion, &m.SchemaHash, &m.BaselineVersion, &m.AppliedAt); err != nil {
		return m, fmt.Errorf("读取 schema_meta: %w", err)
	}
	return m, nil
}

// finalizeTargetMeta 写入升级后的 schema_meta。
// baselineOverride 为空时只刷新 schema_hash/applied_at（保留 EnsureFresh 写入的 baseline_version）；
// 非空时（3.5.5 路径）用源库真实 baseline_version 显式覆盖，并确认 schema_version 与目标版本一致。
func finalizeTargetMeta(target *sql.DB, h, baselineOverride string) error {
	if baselineOverride == "" {
		_, err := target.Exec(`UPDATE schema_meta SET schema_hash=?, applied_at=? WHERE schema_version=?`,
			h, time.Now().Unix(), schema.Version)
		return err
	}
	_, err := target.Exec(`UPDATE schema_meta SET schema_version=?, schema_hash=?, baseline_version=?, applied_at=? WHERE id=1`,
		schema.Version, h, baselineOverride, time.Now().Unix())
	return err
}

// requireCanonicalOutputHash 校验输出库的实际结构哈希必须等于冻结的 Canonical 哈希。
// 仅自洽校验（实际哈希 == schema_meta.schema_hash）不能证明输出符合冻结的 3.5.7 结构。
func requireCanonicalOutputHash(h string) error {
	if h != canonicalSchemaHash {
		return fmt.Errorf("输出结构 hash=%s 与冻结的 Canonical hash=%s 不一致；拒绝发布", h, canonicalSchemaHash)
	}
	return nil
}

// requireCanonicalMetadataHash 校验 3.5.6 源元数据中声明的结构哈希必须等于 Canonical 哈希。
// 它验证「声明」，VerifyStructure 验证「实际结构」，两者不能互相替代。
func requireCanonicalMetadataHash(h string) error {
	if h != canonicalSchemaHash {
		return fmt.Errorf("源元数据 schema_hash=%s 与 Canonical hash=%s 不一致；拒绝迁移", h, canonicalSchemaHash)
	}
	return nil
}

// copyFile 逐字节复制文件，用于 metadata-only 升级的 staging 拷贝。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// runUpgrade356 是 3.5.6 → 3.5.7 的 metadata-only 升级：源库已经是 Canonical 结构
// （hash == canonicalSchemaHash，仅 schema_version=3.5.6），不涉及 DDL 变化。
// 它只把版本标签从 3.5.6 改为 3.5.7，schema_hash / baseline_version / applied_at 原值保留。
// 升级在 staging 副本上完成，校验通过后才原子替换，失败可完整回滚。
func runUpgrade356(args []string) error {
	fs := flag.NewFlagSet("upgrade356", flag.ContinueOnError)
	in := fs.String("in", "", "3.5.6 Canonical 数据库路径（只读）")
	out := fs.String("out", "", "新数据库路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("upgrade356 需要 --in、--out")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("目标已存在，拒绝覆盖: %s", *out)
	} else if !os.IsNotExist(err) {
		return err
	}
	if wal, err := os.Stat(*in + "-wal"); err == nil && wal.Size() > 0 {
		return fmt.Errorf("旧库存在未合并的 WAL（%s，%d 字节）；请先停服务并在副本上 checkpoint", *in+"-wal", wal.Size())
	}
	src, err := sql.Open("sqlite", "file:"+*in+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return err
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	if err := src.Ping(); err != nil {
		return fmt.Errorf("打开旧库: %w", err)
	}
	if _, err := src.Exec("PRAGMA query_only=ON"); err != nil {
		return err
	}
	m, err := readMetaRaw(src)
	if err != nil {
		return fmt.Errorf("读取源元数据: %w", err)
	}
	if m.SchemaVersion != "3.5.6" {
		return fmt.Errorf("upgrade356 要求源 schema_version=3.5.6，实际 %q", m.SchemaVersion)
	}
	// 元数据声明的结构哈希必须等于 Canonical；与 VerifyStructure（实际结构）分别验证，不能互相替代。
	if err := requireCanonicalMetadataHash(m.SchemaHash); err != nil {
		return err
	}
	if err := schema.VerifyStructure(src, canonicalSchemaHash); err != nil {
		return fmt.Errorf("源结构不是 Canonical（hash=%s）: %w", canonicalSchemaHash, err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", *out, os.Getpid())
	_ = os.Remove(tmp)
	if err := copyFile(*in, tmp); err != nil {
		return fmt.Errorf("复制源库到 staging: %w", err)
	}
	target, err := sql.Open("sqlite", "file:"+tmp+"?mode=rwc")
	if err != nil {
		os.Remove(tmp)
		return err
	}
	done := false
	defer func() {
		target.Close()
		if !done {
			os.Remove(tmp)
		}
	}()
	target.SetMaxOpenConns(1)
	if _, err := target.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	tx, err := target.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_meta SET schema_version=? WHERE id=1`, schema.Version); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := target.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	if err := checkIntegrity(target); err != nil {
		return err
	}
	if _, err := schema.Verify(target); err != nil {
		return fmt.Errorf("升级后校验失败: %w", err)
	}
	if err := target.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, *out); err != nil {
		return fmt.Errorf("发布升级结果: %w", err)
	}
	done = true
	report := map[string]any{
		"ok":                   true,
		"from":                 "3.5.6",
		"to":                   schema.Version,
		"schema_hash":          m.SchemaHash,
		"baseline_version":     m.BaselineVersion,
		"applied_at_unchanged": m.AppliedAt,
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(b))
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dbPath := fs.String("db", "", "数据库路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return errors.New("verify 需要 --db")
	}
	db, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	m, err := schema.Verify(db)
	if err != nil {
		return err
	}
	fmt.Printf("ok schema_version=%s baseline=%s hash=%s\n", m.SchemaVersion, m.BaselineVersion, m.SchemaHash)
	return nil
}

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	dbPath := fs.String("db", "", "数据库路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return errors.New("inspect 需要 --db")
	}
	db, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, n := range names {
		var c int
		if err := db.QueryRow(`SELECT count(*) FROM "` + strings.ReplaceAll(n, `"`, `""`) + `"`).Scan(&c); err != nil {
			return err
		}
		fmt.Printf("%-30s %d\n", n, c)
	}
	return nil
}

type tablePlan struct {
	Name        string
	SourceName  string
	SourceCols  map[string]bool
	TargetCols  []string
	Exprs       []string
	KeyCols     []string
	KeyExprs    []string
	Count       int64
	ContentHash string
}

type migrationPlan struct {
	Tables  []*tablePlan
	Counts  map[string]int64
	Skipped []string
}

// canonicalSchemaHash 是 3.5.7 Canonical DDL（internal/store/schema/canonical.sql）经
// schema.Hash 计算得到的冻结结构指纹；与 canonical.sql 字节级绑定，任何 DDL 改动都会改变它。
const canonicalSchemaHash = "7b984881e7875f712b9a71162cc9d3615690efe42a6f5db1185c1e5138e8f51e"

// inputSchemaFingerprints 是真实历史副本的 Schema 指纹。
// 它们由 schema.Hash 生成，包含表、列、主键、索引、触发器和外键定义；不包含业务行。
// 迁移前必须逐字节匹配，--force-baseline 不能绕过。
//
// 这些值必须与 docs/3.5.6-migration-hardening.md / 3.5.7 升级说明中的冻结值一致。
var inputSchemaFingerprints = map[string]string{
	"3.4.6": "972db548d1eac569aec7ae11d859cdfbcd9114a0ee2dd1217ca085ca5a4d835f",
	"3.5.4": "4af3f62e273685b10493cafdeb7c6745b8d98d87e017dc1445b42ca66308cffb",
	"3.5.5": "0842cd1efa4b7330cf53a7f9d98491597481cfcec80d1b5560bf8f9f0ef81170",
}

func buildPlan(target, src *sql.DB, baseline string, hostMap map[string]string) (*migrationPlan, error) {
	targetTables, err := tableNames(target, "main")
	if err != nil {
		return nil, err
	}
	srcTables, err := tableNames(src, "main")
	if err != nil {
		return nil, err
	}
	p := &migrationPlan{Counts: map[string]int64{}}
	names := make([]string, 0, len(targetTables))
	for name := range targetTables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "schema_meta" || name == "sqlite_sequence" {
			continue
		}
		sourceName := name
		if name == "schema_migrations_audit" {
			// 3.5.4 的历史表名是 schema_migrations；3.4.6 没有该表。
			sourceName = "schema_migrations"
		}
		if !srcTables[sourceName] {
			if name == "schema_migrations_audit" || allowedMissingSourceTable(baseline, name) {
				p.Skipped = append(p.Skipped, name+"（旧库无此表，保留空表）")
				continue
			}
			return nil, fmt.Errorf("baseline=%s 缺少 Canonical 表 %s 的输入来源；拒绝把未知结构当成空库", baseline, name)
		}
		tcols, err := columns(target, "main", name)
		if err != nil {
			return nil, err
		}
		scols, err := columns(src, "main", sourceName)
		if err != nil {
			return nil, err
		}
		tp := &tablePlan{Name: name, SourceName: sourceName, SourceCols: map[string]bool{}}
		for _, c := range scols {
			tp.SourceCols[c] = true
		}
		for _, c := range scols {
			if !containsString(tcols, c) && !allowedSourceExtra(baseline, name, c) {
				return nil, fmt.Errorf("表 %s 的源端多余列 %s 不在明确忽略清单；拒绝静默丢弃", sourceName, c)
			}
		}
		for _, c := range tcols {
			expr, ok := columnExpr(baseline, name, c, tp.SourceCols, hostMap)
			if !ok {
				return nil, fmt.Errorf("表 %s 的目标列 %s 没有明确映射；拒绝依赖隐式默认值", name, c)
			}
			tp.TargetCols = append(tp.TargetCols, c)
			tp.Exprs = append(tp.Exprs, expr)
		}
		if len(tp.TargetCols) == 0 {
			return nil, fmt.Errorf("表 %s 没有可搬迁的列", name)
		}
		keyCols, err := primaryKey(target, "main", name)
		if err != nil {
			return nil, err
		}
		if len(keyCols) == 0 {
			return nil, fmt.Errorf("表 %s 没有主键；无法做主键排序的确定性内容校验", name)
		}
		tp.KeyCols = keyCols
		for _, c := range keyCols {
			if !containsString(tp.TargetCols, c) {
				return nil, fmt.Errorf("表 %s 的主键列 %s 不在目标列映射中", name, c)
			}
			if !tp.SourceCols[c] {
				return nil, fmt.Errorf("表 %s 的源端缺少主键列 %s；拒绝用 rowid 猜测顺序", name, c)
			}
			tp.KeyExprs = append(tp.KeyExprs, qcol(c))
		}
		p.Tables = append(p.Tables, tp)
	}
	// 旧库中存在但目标没有的非空表必须报错，不能静默丢数据。
	for name := range srcTables {
		if name == "sqlite_sequence" || name == "_writable" {
			continue
		}
		if name == "schema_migrations" && baseline == "3.5.4" {
			// 已由 schema_migrations_audit 专项保留。
			continue
		}
		if !targetTables[name] {
			var n int64
			if err := src.QueryRow(`SELECT count(*) FROM "` + strings.ReplaceAll(name, `"`, `""`) + `"`).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, fmt.Errorf("旧库表 %s 有 %d 行，但 Canonical 目标没有对应表；拒绝静默丢弃", name, n)
			}
			p.Skipped = append(p.Skipped, name+"（旧库有表但为空，已忽略）")
		}
	}
	return p, nil
}

func (p *migrationPlan) validate() error {
	for _, t := range p.Tables {
		if t.Name == "sources" {
			for _, c := range []string{"id", "base_url", "docker_cap", "ghcr_cap", "github_cap"} {
				if !t.SourceCols[c] {
					return fmt.Errorf("sources 缺少必要列 %s；该旧版本需要单独的 V1 映射，不在本工具支持范围", c)
				}
			}
		}
		if t.Name == "checks" && !t.SourceCols["kind"] {
			return fmt.Errorf("checks 缺少 kind；该旧版本需要先执行 V1 的 _v1kind 映射，禁止静默填空")
		}
	}
	return nil
}

// allowedMissingSourceTable 列出各 baseline 明确没有、Canonical 允许以空表补齐的表。
// 3.5.4 不应缺少任何 Canonical 业务表；缺少就拒绝。
func allowedMissingSourceTable(baseline, table string) bool {
	if baseline == "3.4.6" {
		switch table {
		case "breaker_state", "registry_host_gen", "registry_host_recovery",
			"source_health_archive", "source_health_scope", "schema_migrations_audit":
			return true
		}
	}
	// 3.5.5 Canonical 业务表已与 3.5.7 完全一致，仅缺 schema_migrations_audit（升级后保持空表）。
	if baseline == "3.5.5" && table == "schema_migrations_audit" {
		return true
	}
	return false
}

// allowedSourceExtra 是源端多余列的显式忽略清单。只允许已经确认没有 Canonical
// 语义的列；任何新出现的多余列都必须先经过人工判断。
func allowedSourceExtra(baseline, table, col string) bool {
	return baseline == "3.5.4" && table == "sources" && col == "registry_identity_state"
}

// missingTargetExpr 是 3.4.6 缺失目标列的显式默认映射。不是“省略让 SQLite
// 自己填”，而是把默认值写进 copy 的 SELECT，参与逐行内容校验。
//
// 3.4.6 没有 repo_host，但 docker_cap/ghcr_cap 已经表达了源的服务能力：
// docker_cap=1 的 34 条历史记录就是 Docker Hub 镜像源；仅 ghcr_cap=1 的
// 2 条是 GHCR 源；其余保持空串（没有 Registry 归属）。不能统一写空串，
// 否则 Canonical 的 docker/ghcr 候选查询会把它们全部排除。
func missingTargetExpr(baseline, table, col string) (string, bool) {
	if baseline != "3.4.6" {
		return "", false
	}
	switch table + "." + col {
	case "sources.repo_host":
		return `CASE WHEN "docker_cap" = 1 THEN 'docker.io' WHEN "ghcr_cap" = 1 THEN 'ghcr.io' ELSE '' END`, true
	case "sources.registry_cap":
		return "-1", true
	case "sources.registry_path_mode":
		return "''", true
	case "sources.registry_probe_at":
		return "0", true
	case "sources.registry_probe_error":
		return "''", true
	case "source_events.neutral":
		return "0", true
	}
	return "", false
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// validateRegistryHosts 对旧库 sources.repo_host 的每个非空值调用
// registryhost.Check。空串是 3.5.4 的合法“无归属”，保持为空。
// 这里不猜测历史 Hub 语义，也不把未知 Host 静默写成空串。
func validateRegistryHosts(db *sql.DB, baseline string) (map[string]string, error) {
	out := map[string]string{}
	cols, err := columns(db, "main", "sources")
	if err != nil {
		return nil, fmt.Errorf("读取 sources 列: %w", err)
	}
	if !containsString(cols, "repo_host") {
		// 3.4.6 没有 repo_host；目标库在 missingTargetExpr 中按能力做显式映射，
		// 不存在需要校验的旧非空 Host。
		return out, nil
	}
	rows, err := db.Query(`SELECT DISTINCT repo_host FROM sources`)
	if err != nil {
		return nil, fmt.Errorf("读取 sources.repo_host: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if raw == "" {
			continue
		}
		norm, err := registryhost.Check(raw)
		if err != nil {
			return nil, fmt.Errorf("baseline=%s 的 sources.repo_host %q 未通过 Registry Host 校验: %w", baseline, raw, err)
		}
		out[raw] = norm
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func copyAll(db *sql.DB, p *migrationPlan) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range p.Tables {
		srcCount, err := countTable(tx, "src", t.SourceName)
		if err != nil {
			return err
		}
		cols := quoteList(t.TargetCols)
		exprs := strings.Join(t.Exprs, ",")
		q := fmt.Sprintf(`INSERT INTO main."%s" (%s) SELECT %s FROM src."%s"`,
			strings.ReplaceAll(t.Name, `"`, `""`), cols, exprs, strings.ReplaceAll(t.SourceName, `"`, `""`))
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("复制 %s: %w", t.Name, err)
		}
		dstCount, err := countTable(tx, "main", t.Name)
		if err != nil {
			return err
		}
		if dstCount != srcCount {
			return fmt.Errorf("表 %s 行数不一致：旧=%d 新=%d", t.Name, srcCount, dstCount)
		}
		srcHash, err := hashRows(tx, "src", t.SourceName, strings.Join(t.Exprs, ","), len(t.Exprs), t.KeyExprs)
		if err != nil {
			return fmt.Errorf("计算 %s 源端内容 hash: %w", t.Name, err)
		}
		dstHash, err := hashRows(tx, "main", t.Name, quoteList(t.TargetCols), len(t.TargetCols), t.KeyCols)
		if err != nil {
			return fmt.Errorf("计算 %s 目标端内容 hash: %w", t.Name, err)
		}
		if srcHash != dstHash {
			return fmt.Errorf("表 %s 内容 hash 不一致：源=%s 目标=%s", t.Name, srcHash, dstHash)
		}
		t.ContentHash = dstHash
		t.Count = dstCount
		p.Counts[t.Name] = dstCount
	}
	return tx.Commit()
}

// hashRows 流式计算一张表在指定列上的内容 hash。列顺序固定，主键排序固定；
// NULL、整数、浮点、字符串和 blob 都带类型与长度前缀，避免 "1" 与 1 混淆。
func hashRows(tx *sql.Tx, schemaName, table, colsSQL string, ncols int, keyExprs []string) (string, error) {
	q := `SELECT ` + colsSQL + ` FROM ` + schemaName + "." + qcol(table)
	if len(keyExprs) > 0 {
		q += ` ORDER BY ` + strings.Join(keyExprs, ",")
	}
	rows, err := tx.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	var n int64
	for rows.Next() {
		vals := make([]any, ncols)
		ptrs := make([]any, ncols)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for _, v := range vals {
			writeHashValue(h, v)
		}
		h.Write([]byte{0x1e})
		n++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	h.Write([]byte(fmt.Sprintf("count=%d", n)))
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func writeHashValue(h interface{ Write([]byte) (int, error) }, v any) {
	switch x := v.(type) {
	case nil:
		h.Write([]byte{'N'})
	case int64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(x))
		h.Write([]byte{'I'})
		h.Write(b[:])
	case int:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(x))
		h.Write([]byte{'I'})
		h.Write(b[:])
	case float64:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], mathFloat64bits(x))
		h.Write([]byte{'F'})
		h.Write(b[:])
	case bool:
		if x {
			h.Write([]byte{'B', 1})
		} else {
			h.Write([]byte{'B', 0})
		}
	case string:
		writeHashBytes(h, 'S', []byte(x))
	case []byte:
		writeHashBytes(h, 'Y', x)
	default:
		writeHashBytes(h, 'X', []byte(fmt.Sprintf("%T:%v", x, x)))
	}
}

func writeHashBytes(h interface{ Write([]byte) (int, error) }, tag byte, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	h.Write([]byte{tag})
	h.Write(n[:])
	h.Write(b)
}

func mathFloat64bits(f float64) uint64 {
	return math.Float64bits(f)
}

func primaryKey(db *sql.DB, schemaName, table string) ([]string, error) {
	q := `PRAGMA ` + schemaName + `.table_info(` + qcol(table) + `)`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pkCol struct {
		seq  int
		name string
	}
	var cols []pkCol
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		if pk > 0 {
			cols = append(cols, pkCol{pk, name})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].seq < cols[j].seq })
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.name
	}
	return out, nil
}

func rebuildSequences(db *sql.DB, p *migrationPlan) error {
	// 只处理真正带 AUTOINCREMENT 的表。按实际最大 ID 重建，不复制旧
	// sqlite_sequence 的滞后值。
	var names []string
	rows, err := db.Query(`SELECT name,sql FROM sqlite_master WHERE type='table' AND sql LIKE '%AUTOINCREMENT%'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n, s string
		if err := rows.Scan(&n, &s); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Strings(names)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM sqlite_sequence`); err != nil {
		return err
	}
	for _, n := range names {
		var max sql.NullInt64
		q := `SELECT max(id) FROM "` + strings.ReplaceAll(n, `"`, `""`) + `"`
		if err := tx.QueryRow(q).Scan(&max); err != nil {
			return err
		}
		if !max.Valid {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO sqlite_sequence(name,seq) VALUES(?,?)`, n, max.Int64); err != nil {
			return fmt.Errorf("重建 %s 的 sqlite_sequence: %w", n, err)
		}
	}
	return tx.Commit()
}

func checkIntegrity(db *sql.DB) error {
	var fk sql.NullString
	err := db.QueryRow(`SELECT group_concat("table" || ':' || rowid) FROM pragma_foreign_key_check`).Scan(&fk)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	if fk.Valid && fk.String != "" {
		return fmt.Errorf("foreign_key_check 发现孤儿记录: %s", fk.String)
	}
	var ok string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if ok != "ok" {
		return fmt.Errorf("integrity_check: %s", ok)
	}
	return nil
}

func columnExpr(baseline, table, col string, src map[string]bool, hostMap map[string]string) (string, bool) {
	if src[col] {
		if table == "sources" && col == "repo_host" && baseline == "3.5.4" && len(hostMap) > 0 {
			var changed []string
			for raw, norm := range hostMap {
				if raw != norm {
					changed = append(changed, sqlLiteral(raw)+" THEN "+sqlLiteral(norm))
				}
			}
			if len(changed) > 0 {
				sort.Strings(changed)
				return "CASE " + qcol(col) + " WHEN " + strings.Join(changed, " WHEN ") + " ELSE " + qcol(col) + " END", true
			}
		}
		return qcol(col), true
	}
	return missingTargetExpr(baseline, table, col)
}

func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func qcol(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteList(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = qcol(c)
	}
	return strings.Join(out, ",")
}

func columns(db *sql.DB, schemaName, table string) ([]string, error) {
	q := `PRAGMA ` + schemaName + `.table_info(` + qcol(table) + `)`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func tableNames(db *sql.DB, schemaName string) (map[string]bool, error) {
	q := `SELECT name FROM ` + schemaName + `.sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

func countTable(tx *sql.Tx, schemaName, table string) (int64, error) {
	var n int64
	q := `SELECT count(*) FROM ` + schemaName + `."` + strings.ReplaceAll(table, `"`, `""`) + `"`
	err := tx.QueryRow(q).Scan(&n)
	return n, err
}
