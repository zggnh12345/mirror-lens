package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HostProbeRetry 是一次「未提交成功」的探测失败的重排请求。
//
// Gen 必须来自本次执行起点的快照（凭据读取失败时取任务入队时的代次），
// At 是失败时刻（Unix 秒）。两者都不由调用方随意给：CAS 会拒绝与当前
// 代次或到期状态不符的写入。
type HostProbeRetry struct {
	SourceID int64
	Host     string
	Gen      int64
	At       int64
}

const (
	// hostProbeRetryMaxFailures 是持久化退避阶梯的封顶次数。
	hostProbeRetryMaxFailures = 7
	// hostProbeRetryBase 是第一次失败的退避时长。
	hostProbeRetryBase = 15 * time.Second
	// hostProbeRetryCap 是单次退避的上限。
	hostProbeRetryCap = 10 * time.Minute
)

// HostProbeRetryDelay 返回第 n 次连续失败后的退避时长：15s 起，每次翻倍，
// 600s 封顶，不加抖动。n<1 按 1 处理。
func HostProbeRetryDelay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > hostProbeRetryMaxFailures {
		n = hostProbeRetryMaxFailures
	}
	d := hostProbeRetryBase
	for i := 1; i < n; i++ {
		d *= 2
		if d >= hostProbeRetryCap {
			return hostProbeRetryCap
		}
	}
	if d > hostProbeRetryCap {
		d = hostProbeRetryCap
	}
	return d
}

// ScheduleHostProbeRetry 把一个未提交成功的执行失败持久化为下一次到期时间。
//
// 返回 false 表示被拒绝：代次不符、行已不在到期状态、源被禁用/归档/换 Host、
// 或行处于 hard-excluded。拒绝时不写任何字段，也不清除退避计数。
//
// 只有 next_probe_at 与 retry_failures 两个字段被修改；state、rec_failures、
// last_probe_at、last_success_at、last_error 全部不动 —— 这是「本地执行失败」
// 与「探测结果失败」的边界。
func (s *Store) ScheduleHostProbeRetry(r HostProbeRetry) (bool, error) {
	if r.SourceID == 0 || r.Host == "" || r.At <= 0 {
		return false, fmt.Errorf("ScheduleHostProbeRetry: 参数不完整 source=%d host=%q at=%d", r.SourceID, r.Host, r.At)
	}

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			// Background：即使上层已取消也要把写锁放掉。
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var failures int
	var next int64
	err = conn.QueryRowContext(ctx,
		`SELECT retry_failures, next_probe_at FROM registry_host_recovery
		 WHERE source_id=? AND family='registry' AND scope=?`,
		r.SourceID, r.Host).Scan(&failures, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if next > r.At {
		return false, nil
	}

	// 读、算、写用同一个 n：UPDATE 写回的就是它，old=7 时仍是 7。
	n := failures + 1
	if n > hostProbeRetryMaxFailures {
		n = hostProbeRetryMaxFailures
	}
	nextAt := r.At + int64(HostProbeRetryDelay(n)/time.Second)

	res, err := conn.ExecContext(ctx,
		`UPDATE registry_host_recovery
		    SET next_probe_at=?, retry_failures=?, updated_at=?
		  WHERE source_id=? AND family='registry' AND scope=?
		    AND gen=?
		    AND next_probe_at<=?
		    AND state<>'hard-excluded'
		    AND EXISTS (SELECT 1 FROM registry_host_gen g
		                 WHERE g.host=? AND g.gen=?)
		    AND EXISTS (SELECT 1 FROM sources src
		                 WHERE src.id=? AND src.enabled=1
		                   AND src.status<>'archived' AND src.repo_host=?)`,
		nextAt, n, r.At,
		r.SourceID, r.Host,
		r.Gen,
		r.At,
		r.Host, r.Gen,
		r.SourceID, r.Host)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

// HostProbeRetryState 读取一行的退避状态，供诊断与测试使用。
func (s *Store) HostProbeRetryState(sourceID int64, host string) (failures int, next int64, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT retry_failures, next_probe_at FROM registry_host_recovery
		 WHERE source_id=? AND family='registry' AND scope=?`,
		sourceID, host).Scan(&failures, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return failures, next, true, nil
}
