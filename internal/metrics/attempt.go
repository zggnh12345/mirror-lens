package metrics

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Attempt 生命周期计数（3.1.3 的 Attempt Context 路径）。
//
// 口径：
//   - 只统计绑定了 Lease 的上游尝试（chunkLease.bindAttempt 成功返回 true），
//     官方源 fallback、无调度器路径不在此列；
//   - started = finished 是设计不变量，active 长期不为 0 即为泄漏；
//   - cancelled 只统计由 Reclaim 发起的取消，正常收尾的 ctx.Cancel 不算。
var (
	attemptStarted  atomic.Uint64
	attemptFinished atomic.Uint64
	attemptActive   atomic.Int64
	attemptCancnt   atomic.Uint64
)

// AttemptStarted 记一次 Attempt 创建。
func AttemptStarted() {
	attemptStarted.Add(1)
	attemptActive.Add(1)
}

// AttemptFinished 记一次 Attempt 收尾（成功/失败/取消任一出出口）。
func AttemptFinished() {
	attemptFinished.Add(1)
	attemptActive.Add(-1)
}

// AttemptCancelled 记一次由 Reclaim 触发的取消。
func AttemptCancelled() { attemptCancnt.Add(1) }

// writeAttempt 输出 Attempt 指标。
func (r *Registry) writeAttempt(b *strings.Builder) {
	b.WriteString("# HELP " + prefix + "attempts_started_total 已创建的租约型上游 Attempt 数\n")
	b.WriteString("# TYPE " + prefix + "attempts_started_total counter\n")
	fmt.Fprintf(b, "%sattempts_started_total %d\n", prefix, attemptStarted.Load())
	b.WriteString("# HELP " + prefix + "attempts_finished_total 已收尾的租约型上游 Attempt 数\n")
	b.WriteString("# TYPE " + prefix + "attempts_finished_total counter\n")
	fmt.Fprintf(b, "%sattempts_finished_total %d\n", prefix, attemptFinished.Load())
	b.WriteString("# HELP " + prefix + "attempts_active 尚未收尾的租约型上游 Attempt 数\n")
	b.WriteString("# TYPE " + prefix + "attempts_active gauge\n")
	fmt.Fprintf(b, "%sattempts_active %d\n", prefix, nonNeg(attemptActive.Load()))
	b.WriteString("# HELP " + prefix + "attempts_cancelled_total 因 Reclaim 被取消的 Attempt 数\n")
	b.WriteString("# TYPE " + prefix + "attempts_cancelled_total counter\n")
	fmt.Fprintf(b, "%sattempts_cancelled_total %d\n", prefix, attemptCancnt.Load())
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
