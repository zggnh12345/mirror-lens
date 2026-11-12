package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// expireCand 是物化候选行在事务内的快照。
type expireCand struct {
	SourceID      int64
	Scope         string
	Gen           int64
	LastProbeAt   int64
	LastSuccessAt int64
	RecFailures   int
	LastError     string
	Cap           int
}

// updateExpiredCAS 只允许 ok → expired，并用 state/last_success_at/gen 三列
// 做条件更新。任何一列与读取时不一致都视为状态已变化，返回 0 行、不覆盖。
func updateExpiredCAS(ctx context.Context, conn *sql.Conn, c expireCand, now time.Time) (int64, error) {
	res, err := conn.ExecContext(ctx,
		`UPDATE registry_host_recovery
		 SET state=?, next_probe_at=?, updated_at=?
		 WHERE source_id=? AND family='registry' AND scope=?
		   AND state='ok' AND last_success_at=? AND gen=?`,
		RecoveryExpired, now.Unix(), now.Unix(), c.SourceID, c.Scope, c.LastSuccessAt, c.Gen)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MaterializeExpired 把「cap=1、成功证据已超 TTL」的 ok 行推进为 expired，
// 并把 next_probe_at 拉到 now，使本轮/下一轮扫描重新验证。
//
// 全部动作在一个 BEGIN IMMEDIATE 事务内：读取候选 → 逐行按当前代次的
// scoped 证据调用 DeriveState 复核 → 仅当结果确为 expired 时做 CAS 更新。
// 除此之外的状态（failed / initial / unsupported / hard-excluded）一律不碰，
// 也不修正任何「不一致」，避免把物化任务变成通用状态修复器。
//
// now 由调用方提供，与同一轮 ListDueHostProbes 使用同一个时间点；
// Store 内部不再取当前时间。返回实际成功物化的行数。
func (s *Store) MaterializeExpired(now time.Time) (int, error) {
	ttl := s.RecoveryTTL()
	if ttl < time.Second {
		// cutoff 是秒级：小于 1s 的 TTL 会被截断成 0，语义上等于「全部失效」。
		// 配置入口已拒绝这类值，这里再做一次兜底。
		ttl = time.Second
	}
	cutoff := now.Unix() - int64(ttl/time.Second)

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	rows, err := conn.QueryContext(ctx, `
		SELECT r.source_id, r.scope, r.gen, r.last_probe_at, r.last_success_at,
		       r.rec_failures, r.last_error, s.registry_cap
		FROM registry_host_recovery r
		JOIN sources s ON s.id = r.source_id
		WHERE r.family='registry' AND r.state='ok' AND s.registry_cap=1
		  AND r.last_success_at > 0 AND r.last_success_at < ?
		  AND s.status <> 'archived'`, cutoff)
	if err != nil {
		return 0, err
	}
	var cands []expireCand
	for rows.Next() {
		var c expireCand
		if err := rows.Scan(&c.SourceID, &c.Scope, &c.Gen, &c.LastProbeAt,
			&c.LastSuccessAt, &c.RecFailures, &c.LastError, &c.Cap); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	n := 0
	for _, c := range cands {
		var curGen int64
		err := conn.QueryRowContext(ctx,
			`SELECT gen FROM registry_host_gen WHERE host=?`, c.Scope).Scan(&curGen)
		if errors.Is(err, sql.ErrNoRows) {
			curGen = 0
		} else if err != nil {
			return n, err
		}
		// 代次不等说明行属于旧结论：派生必然不是 expired，直接跳过。
		// （CAS 里的 gen 条件仍是并发防御，不替代这里的判断。）
		if curGen != c.Gen {
			continue
		}

		hasScoped := true
		var scopedFails int
		err = conn.QueryRowContext(ctx,
			`SELECT fails FROM source_health_scope
			 WHERE source_id=? AND family='registry' AND scope=? AND gen=?`,
			c.SourceID, c.Scope, curGen).Scan(&scopedFails)
		if errors.Is(err, sql.ErrNoRows) {
			hasScoped = false
			scopedFails = 0
		} else if err != nil {
			return n, err
		}

		state := DeriveState(DeriveInput{
			Cap:           c.Cap,
			Now:           now.Unix(),
			TTL:           ttl,
			CurrentGen:    curGen,
			HasRecovery:   true,
			RecoveryGen:   c.Gen,
			LastProbeAt:   c.LastProbeAt,
			LastSuccessAt: c.LastSuccessAt,
			RecFailures:   c.RecFailures,
			LastError:     c.LastError,
			HasScoped:     hasScoped,
			ScopedGen:     curGen,
			ScopedFails:   scopedFails,
		})
		if state != RecoveryExpired {
			continue
		}

		affected, err := updateExpiredCAS(ctx, conn, c, now)
		if err != nil {
			return n, err
		}
		n += int(affected)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return n, err
	}
	committed = true
	return n, nil
}
