package metrics

import (
	"sync/atomic"
	"time"
)

// C6 物化（ok → expired）的执行观测。三个计数器语义严格分离：
//   - runs：调用次数（含零行、含失败）
//   - rows：实际 CAS 成功并提交的行数（不是 SQL 候选数；回滚时记 0）
//   - failures：返回 error 的调用次数
//
// 因此「runs 增、rows 不动、failures 不动」表示正常无过期记录，
// 「failures 增」才是执行失败，两者不会混为一谈。
var (
	recoveryRuns     atomic.Uint64
	recoveryRows     atomic.Uint64
	recoveryFailures atomic.Uint64
	recoveryNanos    atomic.Uint64
)

// RecoveryCounters 是一次读取的快照。
type RecoveryCounters struct {
	MaterializedRows uint64
	Runs             uint64
	Failures         uint64
	DurationNanos    uint64
}

// RecoveryMaterialize 记录一次物化调用的结果。took 为整次调用耗时，
// 包含事务开销；失败时 rows 由调用方置 0（未提交的行不算物化成功）。
func RecoveryMaterialize(rows int, took time.Duration, err error) {
	recoveryRuns.Add(1)
	recoveryNanos.Add(uint64(took))
	if rows > 0 {
		recoveryRows.Add(uint64(rows))
	}
	if err != nil {
		recoveryFailures.Add(1)
	}
}

// RecoveryCountersSnapshot 读取累计值。
func RecoveryCountersSnapshot() RecoveryCounters {
	return RecoveryCounters{
		MaterializedRows: recoveryRows.Load(),
		Runs:             recoveryRuns.Load(),
		Failures:         recoveryFailures.Load(),
		DurationNanos:    recoveryNanos.Load(),
	}
}

// RecoveryViewSnapshot 供面板读取；平均耗时按整次调用（含事务开销）计算。
func RecoveryViewSnapshot() RecoveryView {
	c := RecoveryCountersSnapshot()
	avg := 0.0
	if c.Runs > 0 {
		avg = float64(c.DurationNanos) / float64(c.Runs) / 1e6
	}
	return RecoveryView{
		MaterializedRowsTotal: c.MaterializedRows,
		RunsTotal:             c.Runs,
		FailuresTotal:         c.Failures,
		AvgDurationMS:         avg,
	}
}
