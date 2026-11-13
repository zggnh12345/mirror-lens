// db-migrate 是 Mirror Lens 的离线数据库搬迁工具。
//
// 它只读打开旧库，新建 Canonical 目标库，按明确映射复制数据并校验。
// 运行时不会调用这里的任何逻辑；服务启动只接受带 schema_meta 的 Canonical 库。
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

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
  db-migrate init    --out 新库 [--baseline fresh]
  db-migrate checkpoint --db 副本数据库
  db-migrate migrate --in 旧库 --out 新库 --baseline 3.4.6|3.5.4 [--force-baseline]
  db-migrate verify  --db 数据库
  db-migrate inspect --db 数据库

迁移前必须停服务，并确保旧库没有未合并的 -wal。工具会拒绝覆盖已存在的目标文件。`)
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
	baseline := fs.String("baseline", "", "旧库版本标识，例如 3.4.6 / 3.5.4")
	force := fs.Bool("force-baseline", false, "允许覆盖 schema_meta 的 baseline 字符串（仅审计用途）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" || *baseline == "" {
		return errors.New("migrate 需要 --in、--out、--baseline")
	}
	if !*force && *baseline != "3.4.6" && *baseline != "3.5.4" {
		return fmt.Errorf("未识别的 baseline %q；如确属受支持版本请加 --force-baseline", *baseline)
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

	plan, err := buildPlan(target, src)
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
	if _, err := target.Exec(`UPDATE schema_meta SET schema_hash=?, applied_at=? WHERE schema_version=?`,
		h, time.Now().Unix(), schema.Version); err != nil {
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
	Name       string
	SourceCols map[string]bool
	TargetCols []string
	Exprs      []string
	Count      int64
}

type migrationPlan struct {
	Tables  []*tablePlan
	Counts  map[string]int64
	Skipped []string
}

// 这些旧表有数据，但 Canonical 明确不保留；必须在报告里逐项列出，
// 不能走“有数据就拒绝”的通用路径，也不能静默丢。
var retiredTables = map[string]string{
	"schema_migrations": "历史迁移记录已冻结到离线工具，不写入 Canonical",
}

func buildPlan(target, src *sql.DB) (*migrationPlan, error) {
	targetTables, err := tableNames(target, "main")
	if err != nil {
		return nil, err
	}
	srcTables, err := tableNames(src, "main")
	if err != nil {
		return nil, err
	}
	p := &migrationPlan{Counts: map[string]int64{}}
	// sources 必须最先拷贝；其余表按名字排序，外键已关闭，顺序不影响正确性。
	names := make([]string, 0, len(targetTables))
	for name := range targetTables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "schema_meta" {
			continue
		}
		if name == "sqlite_sequence" {
			continue
		}
		if !srcTables[name] {
			p.Skipped = append(p.Skipped, name+"（旧库无此表，保留空表）")
			continue
		}
		tcols, err := columns(target, "main", name)
		if err != nil {
			return nil, err
		}
		scols, err := columns(src, "main", name)
		if err != nil {
			return nil, err
		}
		tp := &tablePlan{Name: name, SourceCols: map[string]bool{}}
		for _, c := range scols {
			tp.SourceCols[c] = true
		}
		for _, c := range tcols {
			expr, ok := columnExpr(name, c, tp.SourceCols)
			if !ok {
				continue // 目标列有默认值，省略即可
			}
			tp.TargetCols = append(tp.TargetCols, c)
			tp.Exprs = append(tp.Exprs, expr)
		}
		if len(tp.TargetCols) == 0 {
			return nil, fmt.Errorf("表 %s 没有可搬迁的列", name)
		}
		p.Tables = append(p.Tables, tp)
	}
	// 旧库中存在但目标没有的非空表必须报错，不能静默丢数据。
	for name := range srcTables {
		if name == "sqlite_sequence" || name == "_writable" {
			continue
		}
		if !targetTables[name] {
			if reason, ok := retiredTables[name]; ok {
				var n int64
				if err := src.QueryRow(`SELECT count(*) FROM "` + strings.ReplaceAll(name, `"`, `""`) + `"`).Scan(&n); err == nil {
					p.Skipped = append(p.Skipped, fmt.Sprintf("%s（%d 行；%s）", name, n, reason))
				} else {
					p.Skipped = append(p.Skipped, name+"（"+reason+"）")
				}
				continue
			}
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

func copyAll(db *sql.DB, p *migrationPlan) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range p.Tables {
		srcCount, err := countTable(tx, "src", t.Name)
		if err != nil {
			return err
		}
		cols := quoteList(t.TargetCols)
		exprs := strings.Join(t.Exprs, ",")
		q := fmt.Sprintf(`INSERT INTO main."%s" (%s) SELECT %s FROM src."%s"`,
			strings.ReplaceAll(t.Name, `"`, `""`), cols, exprs, strings.ReplaceAll(t.Name, `"`, `""`))
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
		t.Count = dstCount
		p.Counts[t.Name] = dstCount
	}
	return tx.Commit()
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

func columnExpr(table, col string, src map[string]bool) (string, bool) {
	if table == "sources" {
		host := repoHostExpr(src)
		switch col {
		case "repo_host":
			return host, true
		case "registry_cap":
			if src[col] {
				return qcol(col), true
			}
			return "CASE WHEN " + host + " = 'ghcr.io' THEN 1 ELSE -1 END", true
		case "registry_path_mode":
			if src[col] {
				return qcol(col), true
			}
			return "CASE WHEN " + host + " = 'ghcr.io' THEN 'native' ELSE '' END", true
		case "registry_probe_at":
			if src[col] {
				return qcol(col), true
			}
			return "0", true
		case "registry_probe_error":
			if src[col] {
				return qcol(col), true
			}
			return "''", true
		}
	}
	if src[col] {
		return qcol(col), true
	}
	return "", false
}

func repoHostExpr(src map[string]bool) string {
	var host string
	if src["repo_host"] {
		host = "lower(trim(repo_host))"
	} else {
		host = "''"
	}
	h := "CASE " +
		"WHEN " + host + " IN ('registry-1.docker.io','index.docker.io','docker.io') THEN 'docker.io' " +
		"WHEN " + host + " = 'ghcr.io' THEN 'ghcr.io' " +
		"WHEN " + host + " = '' AND docker_cap = 1 THEN 'docker.io' " +
		"WHEN " + host + " = '' AND ghcr_cap = 1 AND docker_cap <> 1 THEN 'ghcr.io' " +
		"ELSE " + host + " END"
	return h
}

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
