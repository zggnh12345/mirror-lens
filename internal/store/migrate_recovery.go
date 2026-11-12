package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// scopedMigrationHook 仅供测试注入失败，用来验证迁移中途出错时旧表、数据和外键
// 都能完整回滚。生产环境为 nil。
var scopedMigrationHook func(stage string) error

const recoveryDDL = `
CREATE TABLE IF NOT EXISTS registry_host_recovery (
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  family TEXT NOT NULL DEFAULT 'registry',
  scope TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'initial',
  gen INTEGER NOT NULL DEFAULT 0,
  last_probe_at INTEGER NOT NULL DEFAULT 0,
  last_success_at INTEGER NOT NULL DEFAULT 0,
  next_probe_at INTEGER NOT NULL DEFAULT 0,
  rec_failures INTEGER NOT NULL DEFAULT 0,
  retry_failures INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(source_id, family, scope)
);
CREATE INDEX IF NOT EXISTS idx_recovery_due ON registry_host_recovery(next_probe_at);
`

const scopedNewDDL = `
CREATE TABLE source_health_scope_new (
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  family TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '',
  gen INTEGER NOT NULL DEFAULT 0,
  fails INTEGER NOT NULL DEFAULT 0,
  ok_at INTEGER NOT NULL DEFAULT 0,
  last_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(source_id, family, scope, gen)
)`

func (s *Store) migrateRecoverySchema() error {
	if _, err := s.db.Exec(recoveryDDL); err != nil {
		return fmt.Errorf("建 registry_host_recovery: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS 不会给既有表补列，老库必须显式 ALTER。
	// retry_failures 的默认值 0 等价于「没有未完成的退避」，升级时不需要回填。
	if err := s.ensureColumns("registry_host_recovery", map[string]string{
		"retry_failures": "INTEGER NOT NULL DEFAULT 0",
	}); err != nil {
		return err
	}
	if err := s.migrateScopedGen(); err != nil {
		return err
	}
	return s.seedRecoveryFromScope()
}

// migrateScopedGen 把 3.5.1 的 source_health_scope(source_id,family,scope)
// 重建为四列主键并给历史行补 gen=0。已是四列主键时直接返回，保证可重复执行。
func (s *Store) migrateScopedGen() error {
	_, pk, err := pkColumns(context.Background(), s.db, "source_health_scope")
	if err != nil {
		return err
	}
	if pk == "source_id,family,scope,gen" {
		return nil
	}
	if pk != "source_id,family,scope" {
		return fmt.Errorf("source_health_scope 主键形态未知(%q)，拒绝自动迁移", pk)
	}

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// foreign_keys 是连接级开关，且必须在事务外切换；关闭后仍可执行 foreign_key_check。
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	// 失败与成功路径都要恢复：这个 defer 比 ROLLBACK 后注册，因此先回滚再打开外键。
	defer conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON")

	idxs, err := indexDDL(ctx, conn, "source_health_scope")
	if err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// Background：即使 ctx 已取消也要把锁放掉。
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	stage := func(name string) error {
		if scopedMigrationHook == nil {
			return nil
		}
		return scopedMigrationHook(name)
	}

	if err := stage("before_create"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS source_health_scope_new`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, scopedNewDDL); err != nil {
		return err
	}

	var oldN, newN int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_health_scope`).Scan(&oldN); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO source_health_scope_new(source_id,family,scope,gen,fails,ok_at,last_at)
		SELECT source_id,family,scope,0,fails,ok_at,last_at FROM source_health_scope`); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_health_scope_new`).Scan(&newN); err != nil {
		return err
	}
	if oldN != newN {
		return fmt.Errorf("source_health_scope 迁移行数不一致: old=%d new=%d", oldN, newN)
	}

	if err := stage("before_drop"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE source_health_scope`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE source_health_scope_new RENAME TO source_health_scope`); err != nil {
		return err
	}
	for _, ddl := range idxs {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("重建索引: %w", err)
		}
	}

	// 提交前校验：表结构、外键、完整性。提交后再发现就来不及回滚了。
	if _, pk, err := pkColumns(ctx, conn, "source_health_scope"); err != nil {
		return err
	} else if pk != "source_id,family,scope,gen" {
		return fmt.Errorf("迁移后主键顺序不符: %q", pk)
	}
	if err := checkForeignKeys(ctx, conn); err != nil {
		return err
	}
	var integrity string
	if err := conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity_check 失败: %s", integrity)
	}

	if err := stage("before_commit"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pkColumns 返回按主键序号排列的列名，例如 "source_id,family,scope,gen"。
func pkColumns(ctx context.Context, q rowQuerier, table string) (map[string]bool, string, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	cols := map[string]bool{}
	type pkCol struct {
		seq  int
		name string
	}
	var pks []pkCol
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, "", err
		}
		cols[name] = true
		if pk > 0 {
			pks = append(pks, pkCol{pk, name})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i].seq < pks[j].seq })
	names := make([]string, len(pks))
	for i, c := range pks {
		names[i] = c.name
	}
	return cols, strings.Join(names, ","), nil
}

func indexDDL(ctx context.Context, q rowQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ddl sql.NullString
		if err := rows.Scan(&ddl); err != nil {
			return nil, err
		}
		if ddl.Valid {
			out = append(out, ddl.String)
		}
	}
	return out, rows.Err()
}

func checkForeignKeys(ctx context.Context, q rowQuerier) error {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("foreign_key_check 有 %d 行违规", n)
	}
	return nil
}
