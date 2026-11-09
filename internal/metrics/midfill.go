package metrics

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// midfill（中途并行）的完整性指标。
//
// 口径：
//   - started：已经向客户端写出第一个字节，从此无法撤回；
//   - verify_failed：整份 SHA256 不符；
//   - client_abort_after_midfill：接入后因上游/校验失败而异常终止响应，
//     不包含客户端主动断开和客户端写错误。
var (
	midfillStart atomic.Uint64
	midfillVF    atomic.Uint64
	midfillAbort atomic.Uint64
	parFillLease atomic.Uint64
)

func MidfillStarted()      { midfillStart.Add(1) }
func MidfillVerifyFailed() { midfillVF.Add(1) }
func MidfillAborted()      { midfillAbort.Add(1) }

// ParFillLeaseTimeout 记一次「分片拿不到名额」：属于换源重试，不是段失败。
func ParFillLeaseTimeout() { parFillLease.Add(1) }

func (r *Registry) writeMidfill(b *strings.Builder) {
	b.WriteString("# HELP " + prefix + "midfill_started_total 中途并行已向客户端写出数据的次数\n")
	b.WriteString("# TYPE " + prefix + "midfill_started_total counter\n")
	fmt.Fprintf(b, "%smidfill_started_total %d\n", prefix, midfillStart.Load())
	b.WriteString("# HELP " + prefix + "midfill_verify_failed_total 中途并行整份摘要校验失败次数\n")
	b.WriteString("# TYPE " + prefix + "midfill_verify_failed_total counter\n")
	fmt.Fprintf(b, "%smidfill_verify_failed_total %d\n", prefix, midfillVF.Load())
	b.WriteString("# HELP " + prefix + "client_abort_after_midfill_total 接管后因上游或校验失败而终止响应的次数\n")
	b.WriteString("# TYPE " + prefix + "client_abort_after_midfill_total counter\n")
	fmt.Fprintf(b, "%sclient_abort_after_midfill_total %d\n", prefix, midfillAbort.Load())
	b.WriteString("# HELP " + prefix + "par_fill_lease_timeout_total 分片拿不到并发名额而换源的次数\n")
	b.WriteString("# TYPE " + prefix + "par_fill_lease_timeout_total counter\n")
	fmt.Fprintf(b, "%spar_fill_lease_timeout_total %d\n", prefix, parFillLease.Load())
}
