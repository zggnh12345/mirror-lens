package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// Legacy Registry 数据退出（Phase B）。
//
// A1 之后 registryCandidatesRaw 不再读 source_health，NoteProbeScoped 也拒绝
// family='registry'，因此 source_health 里的 registry 行已是纯历史数据。这里
// 把它们在单个事务内归档、清理并写迁移标记，失败整体回滚、下次启动重试。
//
// 覆盖口径：按 family 全量归档，不按 status / enabled / registry_cap 过滤。
// registry_cap 只决定候选资格，与历史数据去留无关；用 cap 过滤会漏掉
// archived 源与空 repo_host 源，重新启用时留下复活否决权的路径。

const (
	legacyArchiveBatch  = "b1"
	legacyArchiveMarker = "b1-archive-legacy-registry"
)

// dbtx 是 *sql.Conn 与 *sql.Tx 的公共子集。
// BEGIN IMMEDIATE 只能通过专用 *sql.Conn 发起，事务内所有语句必须走同一连接。
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LegacyArchiveResult 是一次归档迁移的结果。
type LegacyArchiveResult struct {
	Candidates  int64 // 迁移前 source_health 中 family='registry' 的行数
	Archived    int64 // 本次新写入归档表的行数
	Removed     int64 // 本次从 source_health 删除的行数
	Left        int64 // 提交前剩余行数（必须为 0）
	AlreadyDone bool  // 迁移标记已存在且无残留
}

// LegacyUnarchiveResult 是一次回滚的结果。
type LegacyUnarchiveResult struct {
	Restored int64 // 补回 source_health 的行数
	Skipped  int64 // 目标行已存在、按冲突策略跳过的行数
}

func (s *Store) migrateLegacyRegistry() error {
	start := time.Now()
	res, err := s.ArchiveLegacyRegistry(context.Background())
	if err != nil {
		log.Printf("迁移步骤 %s 失败: %v", legacyArchiveMarker, err)
		return err
	}
	log.Printf("迁移步骤 %s 完成: batch=%s candidates=%d archived=%d removed=%d left=%d already_done=%t duration=%s",
		legacyArchiveMarker, legacyArchiveBatch, res.Candidates, res.Archived,
		res.Removed, res.Left, res.AlreadyDone, time.Since(start).Round(time.Millisecond))
	return nil
}

// beginImmediate 在专用连接上开写事务。database/sql 的 BeginTx 只能发出
// 默认的 BEGIN（DEFERRED），先读后写时会直接 SQLITE_BUSY；这里显式 IMMEDIATE，
// 由 busy_timeout(5000) 排队。提交/回滚与连接释放由 done 负责。
func (s *Store) beginImmediate(ctx context.Context) (*sql.Conn, func(bool) error, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	done := func(commit bool) error {
		defer conn.Close()
		if commit {
			_, err := conn.ExecContext(ctx, "COMMIT")
			return err
		}
		// Background：即使 ctx 已取消也要把写锁放掉。
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return nil
	}
	return conn, done, nil
}

// ArchiveLegacyRegistry 归档并清理 source_health 中的 legacy registry 行。
//
// 前置：schema_migrations 与 source_health_archive 已由 migrate() 建好。
// 幂等：同一 batch 重跑时 0 写 0 删；若标记存在但仍有 legacy 行，返回错误
// 而不是静默跳过，避免「标记成功、数据没清」的假完成。
func (s *Store) ArchiveLegacyRegistry(ctx context.Context) (LegacyArchiveResult, error) {
	var out LegacyArchiveResult
	now := s.nowTime().Unix()

	conn, done, err := s.beginImmediate(ctx)
	if err != nil {
		return out, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = done(false)
		}
	}()

	var marked int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE name=?`, legacyArchiveMarker).Scan(&marked); err != nil {
		return out, err
	}

	var remaining int64
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM source_health WHERE family='registry'`).Scan(&remaining); err != nil {
		return out, err
	}
	out.Candidates = remaining
	if remaining == 0 {
		out.AlreadyDone = marked > 0
		if err := recordLegacyMarker(ctx, conn, now); err != nil {
			return out, err
		}
		if err := done(true); err != nil {
			return out, err
		}
		committed = true
		return out, nil
	}

	res, err := conn.ExecContext(ctx, `
INSERT INTO source_health_archive
  (source_id, family, fails, ok_at, last_at, archived_at, batch)
SELECT source_id, family, fails, ok_at, last_at, ?, ?
FROM source_health
WHERE family='registry'
  AND NOT EXISTS (
    SELECT 1 FROM source_health_archive a
    WHERE a.source_id = source_health.source_id
      AND a.family = source_health.family
      AND a.batch = ?)`, now, legacyArchiveBatch, legacyArchiveBatch)
	if err != nil {
		return out, err
	}
	archived, err := res.RowsAffected()
	if err != nil {
		return out, err
	}

	del, err := conn.ExecContext(ctx, `DELETE FROM source_health WHERE family='registry'`)
	if err != nil {
		return out, err
	}
	removed, err := del.RowsAffected()
	if err != nil {
		return out, err
	}

	// 事务内自校验：删除数 = 本次新归档数 + 已在归档表中的同批行数。
	// 后者只会在 un-archive 之后重跑时出现，不能算作「字段错位」。
	var dup int64
	if err := conn.QueryRowContext(ctx, `
SELECT COUNT(*) FROM source_health h
WHERE h.family='registry' AND EXISTS (
  SELECT 1 FROM source_health_archive a
  WHERE a.source_id=h.source_id AND a.family=h.family AND a.batch=?)`,
		legacyArchiveBatch).Scan(&dup); err != nil {
		return out, err
	}
	if removed != archived+dup {
		return out, fmt.Errorf("归档行数与删除行数不一致: archived=%d dup=%d removed=%d", archived, dup, removed)
	}
	var left int64
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM source_health WHERE family='registry'`).Scan(&left); err != nil {
		return out, err
	}
	if left != 0 {
		return out, fmt.Errorf("清理后仍有 %d 行 legacy registry 数据", left)
	}
	out.Left = left

	if err := recordLegacyMarker(ctx, conn, now); err != nil {
		return out, err
	}
	if err := done(true); err != nil {
		return out, err
	}
	committed = true
	out.Archived, out.Removed = archived, removed
	return out, nil
}

// UnarchiveLegacyRegistry 把指定批次的历史行补回 source_health。
//
// 冲突策略：目标 (source_id, family) 已存在行时跳过、不覆盖，由调用方
// 根据 Skipped 决定是否人工核对。归档行本身不删除，回滚动作可重复执行。
func (s *Store) UnarchiveLegacyRegistry(ctx context.Context, batch string) (LegacyUnarchiveResult, error) {
	var out LegacyUnarchiveResult
	conn, done, err := s.beginImmediate(ctx)
	if err != nil {
		return out, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = done(false)
		}
	}()

	var total int64
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM source_health_archive WHERE batch=?`, batch).Scan(&total); err != nil {
		return out, err
	}
	res, err := conn.ExecContext(ctx, `
INSERT INTO source_health(source_id, family, fails, ok_at, last_at)
SELECT source_id, family, fails, ok_at, last_at
FROM source_health_archive a
WHERE a.batch=?
  AND NOT EXISTS (
    SELECT 1 FROM source_health h
    WHERE h.source_id=a.source_id AND h.family=a.family)`, batch)
	if err != nil {
		return out, err
	}
	restored, err := res.RowsAffected()
	if err != nil {
		return out, err
	}
	if err := done(true); err != nil {
		return out, err
	}
	committed = true
	out.Restored = restored
	out.Skipped = total - restored
	return out, nil
}

func recordLegacyMarker(ctx context.Context, tx dbtx, now int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(name, applied_at) VALUES(?,?)
		 ON CONFLICT(name) DO UPDATE SET applied_at=excluded.applied_at`,
		legacyArchiveMarker, now)
	return err
}
