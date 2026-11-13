package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// recoverySeedKey 是 C6-E 一次性迁移的版本化标记。标记存在即整个种子
// 过程跳过：手动删掉的恢复行不会在下次启动被补回来。
const recoverySeedKey = "recovery_seed_v1"

// seedRecoveryFromScope 为存量 Host 建立缺失的 registry_host_recovery 行。
//
// 信息边界（C6-E 的核心约束）：3.5.1 及以前的 scoped 证据不能证明属于
// 当前凭据代次——历史行在迁移时统一补成 gen=0，与后续无凭据探测写出的
// gen=0 落在同一整数空间，无法区分。因此这里刻意不读 source_health_scope，
// 也不用历史 fails / ok_at 派生 ok、failed 或 expired。
//
// 只做三件事：
//   - 范围内的存量 Host 建缺失行，一律 initial；
//   - cap=2 固化 hard-excluded；
//   - cap=0 固化 unsupported，按既有 6h 节奏重验。
//
// 首次升级由此出现一次全量重探窗口，这是数据模型的信息边界，不是缺陷。
func (s *Store) seedRecoveryFromScope() error {
	ctx := context.Background()

	var mark string
	err := s.db.QueryRowContext(ctx,
		`SELECT v FROM meta WHERE k=?`, recoverySeedKey).Scan(&mark)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("读恢复表种子标记: %w", err)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// 独立背景 context：即便调用方 ctx 已取消也要释放写锁。
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	rows, err := conn.QueryContext(ctx, `
SELECT s.id, s.repo_host, s.registry_cap
FROM sources s
WHERE s.status <> 'archived'
  AND s.repo_host <> ''
  AND s.repo_host <> 'ghcr.io'
  AND s.repo_host <> 'docker.io'`)
	if err != nil {
		return fmt.Errorf("读存量 Host: %w", err)
	}

	type seedRow struct {
		sourceID int64
		host     string
		cap      int
	}
	var seeds []seedRow
	for rows.Next() {
		var r seedRow
		if err := rows.Scan(&r.sourceID, &r.host, &r.cap); err != nil {
			rows.Close()
			return err
		}
		seeds = append(seeds, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	now := s.nowTime().Unix()
	unsupportedNext := now + int64(recoveryUnsupportedInterval/time.Second)

	for _, r := range seeds {
		state, next := RecoveryInitial, now
		switch r.cap {
		case RegistryCapMismatch:
			state, next = RecoveryHardExcluded, 0
		case RegistryCapNo:
			state, next = RecoveryUnsupported, unsupportedNext
		}

		// gen / 历史时间一律留空：不把未知代次的历史证据写成当前事实。
		if _, err := conn.ExecContext(ctx, `
INSERT INTO registry_host_recovery
 (source_id, family, scope, state, gen, last_probe_at, last_success_at,
  next_probe_at, rec_failures, last_error, updated_at)
VALUES(?,?,?,?,0,0,0,?,0,'',?)
ON CONFLICT(source_id, family, scope) DO NOTHING`,
			r.sourceID, "registry", r.host, state, next, now); err != nil {
			return fmt.Errorf("写入恢复行 source=%d host=%s: %w", r.sourceID, r.host, err)
		}
	}

	if _, err := conn.ExecContext(ctx, `
INSERT INTO meta(k, v) VALUES(?, '1')
ON CONFLICT(k) DO UPDATE SET v='1'`, recoverySeedKey); err != nil {
		return fmt.Errorf("写恢复表种子标记: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
