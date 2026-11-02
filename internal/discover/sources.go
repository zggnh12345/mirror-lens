package discover

// feeds 是内置发现源。
//
// 两类互补：列表仓库条目多但更新慢；状态页是别人正在测的实时结果，
// 只在线的才收。抓到的都只是候选，L1 通过后才算数。
var feeds = []feed{
	// 状态页放前面：同一个域名两处都有时，优先用带在线标记的来源。
	{
		name:   "1Panel 状态页",
		url:    "https://status.1panel.top/",
		format: fmtStatusHTML,
		kind:   KindDocker,
		group:  "dockerhub",
	},
	{
		name:   "AnyE 状态页",
		url:    "https://status.anye.xyz/status/hub",
		format: fmtStatusJSON,
		kind:   KindDocker,
		header: map[string]string{"Accept": "application/json"},
	},

	// Docker Hub 镜像清单
	{
		name:   "dongyubin/DockerHub",
		url:    "https://raw.githubusercontent.com/dongyubin/DockerHub/main/README.md",
		format: fmtMarkdown,
		kind:   KindDocker,
	},
	{
		name:   "DaoCloud/public-image-mirror",
		url:    "https://raw.githubusercontent.com/DaoCloud/public-image-mirror/main/README.md",
		format: fmtMarkdown,
		kind:   KindDocker,
	},
	{
		name:   "kubesre/docker-registry-mirrors",
		url:    "https://raw.githubusercontent.com/kubesre/docker-registry-mirrors/main/README.md",
		format: fmtMarkdown,
		kind:   KindDocker,
	},
	{
		name:   "eryajf/Thanks-Mirror",
		url:    "https://raw.githubusercontent.com/eryajf/Thanks-Mirror/main/README.md",
		format: fmtMarkdown,
		kind:   KindDocker,
	},

	// GitHub 代理清单
	{
		name:   "hunshcn/gh-proxy",
		url:    "https://raw.githubusercontent.com/hunshcn/gh-proxy/master/README.md",
		format: fmtMarkdown,
		kind:   KindGitHub,
	},
}
