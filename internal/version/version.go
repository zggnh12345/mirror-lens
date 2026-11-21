// Package version 是版本号的唯一来源。
//
// 之前 api 与 gateway 各写一份常量，改一处漏一处；面板显示的版本号和
// 出站 User-Agent 里的版本号必须始终一致，否则查问题时对不上。
package version

import "fmt"

// Version 是程序版本的唯一权威来源。
//
// 发布流程按 Tag 校验它（scripts/check-version.sh）；不要在 main.go、
// Dockerfile 或 workflow 里重复硬编码版本号。
const Version = "3.5.18"

// Commit 与 BuildDate 由构建时通过 ldflags 注入：
//
//	-X github.com/zggnh12345/mirror-lens/internal/version.Commit=<sha>
//	-X github.com/zggnh12345/mirror-lens/internal/version.BuildDate=<RFC3339>
//
// 未注入时保留 unknown，避免输出空串。二者不属于 Schema 版本。
var (
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Info 返回单行版本信息；-version 与启动日志共用，保证两处完全一致。
func Info() string {
	return fmt.Sprintf("mirror-lens %s commit=%s built=%s", Version, Commit, BuildDate)
}
