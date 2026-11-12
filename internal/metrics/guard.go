package metrics

import "sync/atomic"

// guardFilterExhausted 是 Guard 过滤重试耗尽的累计次数。递增点在
// store.RecoveryGuard.FilterByGuard 的耗尽分支：它表示「一次候选查询因为
// 屏蔽集持续变化而保守返回空集」，属于 fail-closed 的健康信号，不是错误。
var guardFilterExhausted atomic.Uint64

// GuardFilterExhausted 记录一次过滤重试耗尽。
func GuardFilterExhausted() { guardFilterExhausted.Add(1) }

// GuardFilterExhaustedTotal 返回累计值。
func GuardFilterExhaustedTotal() uint64 { return guardFilterExhausted.Load() }
