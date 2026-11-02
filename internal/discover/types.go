package discover

// 加速类型，与 store.CapYes 对应。
const (
	KindDocker = "docker"
	KindGitHub = "github"
)

// 列表格式。
const (
	fmtMarkdown   = "markdown"    // README 等文本，靠正则抠域名
	fmtStatusHTML = "status-html" // 状态页 HTML，按分组读 data-url/data-status
	fmtStatusJSON = "status-json" // 状态页 JSON 接口
)

// feed 是一个发现源。
type feed struct {
	name   string            // 写进备注，方便面板回溯来源
	url    string            //
	format string            // 见上面的 fmt*
	kind   string            // 条目的默认类型
	group  string            // status-html 只取这一组
	header map[string]string // 额外请求头
}

// candidate 是从列表里解析出的一条候选。
type candidate struct {
	host   string
	kind   string
	origin string // 来自哪个发现源
	online bool   // 状态页报告在线
}
