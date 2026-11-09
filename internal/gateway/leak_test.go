package gateway

import (
	"testing"

	"go.uber.org/goleak"
)

// 只对 gateway 包启用泄漏检查：这里的 Body/Attempt 协程是本项目的生命周期核心。
// 忽略项都是标准库或测试服务器的常驻协程，不属于本项目资源。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("net/http.(*Server).Serve"),
		goleak.IgnoreTopFunction("net/http.(*conn).serve"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
	)
}
