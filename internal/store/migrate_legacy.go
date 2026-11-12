package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
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
// 由 busy_timeout(5000) 排队。
//
// done 只能真正结束事务一次：
//   - 提交成功 → 关闭连接；
//   - 提交失败 → 事务通常仍活着，必须在同一连接上 ROLLBACK 再关闭，
//     提交错误与回滚错误都要带回，不能静默吞掉；
//   - 之后外层 defer 再调用是空操作，不会去操作已经关闭的连接。
func (s *Store) beginImmediate(ctx context.Context) (*sql.Conn, func(bool) error, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	finished := false
	done := func(commit bool) error {
		if finished {
			return errTxFinished
		}
		finished = true
		defer conn.Close()
		if commit {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				// 提交失败：先在原连接上回滚，把写锁放掉。
				_, rbErr := conn.ExecContext(context.Background(), "ROLLBACK")
				if rbErr != nil && !isNoActiveTx(rbErr) {
					return fmt.Errorf("提交失败: %w（回滚也失败: %v）", err, rbErr)
				}
				return fmt.Errorf("提交失败: %w", err)
			}
			return nil
		}
		// Background：即使 ctx 已取消也要把写锁放掉。
		_, rbErr := conn.ExecContext(context.Background(), "ROLLBACK")
		if rbErr != nil && !isNoActiveTx(rbErr) {
			return fmt.Errorf("回滚失败: %w", rbErr)
		}
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
	// 标记存在但又有 legacy 行：这是「迁移完成后数据被重新写回」的信号，
	// 不能再次静默归档删除，否则等于每次启动都无条件清掉人工恢复的数据。
	// 停机交给运维判断；要重跑迁移必须先 Unarchive 撤销标记。
	if marked > 0 && remaining > 0 {
		return out, fmt.Errorf("迁移标记 %s 已存在，但 source_health 仍有 %d 行 legacy registry 数据: "+
			"拒绝再次归档；如需重跑，先调用 UnarchiveLegacyRegistry 撤销标记", legacyArchiveMarker, remaining)
	}
	if remaining == 0 {
		out.AlreadyDone = marked > 0
		if marked == 0 {
			if err := recordLegacyMarker(ctx, conn, now); err != nil {
				return out, err
			}
		}
		if err := done(true); err != nil {
			return out, err
		}
		committed = true
		return out, nil
	}

	// 先数「已在归档表中的同批行」：删除之后就查不到了。
	// 这类行只在 unarchive 后重跑时出现，归档 0 行但删除 N 行是正常的。
	var dup int64
	if err := conn.QueryRowContext(ctx, `
SELECT COUNT(*) FROM source_health h
WHERE h.family='registry' AND EXISTS (
  SELECT 1 FROM source_health_archive a
  WHERE a.source_id=h.source_id AND a.family=h.family AND a.batch=?)`,
		legacyArchiveBatch).Scan(&dup); err != nil {
		return out, err
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

	// 事务内自校验：删除数 = 本次新归档数 + 重跑前已在归档表中的行数。
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
	// 回滚必须同时撤销迁移标记（方案 A）：数据回到 source_health 之后，
	// 下次启动若仍认为「已迁移完成」，这批行就成了既没归档也没被清理的悬空数据。
	// 撤销标记后重跑，归档表的同批次行靠 dup 校验兜住，不会重复计数。
	if batch == legacyArchiveBatch {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM schema_migrations WHERE name=?`, legacyArchiveMarker); err != nil {
			return out, err
		}
	}
	if err := done(true); err != nil {
		return out, err
	}
	committed = true
	out.Restored = restored
	out.Skipped = total - restored
	return out, nil
}

// recordLegacyMarker 只写一次：标记存在时不动 applied_at，
// 否则每次启动都会刷新，迁移记录就不再是「首次完成时间」。
func recordLegacyMarker(ctx context.Context, tx dbtx, now int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(name, applied_at) VALUES(?,?)
		 ON CONFLICT(name) DO NOTHING`,
		legacyArchiveMarker, now)
	return err
}

// errTxFinished 表示事务已经提交或回滚过，防止外层清理二次操作。
var errTxFinished = errors.New("事务已结束")

// isNoActiveTx 判断是否「已经不在事务里」——COMMIT 报错但实际已提交时，
// 随后的 ROLLBACK 会返回这个错误，不算回滚失败。
func isNoActiveTx(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no transaction is active") || errors.Is(err, sql.ErrTxDone)
}
