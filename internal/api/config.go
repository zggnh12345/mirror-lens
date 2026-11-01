package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/config"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"gopkg.in/yaml.v3"
)

// cfgField 描述一个可编辑的配置项。get/set 直接读写 *config.Config，
// 避免反射踩坑；展示与校验元数据手写，保证标签、范围、单位都准确。
type cfgField struct {
	Section string   // 分组标题
	Key     string   // 点号路径，对应 yaml 键
	Label   string   // 中文标签
	Kind    string   // int | float | bool | duration | string | list | enum
	Unit    string   // 单位显示
	Options []string // enum 的可选项
	Min     float64
	Max     float64
	HasMin  bool
	HasMax  bool
	Hint    string
	Restart bool // true = 改完需重启服务才生效
	Get     func(*config.Config) string
	Set     func(*config.Config, string) error
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }
func i32(v int) string   { return strconv.Itoa(v) }

func durStr(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}

func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("时间格式不对，写 30s / 5m / 300 这样")
}

// cfgSchema 是设置页「配置参数」一节的完整字段表，按分组排列。
// 除日志级别外全部标记 Restart=true：绝大多数参数在启动时已拷贝进各子系统，
// 热改内存里的 cfg 不会生效，必须重启；日志级别靠 logging.Setup 即时重建全局 logger。
var cfgSchema = []cfgField{
	// 基础
	{Section: "基础", Key: "addr", Label: "监听地址", Kind: "string", Restart: true,
		Hint: "docker 加速也走这个端口（/v2/），别只绑 127.0.0.1", Get: func(c *config.Config) string { return c.Addr }, Set: func(c *config.Config, v string) error { c.Addr = strings.TrimSpace(v); return nil }},
	{Section: "基础", Key: "data", Label: "数据目录", Kind: "string", Restart: true,
		Hint: "数据库 + 缓存所在目录", Get: func(c *config.Config) string { return c.Data }, Set: func(c *config.Config, v string) error { c.Data = strings.TrimSpace(v); return nil }},
	{Section: "基础", Key: "no_seed", Label: "不写入内置候选池", Kind: "bool", Restart: true,
		Hint: "调试用，正常保持关闭", Get: func(c *config.Config) string { return b2s(c.NoSeed) }, Set: func(c *config.Config, v string) error { c.NoSeed = parseBool(v); return nil }},

	// 缓存
	{Section: "缓存", Key: "cache.dir", Label: "缓存目录", Kind: "string", Restart: true,
		Hint: "留空 = <data>/cache", Get: func(c *config.Config) string { return c.Cache.Dir }, Set: func(c *config.Config, v string) error { c.Cache.Dir = strings.TrimSpace(v); return nil }},
	{Section: "缓存", Key: "cache.max_file_mb", Label: "单文件上限", Kind: "int", Unit: "MB", Min: 0, HasMin: true, Restart: true,
		Hint: "超过只透传、不落盘", Get: func(c *config.Config) string { return i64(c.Cache.MaxFileMB) }, Set: func(c *config.Config, v string) error { return setInt(&c.Cache.MaxFileMB, v, 0, 1<<20, true, false) }},
	{Section: "缓存", Key: "cache.max_gb", Label: "缓存总量上限", Kind: "int", Unit: "GB", Min: 0, HasMin: true, Restart: true,
		Hint: "0 = 不限，超了按最久未访问淘汰", Get: func(c *config.Config) string { return i64(c.Cache.MaxGB) }, Set: func(c *config.Config, v string) error { return setInt(&c.Cache.MaxGB, v, 0, 1<<20, true, false) }},
	{Section: "缓存", Key: "cache.warn_at", Label: "用量告警阈值", Kind: "float", Min: 0, Max: 1, HasMin: true, HasMax: true, Restart: true,
		Hint: "0~1 的比例，到这个值报一次警", Get: func(c *config.Config) string { return f2s(c.Cache.WarnAt) }, Set: func(c *config.Config, v string) error { return setFloat(&c.Cache.WarnAt, v, 0, 1, true, true) }},
	{Section: "缓存", Key: "cache.recover_at", Label: "告警复位阈值", Kind: "float", Min: 0, Max: 1, HasMin: true, HasMax: true, Restart: true,
		Hint: "必须 < 告警阈值，否则永不复位", Get: func(c *config.Config) string { return f2s(c.Cache.RecoverAt) }, Set: func(c *config.Config, v string) error { return setFloat(&c.Cache.RecoverAt, v, 0, 1, true, true) }},

	// 后台巡检
	{Section: "后台巡检", Key: "janitor.interval", Label: "巡检间隔", Kind: "duration", Restart: true,
		Hint: "容量、过期、半截文件、索引对账", Get: func(c *config.Config) string { return durStr(c.Janitor.Interval.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Janitor.Interval.Duration, v) }},
	{Section: "后台巡检", Key: "janitor.partial_ttl", Label: "半截文件保留", Kind: "duration", Restart: true,
		Hint: "断了没续传的临时文件留多久", Get: func(c *config.Config) string { return durStr(c.Janitor.PartialTTL.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Janitor.PartialTTL.Duration, v) }},
	{Section: "后台巡检", Key: "janitor.expired_grace", Label: "过期宽限", Kind: "duration", Restart: true,
		Hint: "条目过期后还要等多久没人访问才删", Get: func(c *config.Config) string { return durStr(c.Janitor.ExpiredGrace.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Janitor.ExpiredGrace.Duration, v) }},

	// 内容 TTL
	{Section: "内容 TTL", Key: "raw.ttl", Label: "Raw 重新验证间隔", Kind: "duration", Restart: true,
		Hint: "分支/标签；commit 永久缓存", Get: func(c *config.Config) string { return durStr(c.Raw.TTL.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Raw.TTL.Duration, v) }},
	{Section: "内容 TTL", Key: "git.ttl", Label: "Git info/refs 缓存", Kind: "duration", Restart: true,
		Hint: "0 = 每次回源", Get: func(c *config.Config) string { return durStr(c.Git.TTL.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Git.TTL.Duration, v) }},

	// Registry
	{Section: "Registry 加速", Key: "registry.enabled", Label: "开启 Docker 加速", Kind: "bool", Restart: true,
		Get: func(c *config.Config) string { return b2s(c.Registry.Enabled) }, Set: func(c *config.Config, v string) error { c.Registry.Enabled = parseBool(v); return nil }},
	{Section: "Registry 加速", Key: "registry.upstream", Label: "上游主机", Kind: "string", Restart: true,
		Hint: "只写主机名，不带 https:// 和路径", Get: func(c *config.Config) string { return c.Registry.Upstream }, Set: func(c *config.Config, v string) error { c.Registry.Upstream = strings.TrimSpace(v); return nil }},
	{Section: "Registry 加速", Key: "registry.max_file_mb", Label: "layer 单文件上限", Kind: "int", Unit: "MB", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i64(c.Registry.MaxFileMB) }, Set: func(c *config.Config, v string) error { return setInt(&c.Registry.MaxFileMB, v, 0, 1<<20, true, false) }},
	{Section: "Registry 加速", Key: "registry.tag_ttl", Label: "tag 重新验证", Kind: "duration", Restart: true,
		Hint: "tag 可能被覆盖，过一会儿要复验", Get: func(c *config.Config) string { return durStr(c.Registry.TagTTL.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Registry.TagTTL.Duration, v) }},

	// 多源调度
	{Section: "多源调度", Key: "sources.enabled", Label: "开启多源调度", Kind: "bool", Restart: true,
		Hint: "只影响回源；缓存命中不碰网络", Get: func(c *config.Config) string { return b2s(c.Sources.Enabled) }, Set: func(c *config.Config, v string) error { c.Sources.Enabled = parseBool(v); return nil }},
	{Section: "多源调度", Key: "sources.max_candidates", Label: "最多试几个源", Kind: "int", Min: 1, Max: 10, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Sources.MaxCandidates) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Sources.MaxCandidates, v, 1, 10, true, true) }},
	{Section: "多源调度", Key: "sources.sticky_ttl", Label: "成功源复用", Kind: "duration", Restart: true,
		Get: func(c *config.Config) string { return durStr(c.Sources.StickyTTL.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Sources.StickyTTL.Duration, v) }},
	{Section: "多源调度", Key: "sources.fail_cooldown", Label: "失败冷却", Kind: "duration", Restart: true,
		Hint: "期间该源降权；遇 429+Retry-After 取较长者", Get: func(c *config.Config) string { return durStr(c.Sources.FailCooldown.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Sources.FailCooldown.Duration, v) }},
	{Section: "多源调度", Key: "sources.max_per_source", Label: "单源并发上限", Kind: "int", Min: 0, Max: 64, HasMin: true, HasMax: true, Restart: true,
		Hint: "0 取默认 4，负数不限", Get: func(c *config.Config) string { return i32(c.Sources.MaxPerSource) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Sources.MaxPerSource, v, 0, 64, true, true) }},
	{Section: "多源调度", Key: "sources.dynamic", Label: "动态带宽调度", Kind: "bool", Restart: true,
		Hint: "按实测吞吐自动调并发（AIMD）", Get: func(c *config.Config) string { return b2s(c.Sources.Dynamic) }, Set: func(c *config.Config, v string) error { c.Sources.Dynamic = parseBool(v); return nil }},
	{Section: "多源调度", Key: "sources.archive_days", Label: "死源归档天数", Kind: "int", Restart: true,
		Get: func(c *config.Config) string { return i32(c.Sources.ArchiveDays) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Sources.ArchiveDays, v, 0, 1<<20, false, false) }},
	{Section: "多源调度", Key: "sources.cleanup_days", Label: "归档后删除天数", Kind: "int", Restart: true,
		Hint: "0/负 = 永不删", Get: func(c *config.Config) string { return i32(c.Sources.CleanupDays) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Sources.CleanupDays, v, 0, 1<<20, false, false) }},
	{Section: "多源调度", Key: "sources.recovery_minutes", Label: "死源恢复探测", Kind: "int", Unit: "分钟", Restart: true,
		Get: func(c *config.Config) string { return i32(c.Sources.RecoveryMinutes) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Sources.RecoveryMinutes, v, 0, 1<<20, false, false) }},

	// 分片并行
	{Section: "分片并行", Key: "parallel.enabled", Label: "开启分片并行", Kind: "bool", Restart: true,
		Hint: "只对带可信整份摘要的请求生效", Get: func(c *config.Config) string { return b2s(c.Parallel.Enabled) }, Set: func(c *config.Config, v string) error { c.Parallel.Enabled = parseBool(v); return nil }},
	{Section: "分片并行", Key: "parallel.min_size_mb", Label: "最小分片尺寸", Kind: "int", Unit: "MB", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i64(c.Parallel.MinSizeMB) }, Set: func(c *config.Config, v string) error { return setInt(&c.Parallel.MinSizeMB, v, 0, 1<<20, true, false) }},
	{Section: "分片并行", Key: "parallel.chunk_mb", Label: "单片大小", Kind: "int", Unit: "MB", Min: 1, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i64(c.Parallel.ChunkMB) }, Set: func(c *config.Config, v string) error { return setInt(&c.Parallel.ChunkMB, v, 1, 1<<20, true, false) }},
	{Section: "分片并行", Key: "parallel.workers", Label: "同时下载片数", Kind: "int", Min: 2, Max: 16, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Parallel.Workers) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Parallel.Workers, v, 2, 16, true, true) }},

	// 探测
	{Section: "探测", Key: "probe.allow_private", Label: "允许探测内网源", Kind: "bool", Restart: true,
		Hint: "源架在局域网时才开；开启等于关掉 SSRF 防护", Get: func(c *config.Config) string { return b2s(c.Probe.AllowPrivate) }, Set: func(c *config.Config, v string) error { c.Probe.AllowPrivate = parseBool(v); return nil }},

	// 访问控制
	{Section: "访问控制", Key: "auth.require_token", Label: "代理路径需 Token", Kind: "bool", Restart: true,
		Hint: "默认匿名放行；true 时非浏览器客户端必须带 Access Token", Get: func(c *config.Config) string { return b2s(c.Auth.RequireToken) }, Set: func(c *config.Config, v string) error { c.Auth.RequireToken = parseBool(v); return nil }},

	// 入口限流
	{Section: "入口限流", Key: "limits.enabled", Label: "开启限流", Kind: "bool", Restart: true,
		Get: func(c *config.Config) string { return b2s(c.Limits.Enabled) }, Set: func(c *config.Config, v string) error { c.Limits.Enabled = parseBool(v); return nil }},
	{Section: "入口限流", Key: "limits.global_rps", Label: "全局每秒请求", Kind: "float", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return f2s(c.Limits.GlobalRPS) }, Set: func(c *config.Config, v string) error { return setFloat(&c.Limits.GlobalRPS, v, 0, 1e9, true, false) }},
	{Section: "入口限流", Key: "limits.global_burst", Label: "全局突发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.GlobalBurst) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.GlobalBurst, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.global_concurrency", Label: "全局并发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.GlobalConcurrency) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.GlobalConcurrency, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.ip_per_minute", Label: "单 IP 每分钟", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.IPPerMinute) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.IPPerMinute, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.ip_burst", Label: "单 IP 突发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.IPBurst) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.IPBurst, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.ip_concurrency", Label: "单 IP 并发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.IPConcurrency) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.IPConcurrency, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.token_per_minute", Label: "单 Token 每分钟", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.TokenPerMinute) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.TokenPerMinute, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.token_burst", Label: "单 Token 突发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.TokenBurst) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.TokenBurst, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.token_concurrency", Label: "单 Token 并发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.TokenConcurrency) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.TokenConcurrency, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.trusted_proxies", Label: "可信代理网段", Kind: "list", Restart: true,
		Hint: "CIDR，每行一个；部署在反代后填反代网段", Get: func(c *config.Config) string { return strings.Join(c.Limits.TrustedProxies, "\n") }, Set: func(c *config.Config, v string) error {
			var out []string
			for _, line := range strings.Split(v, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					out = append(out, line)
				}
			}
			c.Limits.TrustedProxies = out
			return nil
		}},

	// 日志
	{Section: "日志", Key: "logging.level", Label: "日志级别", Kind: "enum", Options: []string{"debug", "info", "warn", "error"}, Restart: false,
		Hint: "改完即时生效", Get: func(c *config.Config) string { return c.Logging.Level }, Set: func(c *config.Config, v string) error {
			v = strings.ToLower(strings.TrimSpace(v))
			switch v {
			case "debug", "info", "warn", "error":
				c.Logging.Level = v
				return nil
			}
			return fmt.Errorf("日志级别只支持 debug/info/warn/error")
		}},
	{Section: "日志", Key: "logging.format", Label: "日志格式", Kind: "enum", Options: []string{"text", "json"}, Restart: true,
		Hint: "json 接 Loki/ELK", Get: func(c *config.Config) string { return c.Logging.Format }, Set: func(c *config.Config, v string) error {
			v = strings.ToLower(strings.TrimSpace(v))
			if v != "text" && v != "json" {
				return fmt.Errorf("日志格式只支持 text/json")
			}
			c.Logging.Format = v
			return nil
		}},

	// 指标
	{Section: "指标", Key: "metrics.enabled", Label: "暴露 /metrics", Kind: "bool", Restart: true,
		Get: func(c *config.Config) string { return b2s(c.Metrics.Enabled) }, Set: func(c *config.Config, v string) error { c.Metrics.Enabled = parseBool(v); return nil }},
	{Section: "指标", Key: "metrics.require_token", Label: "/metrics 需认证", Kind: "bool", Restart: true,
		Hint: "公网部署建议打开", Get: func(c *config.Config) string { return b2s(c.Metrics.RequireToken) }, Set: func(c *config.Config, v string) error { c.Metrics.RequireToken = parseBool(v); return nil }},

	// CDN
	{Section: "CDN 边缘", Key: "cdn.enabled", Label: "开启边缘缓存语义", Kind: "bool", Restart: true,
		Get: func(c *config.Config) string { return b2s(c.CDN.Enabled) }, Set: func(c *config.Config, v string) error { c.CDN.Enabled = parseBool(v); return nil }},
	{Section: "CDN 边缘", Key: "cdn.max_age", Label: "边缘 TTL 上限", Kind: "int", Unit: "秒", Min: 0, HasMin: true, Restart: true,
		Hint: "0 = 沿用条目 TTL", Get: func(c *config.Config) string { return i32(c.CDN.MaxAge) }, Set: func(c *config.Config, v string) error { return setIntI(&c.CDN.MaxAge, v, 0, 1<<30, true, false) }},
}

func b2s(b bool) string { return strconv.FormatBool(b) }
func parseBool(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "true" || s == "1" || s == "yes" || s == "on"
}
func f2s(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func setInt(p *int64, v string, min, max int64, hasMin, hasMax bool) error {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return fmt.Errorf("必须是整数：%q", v)
	}
	if hasMin && n < min {
		return fmt.Errorf("不能小于 %d", min)
	}
	if hasMax && n > max {
		return fmt.Errorf("不能大于 %d", max)
	}
	*p = n
	return nil
}
func setIntI(p *int, v string, min, max int, hasMin, hasMax bool) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("必须是整数：%q", v)
	}
	if hasMin && n < min {
		return fmt.Errorf("不能小于 %d", min)
	}
	if hasMax && n > max {
		return fmt.Errorf("不能大于 %d", max)
	}
	*p = n
	return nil
}
func setFloat(p *float64, v string, min, max float64, hasMin, hasMax bool) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return fmt.Errorf("必须是数字：%q", v)
	}
	if hasMin && f < min {
		return fmt.Errorf("不能小于 %v", min)
	}
	if hasMax && f > max {
		return fmt.Errorf("不能大于 %v", max)
	}
	*p = f
	return nil
}
func setDur(p *time.Duration, v string) error {
	d, err := parseDur(v)
	if err != nil {
		return err
	}
	if d < 0 {
		return fmt.Errorf("时间不能是负数")
	}
	*p = d
	return nil
}

// configView 把当前 cfg 按分组整理成前端要的结构。
func configView(c *config.Config, path string) map[string]any {
	groups := map[string][]map[string]any{}
	order := []string{}
	for _, f := range cfgSchema {
		if _, ok := groups[f.Section]; !ok {
			groups[f.Section] = nil
			order = append(order, f.Section)
		}
		groups[f.Section] = append(groups[f.Section], map[string]any{
			"key":     f.Key,
			"label":   f.Label,
			"kind":    f.Kind,
			"value":   f.Get(c),
			"unit":    f.Unit,
			"options": f.Options,
			"min":     f.Min,
			"max":     f.Max,
			"hasMin":  f.HasMin,
			"hasMax":  f.HasMax,
			"hint":    f.Hint,
			"restart": f.Restart,
		})
	}
	secs := make([]map[string]any, 0, len(order))
	for _, name := range order {
		secs = append(secs, map[string]any{"title": name, "fields": groups[name]})
	}
	return map[string]any{
		"sections":    secs,
		"config_path": path,
		"restart_note": "除「日志级别」即时生效外，其余参数在启动时已拷贝进各子系统，修改后需重启服务才生效；修改会写入 config.yaml（路径见上）。若配置文件位于镜像内而非持久卷，重启容器会丢失，请确认 -config 指向挂载卷。",
	}
}

// applyConfigValues 把提交的值套用到 *config.Config 的副本上，做范围与格式校验，
// 返回出错字段。不改文件、不动原 cfg。
func applyConfigValues(c *config.Config, values map[string]string) (changed []string, errField string, err error) {
	for _, f := range cfgSchema {
		raw, ok := values[f.Key]
		if !ok {
			continue
		}
		if err := f.Set(c, raw); err != nil {
			return changed, f.Key, fmt.Errorf("%s: %s", f.Label, err.Error())
		}
		changed = append(changed, f.Key)
	}
	if err := c.Validate(); err != nil {
		return changed, "", fmt.Errorf("校验失败：%s", err.Error())
	}
	return changed, "", nil
}

// writeConfigFile 把 values 写回 YAML 文件。节点级更新：保留结构/注释，
// 只改被提交的那几个键；文件不存在则整份写出（会丢失原注释）。
func writeConfigFile(path string, values map[string]string) error {
	var root yaml.Node
	exists := false
	if b, err := os.ReadFile(path); err == nil {
		exists = true
		if err := yaml.Unmarshal(b, &root); err != nil {
			return fmt.Errorf("读取现有配置失败：%w", err)
		}
	}
	if !exists || root.Kind == 0 {
		// 没有文件或解析不出结构：用默认 cfg 整份写出。
		def := config.Default()
		// 仅把 values 应用到默认副本，避免把内存里运行态（含 secrets 无关）全量落盘差异过大。
		if _, _, err := applyConfigValues(def, values); err != nil {
			return err
		}
		out, err := yaml.Marshal(def)
		if err != nil {
			return err
		}
		return os.WriteFile(path, out, 0o644)
	}
	for k, v := range values {
		segs := strings.Split(k, ".")
		f := findField(k)
		if f == nil {
			continue
		}
		if err := setYAMLNode(&root, segs, v, f.Kind); err != nil {
			return fmt.Errorf("写 %s 失败：%w", k, err)
		}
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func findField(key string) *cfgField {
	for i := range cfgSchema {
		if cfgSchema[i].Key == key {
			return &cfgSchema[i]
		}
	}
	return nil
}

// setYAMLNode 在 yaml 文档树里定位 path 并把标量设为 value。
// 缺失的 mapping 键会自动创建；list 类型（trusted_proxies）整段重建。
func setYAMLNode(root *yaml.Node, path []string, value, kind string) error {
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	cur := root
	for i, seg := range path {
		if i == len(path)-1 {
			// 叶子
			if kind == "list" {
				return setSeqLeaf(cur, seg, value)
			}
			return setScalarLeaf(cur, seg, value, kind)
		}
		// 中间层：必须是 mapping
		child, err := ensureMapKey(cur, seg)
		if err != nil {
			return err
		}
		cur = child
	}
	return nil
}

func ensureMapKey(parent *yaml.Node, key string) (*yaml.Node, error) {
	if parent.Kind != yaml.MappingNode {
		// 原值是标量（被覆盖成对象），重建为 mapping
		parent.Kind = yaml.MappingNode
		parent.Tag = "!!map"
		parent.Content = nil
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			if parent.Content[i+1].Kind != yaml.MappingNode {
				parent.Content[i+1].Kind = yaml.MappingNode
				parent.Content[i+1].Tag = "!!map"
				parent.Content[i+1].Content = nil
			}
			return parent.Content[i+1], nil
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, k, v)
	return v, nil
}

func setScalarLeaf(parent *yaml.Node, key, value, kind string) error {
	if parent.Kind != yaml.MappingNode {
		parent.Kind = yaml.MappingNode
		parent.Tag = "!!map"
		parent.Content = nil
	}
	var tag string
	switch kind {
	case "int":
		tag = "!!int"
	case "float":
		tag = "!!float"
	case "bool":
		tag = "!!bool"
	default:
		tag = "!!str"
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1].Kind = yaml.ScalarNode
			parent.Content[i+1].Tag = tag
			parent.Content[i+1].Value = value
			return nil
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
	parent.Content = append(parent.Content, k, v)
	return nil
}

func setSeqLeaf(parent *yaml.Node, key, value string) error {
	if parent.Kind != yaml.MappingNode {
		parent.Kind = yaml.MappingNode
		parent.Tag = "!!map"
		parent.Content = nil
	}
	var items []*yaml.Node
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			items = append(items, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: line})
		}
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1] = seq
			return nil
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	parent.Content = append(parent.Content, k, seq)
	return nil
}

// config 处理 GET（读取）/ PUT（校验 + 写文件 + 热更日志）。
func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil {
		writeJSON(w, 500, map[string]string{"error": "服务端未加载配置文件"})
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, configView(s.cfg, s.cfgPath))
		return
	}
	if r.Method != http.MethodPut {
		writeJSON(w, 405, map[string]string{"error": "方法不支持"})
		return
	}
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	if len(body.Values) == 0 {
		writeJSON(w, 400, map[string]string{"error": "没有提交任何改动"})
		return
	}
	// 在副本上套用，校验通过才落盘，避免半截配置。
	work := *s.cfg
	changed, field, err := applyConfigValues(&work, body.Values)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error(), "field": field})
		return
	}
	if s.cfgPath == "" {
		writeJSON(w, 400, map[string]string{"error": "未指定配置文件路径，无法保存（用 -config 启动才会落盘）"})
		return
	}
	if err := writeConfigFile(s.cfgPath, body.Values); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	*s.cfg = work
	// 日志级别即时热更
	if _, ok := body.Values["logging.level"]; ok {
		if err := logging.Setup(s.cfg.Logging); err != nil {
			writeJSON(w, 200, map[string]any{"ok": true, "applied": changed, "warn": "配置已保存，但日志级别热更失败：" + err.Error()})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "applied": changed})
}

// WithConfig 让 Server 持有一个可编辑的 cfg 副本与配置文件路径。
func WithConfig(c *config.Config, path string) Option {
	return func(s *Server) {
		s.cfg = c
		s.cfgPath = path
	}
}

// decodeJSON 容忍空体。
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return fmt.Errorf("空请求体")
	}
	return json.NewDecoder(r.Body).Decode(v)
}
