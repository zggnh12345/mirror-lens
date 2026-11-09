package scheduler

import (
	"testing"

	"go.uber.org/goleak"
)

// 租约账本不允许残留每个 Lease 的取消协程；标准库常驻协程忽略。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
	)
}
