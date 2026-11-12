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

// RetryReject 是持久化调度的拒绝原因（A2-2 观测用，封闭集合）。
// 空值表示写入成功；分类只用于指标，不参与 CAS 判定。
type RetryReject string

const (
	RetryWritten                 RetryReject = ""
	RetryRejectGenMismatch       RetryReject = "gen_mismatch"
	RetryRejectCASLost           RetryReject = "cas_lost"
	RetryRejectSourceUnavailable RetryReject = "source_unavailable"
	RetryRejectDBError           RetryReject = "db_error"
)

// ScheduleHostProbeRetry 把一个未提交成功的执行失败持久化为下一次到期时间。
//
// 返回 false 表示被拒绝：代次不符、行已不在到期状态、源被禁用/归档/换 Host、
// 或行处于 hard-excluded。拒绝时不写任何字段，也不清除退避计数。
//
// 只有 next_probe_at 与 retry_failures 两个字段被修改；state、rec_failures、
// last_probe_at、last_success_at、last_error 全部不动 —— 这是「本地执行失败」
// 与「探测结果失败」的边界。
func (s *Store) ScheduleHostProbeRetry(r HostProbeRetry) (bool, error) {
	rej, _, err := s.ScheduleHostProbeRetryClassified(r)
	return rej == RetryWritten, err
}

// ScheduleHostProbeRetryClassified 与 ScheduleHostProbeRetry 行为完全一致，
// 额外返回拒绝原因与实际写入的退避间隔（A2-2 指标用）。分类 SELECT 与
// UPDATE 在同一 BEGIN IMMEDIATE 事务内，只读不改。
func (s *Store) ScheduleHostProbeRetryClassified(r HostProbeRetry) (RetryReject, time.Duration, error) {
	if r.SourceID == 0 || r.Host == "" || r.At <= 0 {
		return RetryRejectDBError, 0, fmt.Errorf("ScheduleHostProbeRetry: 参数不完整 source=%d host=%q at=%d", r.SourceID, r.Host, r.At)
	}

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RetryRejectDBError, 0, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return RetryRejectDBError, 0, err
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
		return RetryRejectSourceUnavailable, 0, nil
	}
	if err != nil {
		return RetryRejectDBError, 0, err
	}
	if next > r.At {
		return RetryRejectCASLost, 0, nil
	}

	// 读、算、写用同一个 n：UPDATE 写回的就是它，old=7 时仍是 7。
	n := failures + 1
	if n > hostProbeRetryMaxFailures {
		n = hostProbeRetryMaxFailures
	}
	delay := HostProbeRetryDelay(n)
	nextAt := r.At + int64(delay/time.Second)

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
		return RetryRejectDBError, 0, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return RetryRejectDBError, 0, err
	}
	if rows == 0 {
		return classifyRetryReject(ctx, conn, r), 0, nil
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RetryRejectDBError, 0, err
	}
	committed = true
	return RetryWritten, delay, nil
}

// classifyRetryReject 只在 RowsAffected==0 时执行一次只读分类。
// 判定顺序与 CAS 条件一一对应，保证指标原因与真实拒绝原因一致。
func classifyRetryReject(ctx context.Context, conn *sql.Conn, r HostProbeRetry) RetryReject {
	var rowGen, hostGen int64
	var enabled int
	var status, repoHost, state string
	err := conn.QueryRowContext(ctx,
		`SELECT rec.gen,
		        COALESCE((SELECT gen FROM registry_host_gen WHERE host=?), -1),
		        src.enabled, src.status, src.repo_host, rec.state
		   FROM registry_host_recovery rec
		   JOIN sources src ON src.id=rec.source_id
		  WHERE rec.source_id=? AND rec.family='registry' AND rec.scope=?`,
		r.Host, r.SourceID, r.Host).Scan(&rowGen, &hostGen, &enabled, &status, &repoHost, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return RetryRejectSourceUnavailable
	}
	if err != nil {
		return RetryRejectDBError
	}
	if rowGen != r.Gen || hostGen != r.Gen {
		return RetryRejectGenMismatch
	}
	if enabled != 1 || status == "archived" || repoHost != r.Host || state == "hard-excluded" {
		return RetryRejectSourceUnavailable
	}
	return RetryRejectCASLost
}

// CountRetryPending 统计处于退避中（未到期）的 Host 数，供 Gauge 使用。
func (s *Store) CountRetryPending(now int64) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("CountRetryPending: store 未初始化")
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM registry_host_recovery
		 WHERE family='registry' AND retry_failures>0 AND next_probe_at>?`, now).Scan(&n)
	return n, err
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
