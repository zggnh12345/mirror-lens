package config

import "gopkg.in/yaml.v3"

// fieldDocs 是 yaml 路径到说明的映射。模板和面板保存时用它给每个键补注释，
// 保证生成的 config.yaml 里没有"裸键"。新增字段必须同步加注释，
// TestEveryFieldHasDoc 会拦住漏项。
var fieldDocs = map[string]string{
	"cache":                         "缓存策略：容量、淘汰、磁盘保护",
	"janitor":                       "后台清理任务",
	"raw":                           "raw 文件缓存",
	"git":                           "git 智能协议元数据缓存",
	"registry":                      "Docker Registry 加速",
	"sources":                       "加速源调度：候选数量、粘滞、并发、生命周期",
	"parallel":                      "多源分片并行下载",
	"private":                       "私有仓库 / 信任源加速",
	"hedge":                         "对冲请求：首选源变慢时补发第二路",
	"probe":                         "源探测",
	"auth":                          "下载认证",
	"limits":                        "限流：全局、按 IP、按 Token",
	"metrics":                       "Prometheus 指标",
	"cdn":                           "CDN 缓存头",
	"logging":                       "日志",
	"addr":                          "监听地址，如 :8080 或 127.0.0.1:8080",
	"data":                          "数据目录：缓存、数据库、日志都在这里",
	"no_seed":                       "首次启动时不写入内置加速源",
	"cache.dir":                     "缓存目录，留空表示 <data>/cache",
	"cache.max_file_mb":             "单个文件缓存上限（MiB）",
	"cache.max_gb":                  "缓存总量上限（GiB），超出后按最近最少使用淘汰",
	"cache.min_free_gb":             "磁盘剩余低于此值时停止写入新缓存（GiB）",
	"cache.warn_at":                 "缓存占用达到该比例时告警，0.9 表示 90%",
	"cache.recover_at":              "占用回落到该比例以下时恢复缓存，需小于 warn_at",
	"janitor.interval":              "执行周期，建议 10m~1h",
	"janitor.partial_ttl":           "未完成的分片文件保留时长，超时删除",
	"janitor.expired_grace":         "过期缓存的宽限期，到期后才真正删除",
	"raw.ttl":                       "raw 原始文件缓存时长，0 表示不缓存",
	"git.ttl":                       "git 元数据（refs、info）缓存时长",
	"registry.enabled":              "Registry 加速总开关，关闭后 /v2/ 整体不可用",
	"registry.docker_enabled":       "Docker Hub 通道开关（裸镜像名走这条）",
	"registry.ghcr_enabled":         "GHCR 通道开关（ghcr.io/... 走这条）",
	"github":                        "GitHub 加速：/gh/ /raw/ /git/ 三条路径",
	"github.enabled":                "是否启用 GitHub 加速，关闭后三条路径都返回 404",
	"github.token_env":              "存放 GitHub Token 的环境变量名；未设置时完全不调用 Release API",
	"github.token_enc":              "设置页保存的 Token 密文（AES-256-GCM，密钥在 <data>/secret.key）；不要手改",
	"network":                       "回源网络行为",
	"network.ip_family":             "IP 协议族：auto（IPv4 优先、IPv6 兜底）/ v4 / v6",
	"registry.upstream":             "Registry 上游地址，默认 Docker Hub",
	"registry.max_file_mb":          "单个镜像层允许的最大体积（MiB）",
	"registry.tag_ttl":              "tag 到 digest 的解析结果缓存时长",
	"sources.enabled":               "是否启用多源调度（关闭后只用主源）",
	"sources.max_candidates":        "每次请求最多尝试的候选源数量",
	"sources.sticky_ttl":            "命中后同一客户端粘滞同一源的时长",
	"sources.fail_cooldown":         "源失败后的冷却时长，期间不再选用",
	"sources.max_per_source":        "单个源的最大并发请求数",
	"sources.dynamic":               "按吞吐自动增减单源并发（AIMD，3.0 默认关闭）",
	"sources.headroom":              "普通派发占硬上限的比例，其余名额留给换源接管（默认 0.8）",
	"sources.admission":             "名额不足时的策略：wait_then_fallback/blocking/non_blocking/observe",
	"sources.wait_timeout":          "wait_then_fallback 的最长等待，超时后组内换源（默认 2s）",
	"sources.preflight_concurrency": "预检全局在途上限（默认 4），满了跳过该源",
	"sources.relay_concurrency":     "信任中转按排名参与竞速的站数上限（默认 4）",
	"sources.git_concurrency":       "Git upload-pack 并发上限（默认 4）",
	"sources.archive_days":          "连续多少天无流量后归档该源",
	"sources.cleanup_days":          "归档后多少天彻底删除源记录",
	"sources.recovery_minutes":      "被熔断的源每隔多少分钟试恢复一次",
	"parallel.enabled":              "是否启用多源分片并行下载",
	"parallel.min_size_mb":          "文件大于该值才分片（MiB）",
	"parallel.range_ttl":            "「该源不支持 Range」结论的缓存时长，过期重测（默认 6h）",
	"parallel.chunk_mb":             "每个分片的请求大小（MiB）",
	"parallel.workers":              "单个文件的并行分片数",
	"parallel.degrade_mbps":         "星标源最近 3 次真实单流都低于该速度（MB/s）才改走并行",
	"parallel.degrade_hold_min":     "降级后并行持续多少分钟，再放一次单流重探",
	"parallel.watch_mbps":           "单流下载时每秒采样，窗口平均低于该值判为慢",
	"parallel.watch_sec":            "判速滑动窗口的秒数",
	"parallel.switch_min_mbps":      "备选星标要有这么快的历史样本才值得换源",
	"parallel.switch_min_file_mib":  "小于该尺寸的文件不参与换源",
	"parallel.switch_max_sent_mib":  "已发送超过该字节数就不再换源",
	"parallel.switch_max":           "单个请求最多换源几次",
	"parallel.switch_gap_sec":       "两次换源之间的最小间隔秒数",
	"probe.interval_sec":            "源探测轮询间隔秒数",
	"probe.concurrency":             "同时探测的源数量",
	"probe.timeout_sec":             "单个源探测的超时秒数",
	"probe.sample_mib":              "每轮探测读取的数据量（MB），越大判速越准",
	"parallel.disable_tail_verify":  "跳过尾部哈希校验，仅调试用，生产不要开",
	"private.break_after":           "信任源连续失败多少次后熔断",
	"private.break_seconds":         "熔断持续秒数，到期后放一个请求试探",
	"private.fallback_concurrency":  "回退到普通下载时的并发数",
	"hedge.enabled":                 "是否启用对冲请求（首选源慢时补发第二路）",
	"hedge.after_ms":                "首选源多少毫秒没有响应头就补发第二路",
	"hedge.grace_ms":                "第二路先到后，再给首选源多少毫秒的宽限",
	"relay":                         "信任中转的 IP 钉死与解析策略",
	"relay.pin_grace_hours":         "首次解析失败后的迁移窗口（小时），到期停止未钉 IP 的拨号",
	"relay.resolver_timeout":        "单次 DNS 解析超时，解析结果会被钉住",
	"probe.allow_private":           "允许探测内网地址，默认关闭以防 SSRF",
	"auth.require_token":            "下载接口是否强制校验 Access Token",
	"limits.enabled":                "是否启用全局限流",
	"limits.global_rps":             "全局每秒请求数上限",
	"limits.global_burst":           "全局突发请求数上限",
	"limits.global_concurrency":     "全局并发请求数上限",
	"limits.ip_per_minute":          "单个 IP 每分钟请求数上限",
	"limits.ip_burst":               "单个 IP 突发请求数上限",
	"limits.ip_concurrency":         "单个 IP 并发请求数上限",
	"limits.token_per_minute":       "单个 Token 每分钟请求数上限",
	"limits.token_burst":            "单个 Token 突发请求数上限",
	"limits.token_concurrency":      "单个 Token 并发请求数上限",
	"limits.trusted_proxies":        "信任的反向代理网段，只有这些来源的 X-Forwarded-For/Proto/Host 会被采信；默认 Docker 网桥+回环",
	"metrics.enabled":               "是否开启 /api/metrics",
	"metrics.require_token":         "访问 /api/metrics 是否需要 Token",
	"cdn.enabled":                   "是否为响应加上 CDN 缓存头",
	"cdn.max_age":                   "CDN 缓存秒数",
	"logging.level":                 "日志级别：debug/info/warn/error，留空为 info",
	"logging.format":                "日志格式：text/json，留空为 text",
}

// Docs 返回某个 yaml 路径的说明，没有则返回空串。
func Docs(path string) string { return fieldDocs[path] }

// Annotate 给节点里缺少注释的键补上字段说明。
// 已有注释保持原样，所以手写注释不会被覆盖。
func Annotate(n *yaml.Node) { annotate(n, "") }

func annotate(n *yaml.Node, prefix string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		annotate(n.Content[0], prefix)
		return
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		p := k.Value
		if prefix != "" {
			p = prefix + "." + k.Value
		}
		if k.HeadComment == "" && k.LineComment == "" {
			if d := fieldDocs[p]; d != "" {
				k.HeadComment = d
			}
		}
		annotate(v, p)
	}
}
