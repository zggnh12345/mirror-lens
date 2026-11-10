package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/config"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/secret"
	"gopkg.in/yaml.v3"
)

// cfgField 描述一个可编辑配置项。get/set 直接读写结构体，不用反射；
// 展示与校验元数据手写，保证标签、范围、单位准确。
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
	Restart bool // true = 需重启生效
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

// cfgSchema 是设置页「配置参数」的字段表，按分组排列。
// 除日志级别外都要重启：参数在启动时已拷进各子系统，热改 cfg 无效。
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

	// 数据库备份
	{Section: "数据库备份", Key: "backup.enabled", Label: "每日快照", Kind: "bool", Restart: true,
		Hint: "巡检时用 VACUUM INTO 出一份独立快照，随库复制 secret.key；防误删、迁移失败、索引损坏，不防磁盘故障",
		Get:  func(c *config.Config) string { return b2s(c.Backup.Enabled) }, Set: func(c *config.Config, v string) error { c.Backup.Enabled = parseBool(v); return nil }},
	{Section: "数据库备份", Key: "backup.keep", Label: "保留份数", Kind: "int", Unit: "份", Min: 1, Max: 365, HasMin: true, HasMax: true, Restart: true,
		Hint: "按日期滚动，超出删最旧；每天最多一份",
		Get:  func(c *config.Config) string { return i32(c.Backup.Keep) }, Set: func(c *config.Config, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 1 || n > 365 {
				return fmt.Errorf("保留份数应在 1~365 之间")
			}
			c.Backup.Keep = n
			return nil
		}},
	{Section: "数据库备份", Key: "backup.dir", Label: "快照目录", Kind: "string", Restart: true,
		Hint: "留空 = <数据目录>/backup",
		Get:  func(c *config.Config) string { return c.Backup.Dir }, Set: func(c *config.Config, v string) error { c.Backup.Dir = strings.TrimSpace(v); return nil }},

	// 内容 TTL
	{Section: "内容 TTL", Key: "raw.ttl", Label: "Raw 重新验证间隔", Kind: "duration", Restart: true,
		Hint: "分支/标签；commit 永久缓存", Get: func(c *config.Config) string { return durStr(c.Raw.TTL.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Raw.TTL.Duration, v) }},
	{Section: "内容 TTL", Key: "git.ttl", Label: "Git info/refs 缓存", Kind: "duration", Restart: true,
		Hint: "0 = 每次回源", Get: func(c *config.Config) string { return durStr(c.Git.TTL.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Git.TTL.Duration, v) }},

	// 网络
	{Section: "网络", Key: "network.ip_family", Label: "回源 IP 协议族", Kind: "enum", Options: []string{"auto", "v4", "v6"}, Restart: true,
		Hint: "auto = IPv4 优先、失败回退 IPv6；v4/v6 = 只用该族。信任中转已钉死 IP，不受影响",
		Get: func(c *config.Config) string {
			if c.Network.IPFamily == "" {
				return "auto"
			}
			return c.Network.IPFamily
		}, Set: func(c *config.Config, v string) error { c.Network.IPFamily = strings.TrimSpace(v); return nil }},

	// Registry
	{Section: "Registry 加速", Key: "registry.enabled", Label: "Registry 加速总开关", Kind: "bool", Restart: true,
		Hint: "关掉后 /v2/ 整体不可用，下面的分通道开关随之失效",
		Get:  func(c *config.Config) string { return b2s(c.Registry.Enabled) }, Set: func(c *config.Config, v string) error { c.Registry.Enabled = parseBool(v); return nil }},
	{Section: "Registry 加速", Key: "registry.docker_enabled", Label: "Docker Hub 通道", Kind: "bool", Restart: true,
		Hint: "裸镜像名（如 nginx、jc21/nginx-proxy-manager）走这条",
		Get:  func(c *config.Config) string { return b2s(c.Registry.Docker) }, Set: func(c *config.Config, v string) error { c.Registry.Docker = parseBool(v); return nil }},
	{Section: "Registry 加速", Key: "registry.ghcr_enabled", Label: "GHCR 通道", Kind: "bool", Restart: true,
		Hint: "ghcr.io/... 的镜像走这条；关掉后这类镜像 404",
		Get:  func(c *config.Config) string { return b2s(c.Registry.GHCR) }, Set: func(c *config.Config, v string) error { c.Registry.GHCR = parseBool(v); return nil }},
	{Section: "Registry 加速", Key: "registry.upstream", Label: "上游主机", Kind: "string", Restart: true,
		Hint: "只写主机名，不带 https:// 和路径", Get: func(c *config.Config) string { return c.Registry.Upstream }, Set: func(c *config.Config, v string) error { c.Registry.Upstream = strings.TrimSpace(v); return nil }},
	{Section: "Registry 加速", Key: "registry.max_file_mb", Label: "layer 单文件上限", Kind: "int", Unit: "MB", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i64(c.Registry.MaxFileMB) }, Set: func(c *config.Config, v string) error { return setInt(&c.Registry.MaxFileMB, v, 0, 1<<20, true, false) }},
	{Section: "Registry 加速", Key: "registry.tag_ttl", Label: "tag 重新验证", Kind: "duration", Restart: true,
		Hint: "tag 可能被覆盖，过一会儿要复验", Get: func(c *config.Config) string { return durStr(c.Registry.TagTTL.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Registry.TagTTL.Duration, v) }},

	// GitHub
	{Section: "GitHub 加速", Key: "github.enabled", Label: "开启 GitHub 加速", Kind: "bool", Restart: true,
		Hint: "统一控制 /gh/（release 资产）、/raw/（raw 文件）、/git/（git clone）；关掉后三条路都返回 404",
		Get:  func(c *config.Config) string { return b2s(c.GitHub.Enabled) }, Set: func(c *config.Config, v string) error { c.GitHub.Enabled = parseBool(v); return nil }},
	// 敏感字段：Get 永远返回空串，值只单向写入；是否已配置由 configView 的 set 标记。
	// 加密密钥跟数据目录走，换机器/换数据卷后旧密文解不开，需要重新填。
	{Section: "GitHub 加速", Key: "github.token", Label: "Release 摘要 API Token", Kind: "secret", Restart: true,
		Hint: "可选。留空=完全不调用 Release Asset API，不耗配额；填入后加密保存，不回显。环境变量 token_env 优先",
		Get:  func(c *config.Config) string { return "" },
		Set: func(c *config.Config, v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				c.GitHub.TokenEnc = ""
				return nil
			}
			if len(v) < 20 || strings.ContainsAny(v, " 	\r\n") {
				return fmt.Errorf("token 格式不对：长度至少 20，且不能含空白")
			}
			key, err := secret.LoadOrCreateKey(filepath.Join(c.Data, "secret.key"))
			if err != nil {
				return fmt.Errorf("读取加密密钥失败: %w", err)
			}
			enc, err := secret.Encrypt(key, v)
			if err != nil {
				return fmt.Errorf("加密 Token 失败: %w", err)
			}
			c.GitHub.TokenEnc = enc
			return nil
		}},

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
	{Section: "多源调度", Key: "sources.headroom", Label: "普通派发余量", Kind: "float", Min: 0, Max: 1, HasMin: true, HasMax: true, Restart: true,
		Hint: "普通请求最多占到硬上限的这个比例，其余留给换源接管", Get: func(c *config.Config) string { return f2s(c.Sources.Headroom) }, Set: func(c *config.Config, v string) error { return setFloat(&c.Sources.Headroom, v, 0, 1, false, false) }},
	{Section: "多源调度", Key: "sources.admission", Label: "名额不足时", Kind: "enum", Options: []string{"wait_then_fallback", "blocking", "non_blocking", "observe"}, Restart: true,
		Hint: "wait_then_fallback：最多等 wait_timeout 再换源（默认）", Get: func(c *config.Config) string {
			if c.Sources.Admission == "" {
				return "wait_then_fallback"
			}
			return c.Sources.Admission
		}, Set: func(c *config.Config, v string) error {
			switch v {
			case "observe", "blocking", "wait_then_fallback", "non_blocking":
				c.Sources.Admission = v
				return nil
			}
			return fmt.Errorf("admission 只支持 observe/blocking/wait_then_fallback/non_blocking")
		}},
	{Section: "多源调度", Key: "sources.wait_timeout", Label: "等名额上限", Kind: "duration", Restart: true,
		Hint: "只有 wait_then_fallback 用；超时就不等了，换下一台源", Get: func(c *config.Config) string { return durStr(c.Sources.WaitTimeout.Duration) }, Set: func(c *config.Config, v string) error { return setDur(&c.Sources.WaitTimeout.Duration, v) }},
	{Section: "多源调度", Key: "sources.preflight_concurrency", Label: "预检并发", Kind: "int", Min: 0, Max: 64, HasMin: true, HasMax: true, Restart: true,
		Hint: "全局在途预检数，0 取默认 4；满了就跳过该源", Get: func(c *config.Config) string { return i32(c.Sources.PreflightConcurrency) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Sources.PreflightConcurrency, v, 0, 64, true, true)
		}},
	{Section: "多源调度", Key: "sources.relay_concurrency", Label: "中转竞速上限", Kind: "int", Min: 0, Max: 64, HasMin: true, HasMax: true, Restart: true,
		Hint: "按排名截断参与竞速的站数，不排队", Get: func(c *config.Config) string { return i32(c.Sources.RelayConcurrency) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Sources.RelayConcurrency, v, 0, 64, true, true)
		}},
	{Section: "多源调度", Key: "sources.git_concurrency", Label: "Git 回源并发", Kind: "int", Min: 0, Max: 64, HasMin: true, HasMax: true, Restart: true,
		Hint: "upload-pack POST 前取名额，流结束才归还", Get: func(c *config.Config) string { return i32(c.Sources.GitConcurrency) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Sources.GitConcurrency, v, 0, 64, true, true)
		}},
	{Section: "多源调度", Key: "sources.archive_days", Label: "死源归档天数", Kind: "int", Restart: true,
		Get: func(c *config.Config) string { return i32(c.Sources.ArchiveDays) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Sources.ArchiveDays, v, 0, 1<<20, false, false)
		}},
	{Section: "多源调度", Key: "sources.cleanup_days", Label: "归档后删除天数", Kind: "int", Restart: true,
		Hint: "0/负 = 永不删", Get: func(c *config.Config) string { return i32(c.Sources.CleanupDays) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Sources.CleanupDays, v, 0, 1<<20, false, false)
		}},
	{Section: "多源调度", Key: "sources.recovery_minutes", Label: "死源恢复探测", Kind: "int", Unit: "分钟", Restart: true,
		Get: func(c *config.Config) string { return i32(c.Sources.RecoveryMinutes) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Sources.RecoveryMinutes, v, 0, 1<<20, false, false)
		}},

	// 分片并行
	{Section: "分片并行", Key: "parallel.enabled", Label: "开启分片并行", Kind: "bool", Restart: true,
		Hint: "只对带可信整份摘要的请求生效", Get: func(c *config.Config) string { return b2s(c.Parallel.Enabled) }, Set: func(c *config.Config, v string) error { c.Parallel.Enabled = parseBool(v); return nil }},
	{Section: "分片并行", Key: "parallel.min_size_mb", Label: "最小分片尺寸", Kind: "int", Unit: "MB", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i64(c.Parallel.MinSizeMB) }, Set: func(c *config.Config, v string) error { return setInt(&c.Parallel.MinSizeMB, v, 0, 1<<20, true, false) }},
	{Section: "分片并行", Key: "parallel.chunk_mb", Label: "单片大小", Kind: "int", Unit: "MB", Min: 1, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i64(c.Parallel.ChunkMB) }, Set: func(c *config.Config, v string) error { return setInt(&c.Parallel.ChunkMB, v, 1, 1<<20, true, false) }},
	{Section: "分片并行", Key: "parallel.workers", Label: "同时下载片数", Kind: "int", Min: 2, Max: 16, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Parallel.Workers) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Parallel.Workers, v, 2, 16, true, true) }},
	{Section: "分片并行", Key: "parallel.degrade_mbps", Label: "星标源降级阈值", Kind: "int", Unit: "MB/s", Min: 1, Max: 1024, HasMin: true, HasMax: true, Restart: true,
		Hint: "星标源最近 3 次真实单流都低于此值才改走并行", Get: func(c *config.Config) string { return i64(c.Parallel.DegradeMBps) }, Set: func(c *config.Config, v string) error { return setInt(&c.Parallel.DegradeMBps, v, 1, 1024, true, true) }},
	{Section: "分片并行", Key: "parallel.degrade_hold_min", Label: "降级后并行时长", Kind: "int", Unit: "分钟", Min: 1, Max: 1440, HasMin: true, HasMax: true, Restart: true,
		Hint: "并行这么久后放一次单流重新探测", Get: func(c *config.Config) string { return i32(c.Parallel.DegradeHoldMin) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Parallel.DegradeHoldMin, v, 1, 1440, true, true)
		}},

	{Section: "分片并行", Key: "parallel.watch_mbps", Label: "单流判慢阈值", Kind: "int", Unit: "MB/s", Min: 1, Max: 1024, HasMin: true, HasMax: true, Restart: true,
		Hint: "每秒采样，watch_sec 秒窗口平均低于此值判为慢", Get: func(c *config.Config) string { return i64(c.Parallel.WatchMBps) }, Set: func(c *config.Config, v string) error {
			return setInt(&c.Parallel.WatchMBps, v, 1, 1024, true, true)
		}},
	{Section: "分片并行", Key: "parallel.watch_sec", Label: "判速窗口", Kind: "int", Unit: "秒", Min: 5, Max: 120, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Parallel.WatchSec) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Parallel.WatchSec, v, 5, 120, true, true)
		}},
	{Section: "分片并行", Key: "parallel.switch_min_mbps", Label: "换源速度门槛", Kind: "int", Unit: "MB/s", Min: 1, Max: 1024, HasMin: true, HasMax: true, Restart: true,
		Hint: "备选星标要有这么快的历史样本才值得换，否则直接并行", Get: func(c *config.Config) string { return i64(c.Parallel.SwitchMinMBps) }, Set: func(c *config.Config, v string) error {
			return setInt(&c.Parallel.SwitchMinMBps, v, 1, 1024, true, true)
		}},
	{Section: "分片并行", Key: "parallel.switch_min_file_mib", Label: "换源最小文件", Kind: "int", Unit: "MB", Min: 1, Max: 102400, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i64(c.Parallel.SwitchMinFileMiB) }, Set: func(c *config.Config, v string) error {
			return setInt(&c.Parallel.SwitchMinFileMiB, v, 1, 102400, true, true)
		}},
	{Section: "分片并行", Key: "parallel.switch_max_sent_mib", Label: "换源已发上限", Kind: "int", Unit: "MB", Min: 1, Max: 102400, HasMin: true, HasMax: true, Restart: true,
		Hint: "已发给客户端的字节超过此值就不再换源", Get: func(c *config.Config) string { return i64(c.Parallel.SwitchMaxSentMiB) }, Set: func(c *config.Config, v string) error {
			return setInt(&c.Parallel.SwitchMaxSentMiB, v, 1, 102400, true, true)
		}},
	{Section: "分片并行", Key: "parallel.switch_max", Label: "最多换源次数", Kind: "int", Min: 0, Max: 10, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Parallel.SwitchMax) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Parallel.SwitchMax, v, 0, 10, true, true)
		}},
	{Section: "分片并行", Key: "parallel.switch_gap_sec", Label: "换源最小间隔", Kind: "int", Unit: "秒", Min: 0, Max: 600, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Parallel.SwitchGapSec) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Parallel.SwitchGapSec, v, 0, 600, true, true)
		}},

	// 信任中转：IP 钉死与解析策略
	{Section: "信任中转", Key: "relay.pin_grace_hours", Label: "IP 钉死迁移窗口", Kind: "int", Unit: "小时", Min: 0, Max: 720, HasMin: true, HasMax: true, Restart: true,
		Hint: "首次解析失败时沿用旧版解析的时限；到期仍未解析成功就停止未钉 IP 的拨号。0 = 24 小时",
		Get:  func(c *config.Config) string { return i32(c.Relay.PinGraceHours) },
		Set: func(c *config.Config, v string) error {
			return setIntI(&c.Relay.PinGraceHours, v, 0, 720, true, true)
		}},
	{Section: "信任中转", Key: "relay.resolver_timeout", Label: "DNS 解析超时", Kind: "duration", Restart: true,
		Hint: "单次解析超时；解析结果会被钉住，后续拨号不再重新解析。0 = 5s",
		Get:  func(c *config.Config) string { return durStr(c.Relay.ResolverTimeout.Duration) },
		Set:  func(c *config.Config, v string) error { return setDur(&c.Relay.ResolverTimeout.Duration, v) }},

	// 探测
	{Section: "探测", Key: "probe.allow_private", Label: "允许探测内网源", Kind: "bool", Restart: true,
		Hint: "源架在局域网时才开；开启等于关掉 SSRF 防护", Get: func(c *config.Config) string { return b2s(c.Probe.AllowPrivate) }, Set: func(c *config.Config, v string) error { c.Probe.AllowPrivate = parseBool(v); return nil }},
	{Section: "探测", Key: "probe.interval_sec", Label: "探测轮询间隔", Kind: "int", Unit: "秒", Min: 5, Max: 86400, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Probe.IntervalSec) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Probe.IntervalSec, v, 5, 86400, true, true)
		}},
	{Section: "探测", Key: "probe.concurrency", Label: "探测并发", Kind: "int", Min: 1, Max: 64, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Probe.Concurrency) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Probe.Concurrency, v, 1, 64, true, true)
		}},
	{Section: "探测", Key: "probe.timeout_sec", Label: "单源探测超时", Kind: "int", Unit: "秒", Min: 5, Max: 600, HasMin: true, HasMax: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Probe.TimeoutSec) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Probe.TimeoutSec, v, 5, 600, true, true)
		}},
	{Section: "探测", Key: "probe.sample_mib", Label: "探测采样大小", Kind: "int", Unit: "MB", Min: 1, Max: 256, HasMin: true, HasMax: true, Restart: true,
		Hint: "每轮读多少数据算速度，越大越准，流量也越大", Get: func(c *config.Config) string { return i32(c.Probe.SampleMiB) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Probe.SampleMiB, v, 1, 256, true, true)
		}},

	// 访问控制
	{Section: "访问控制", Key: "auth.require_token", Label: "代理路径需 Token", Kind: "bool", Restart: true,
		Hint: "默认匿名放行；true 时非浏览器客户端必须带 Access Token", Get: func(c *config.Config) string { return b2s(c.Auth.RequireToken) }, Set: func(c *config.Config, v string) error { c.Auth.RequireToken = parseBool(v); return nil }},

	// 入口限流
	{Section: "入口限流", Key: "limits.enabled", Label: "开启限流", Kind: "bool", Restart: true,
		Get: func(c *config.Config) string { return b2s(c.Limits.Enabled) }, Set: func(c *config.Config, v string) error { c.Limits.Enabled = parseBool(v); return nil }},
	{Section: "入口限流", Key: "limits.global_rps", Label: "全局每秒请求", Kind: "float", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return f2s(c.Limits.GlobalRPS) }, Set: func(c *config.Config, v string) error { return setFloat(&c.Limits.GlobalRPS, v, 0, 1e9, true, false) }},
	{Section: "入口限流", Key: "limits.global_burst", Label: "全局突发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.GlobalBurst) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Limits.GlobalBurst, v, 0, 1<<30, true, false)
		}},
	{Section: "入口限流", Key: "limits.global_concurrency", Label: "全局并发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.GlobalConcurrency) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Limits.GlobalConcurrency, v, 0, 1<<30, true, false)
		}},
	{Section: "入口限流", Key: "limits.ip_per_minute", Label: "单 IP 每分钟", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.IPPerMinute) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Limits.IPPerMinute, v, 0, 1<<30, true, false)
		}},
	{Section: "入口限流", Key: "limits.ip_burst", Label: "单 IP 突发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.IPBurst) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.IPBurst, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.ip_concurrency", Label: "单 IP 并发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.IPConcurrency) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Limits.IPConcurrency, v, 0, 1<<30, true, false)
		}},
	{Section: "入口限流", Key: "limits.token_per_minute", Label: "单 Token 每分钟", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.TokenPerMinute) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Limits.TokenPerMinute, v, 0, 1<<30, true, false)
		}},
	{Section: "入口限流", Key: "limits.token_burst", Label: "单 Token 突发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.TokenBurst) }, Set: func(c *config.Config, v string) error { return setIntI(&c.Limits.TokenBurst, v, 0, 1<<30, true, false) }},
	{Section: "入口限流", Key: "limits.token_concurrency", Label: "单 Token 并发", Kind: "int", Min: 0, HasMin: true, Restart: true,
		Get: func(c *config.Config) string { return i32(c.Limits.TokenConcurrency) }, Set: func(c *config.Config, v string) error {
			return setIntI(&c.Limits.TokenConcurrency, v, 0, 1<<30, true, false)
		}},
	{Section: "访问控制", Key: "limits.trusted_proxies", Label: "可信代理（XFF）", Kind: "list", Restart: true,
		Hint: "只有这些网段的 X-Forwarded-For 会被采信；默认 172.16.0.0/12（已含 172.16.0.0/16）+ fd00::/8 + 回环。放在反代后必须包含反代网段，否则白/黑名单会把所有请求看成同一个 IP",
		Get:  func(c *config.Config) string { return strings.Join(c.Limits.TrustedProxies, "\n") }, Set: func(c *config.Config, v string) error {
			c.Limits.TrustedProxies = splitLines(v)
			return nil
		}},
	{Section: "访问控制", Key: "access.allow_cidrs", Label: "允许来源（白名单）", Kind: "list",
		Hint: "留空 = 不限来源；填了就只有这些网段能访问。保存前会弹风险提示",
		Get:  func(c *config.Config) string { return strings.Join(c.Access.Allow, "\n") }, Set: func(c *config.Config, v string) error {
			c.Access.Allow = splitLines(v)
			return nil
		}},
	{Section: "访问控制", Key: "access.deny_cidrs", Label: "拒绝来源（黑名单）", Kind: "list",
		Hint: "优先于白名单，命中直接 403；留空 = 不拒绝任何人",
		Get:  func(c *config.Config) string { return strings.Join(c.Access.Deny, "\n") }, Set: func(c *config.Config, v string) error {
			c.Access.Deny = splitLines(v)
			return nil
		}},
	{Section: "访问控制", Key: "access.scope", Label: "生效范围", Kind: "enum", Options: []string{"panel", "all"},
		Hint: "panel = 只约束面板与 /api，加速路径照常开放（默认）；all = 连 /v2 /gh /raw /git 一起约束",
		Get: func(c *config.Config) string {
			if c.Access.Scope == "" {
				return "panel"
			}
			return c.Access.Scope
		}, Set: func(c *config.Config, v string) error { c.Access.Scope = strings.TrimSpace(v); return nil }},

	// 日志
	{Section: "日志", Key: "logging.level", Label: "日志级别", Kind: "enum", Options: []string{"debug", "info", "warn", "error"}, Restart: false,
		Hint: "改完即时生效", Get: func(c *config.Config) string { return effLevel(c.Logging.Level) }, Set: func(c *config.Config, v string) error {
			v = strings.ToLower(strings.TrimSpace(v))
			switch v {
			case "debug", "info", "warn", "error":
				c.Logging.Level = v
				return nil
			}
			return fmt.Errorf("日志级别只支持 debug/info/warn/error")
		}},
	{Section: "日志", Key: "logging.format", Label: "日志格式", Kind: "enum", Options: []string{"text", "json"}, Restart: true,
		Hint: "json 接 Loki/ELK", Get: func(c *config.Config) string { return effFormat(c.Logging.Format) }, Set: func(c *config.Config, v string) error {
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

// effLevel 把日志级别归一化到面板选项：空值当 info，warning 当 warn。
// 不归一化则面板一个选项都不选中。
func effLevel(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return "debug"
	case "warn", "warning":
		return "warn"
	case "error":
		return "error"
	default:
		return "info"
	}
}

// effFormat 同理：空值当 text。
func effFormat(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "json") {
		return "json"
	}
	return "text"
}

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

// cfgSectionGroup 把具体小节归到六个一级分类里：设置项太多，
// 平铺 14 张小卡没人愿意滚。前端按组做一排 chip 过滤。
var cfgSectionGroup = map[string]string{
	"基础": "基础", "网络": "基础",
	"Registry 加速": "加速", "GitHub 加速": "加速",
	"缓存": "缓存", "后台巡检": "缓存", "内容 TTL": "缓存", "CDN 边缘": "缓存",
	"多源调度": "调度", "探测": "调度", "分片并行": "并发",
	"入口限流": "安全", "访问控制": "安全", "信任中转": "安全",
	"日志": "观测", "指标": "观测",
	"数据库备份": "运维",
}

var cfgGroupOrder = []string{"基础", "加速", "缓存", "调度", "并发", "安全", "运维", "观测"}

// configView 把 cfg 按分组整理成前端要的结构。
func configView(c *config.Config, path string) map[string]any {
	groups := map[string][]map[string]any{}
	order := []string{}
	for _, f := range cfgSchema {
		if _, ok := groups[f.Section]; !ok {
			groups[f.Section] = nil
			order = append(order, f.Section)
		}
		fields := map[string]any{
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
		}
		if f.Kind == "secret" {
			// 只告诉前端「有没有」，不给任何可回显的内容。
			fields["set"] = c.GitHub.TokenEnc != ""
		}
		groups[f.Section] = append(groups[f.Section], fields)
	}
	secs := make([]map[string]any, 0, len(order))
	for _, name := range order {
		g := cfgSectionGroup[name]
		if g == "" {
			g = "其他"
		}
		secs = append(secs, map[string]any{"title": name, "group": g, "fields": groups[name]})
	}
	return map[string]any{
		"sections":     secs,
		"groups":       cfgGroupOrder,
		"config_path":  path,
		"restart_note": "除「日志级别」即时生效外，其余参数在启动时已拷贝进各子系统，修改后需重启服务才生效；修改会写入 config.yaml（路径见上）。若配置文件位于镜像内而非持久卷，重启容器会丢失，请确认 -config 指向挂载卷。",
	}
}

// applyConfigValues 校验并套用到 cfg 副本，返回出错字段；不动文件与原 cfg。
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

// cfgFieldError 标记“某个字段校验失败”，handler 据此回 400 + field。
type cfgFieldError struct {
	field string
	err   error
}

func (e *cfgFieldError) Error() string { return e.err.Error() }
func (e *cfgFieldError) Unwrap() error { return e.err }

// writeConfigFile 把 values 写回 YAML，供既有调用与测试使用。
// 事务语义见 writeConfigTx；这里不带锁，锁在 Server.saveConfig。
func writeConfigFile(path string, values map[string]string) error {
	_, _, _, err := writeConfigTx(path, values)
	return err
}

// writeConfigTx 是配置写入的完整事务（不含互斥，锁由调用方持有）：
// 读盘 → 解析/校验基准 → 改副本 → 校验 → 写 Node → 读回校验 → 原子替换。
//
// 保护规则：
//   - 文件不存在：允许用默认值初始化（首次启动）。
//   - 文件存在（包括 0 字节）但解析/校验失败：拒绝写入，原文件字节不动。
//     "存在但为空"不能当成"不存在"，否则一次写坏就会让面板用默认值覆盖现场。
//
// 写盘始终走同目录临时文件 + fsync + rename + 目录 fsync，
// 并且序列化保留 yaml.Node，避免丢注释和未在面板暴露的字段。
func writeConfigTx(path string, values map[string]string) (*config.Config, []string, bool, error) {
	base, root, full, err := readConfigForUpdate(path)
	if err != nil {
		return nil, nil, false, err
	}
	work := cloneConfig(base)
	changed, field, err := applyConfigValues(work, values)
	if err != nil {
		return nil, nil, false, &cfgFieldError{field: field, err: err}
	}

	var out []byte
	if full {
		out, err = yaml.Marshal(work)
	} else {
		// 整份同步：所有已知键按当前生效值写回，未知键与注释保留。
		if err := syncConfigNode(root, work); err != nil {
			return nil, nil, false, fmt.Errorf("更新配置节点失败：%w", err)
		}
		out, err = yaml.Marshal(root)
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("序列化配置失败：%w", err)
	}

	// 落盘前先确认这份字节能被读回并通过校验，避免写出自相矛盾的配置。
	check := config.Default()
	if err := yaml.Unmarshal(out, check); err != nil {
		return nil, nil, false, fmt.Errorf("生成的配置无法解析：%w", err)
	}
	if err := check.Validate(); err != nil {
		return nil, nil, false, fmt.Errorf("生成的配置校验失败：%w", err)
	}

	committed, err := writeFileAtomic(path, out)
	if err != nil && !committed {
		return nil, nil, false, err
	}
	// committed=true 且 err!=nil：文件已替换、仅持久化未确认，仍需发布运行态。
	return work, changed, committed, err
}

// readConfigForUpdate 读取当前磁盘状态。full=true 表示没有可复用的
// Node 结构（文件不存在或为空），需要整份写出。
func readConfigForUpdate(path string) (*config.Config, *yaml.Node, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return config.Default(), nil, true, nil
		}
		return nil, nil, false, fmt.Errorf("读取配置失败：%w", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		// 空文件不能当成"不存在"：它可能是写坏的产物，直接拒绝，避免用默认值覆盖现场。
		return nil, nil, false, fmt.Errorf("配置文件为空，已拒绝写入以保护原文件；请手工写入有效配置或删除该文件")
	}
	base := config.Default()
	if err := yaml.Unmarshal(b, base); err != nil {
		return nil, nil, false, fmt.Errorf("配置文件解析失败，已拒绝写入以保护原文件：%w", err)
	}
	if err := base.Validate(); err != nil {
		return nil, nil, false, fmt.Errorf("配置文件校验失败，已拒绝写入以保护原文件：%w", err)
	}
	root := &yaml.Node{}
	if err := yaml.Unmarshal(b, root); err != nil {
		return nil, nil, false, fmt.Errorf("配置文件解析失败，已拒绝写入以保护原文件：%w", err)
	}
	return base, root, false, nil
}

// cloneConfig 复制配置；slice 需要单独复制，避免工作副本与原对象共享底层数组。
func cloneConfig(c *config.Config) *config.Config {
	out := *c
	if c.Limits.TrustedProxies != nil {
		out.Limits.TrustedProxies = append([]string(nil), c.Limits.TrustedProxies...)
	}
	return &out
}

// writeFileAtomic 交给 config.SaveFile（同目录临时文件 + 0600 + fsync +
// rename + 目录 fsync），保留 committed 语义供调用方判断是否已切换。
func writeFileAtomic(path string, data []byte) (bool, error) {
	return config.SaveFile(path, data)
}

// syncConfigNode 把 cfg 的全部已知键同步进 root：已有键原地改值（注释保留），
// 结构变化递归合并，缺少的键追加，未知键保持不动。
func syncConfigNode(root *yaml.Node, cfg *config.Config) error {
	doc := &yaml.Node{}
	if err := doc.Encode(cfg); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("空配置节点")
	}
	src := doc
	if doc.Kind == yaml.DocumentNode {
		src = doc.Content[0]
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mergeMapping(root.Content[0], src)
		config.Annotate(root)
		return nil
	}
	*root = *doc
	config.Annotate(root)
	return nil
}

// mergeMapping 递归合并两个 mapping：src 的键覆盖 dst，dst 独有的键保留。
func mergeMapping(dst, src *yaml.Node) {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		return
	}
	index := make(map[string]*yaml.Node, len(dst.Content)/2)
	for i := 0; i+1 < len(dst.Content); i += 2 {
		index[dst.Content[i].Value] = dst.Content[i+1]
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		old, ok := index[k.Value]
		if !ok {
			dst.Content = append(dst.Content, k, v)
			continue
		}
		if old.Kind == yaml.MappingNode && v.Kind == yaml.MappingNode {
			mergeMapping(old, v)
			continue
		}
		// 换值前把原节点的注释搬过去。
		v.HeadComment, v.LineComment, v.FootComment = old.HeadComment, old.LineComment, old.FootComment
		*old = *v
	}
}

// saveConfig 在写锁内完成「读盘 → 校验 → 原子落盘 → 发布运行态」。
// 返回 warn 表示配置已保存，但某项附属动作（目录同步、日志热更）失败。
func (s *Server) saveConfig(values map[string]string) ([]string, string, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	if s.cfgPath == "" {
		return nil, "", fmt.Errorf("未指定配置文件路径，无法保存（用 -config 启动才会落盘）")
	}
	work, changed, committed, err := writeConfigTx(s.cfgPath, values)
	if err != nil && !committed {
		return nil, "", err
	}
	// 到这里文件已替换（或本就不存在/为空），发布运行态；发布后 work 只读。
	s.cfg.Store(work)

	var warn string
	if err != nil {
		warn = err.Error()
	}
	if _, ok := values["logging.level"]; ok {
		if e := logging.Setup(work.Logging); e != nil {
			if warn != "" {
				warn += "；"
			}
			warn += "配置已保存，但日志级别热更失败：" + e.Error()
		}
	}
	return changed, warn, nil
}

// configHandler 处理 GET 读取、PUT 校验 + 事务写盘 + 热更日志。
func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	if cfg == nil {
		writeJSON(w, 500, map[string]string{"error": "服务端未加载配置文件"})
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, configView(cfg, s.cfgPath))
		return
	}
	if r.Method != http.MethodPut {
		writeJSON(w, 405, map[string]string{"error": "方法不支持"})
		return
	}
	// 配置提交只是一组字符串键值，1 MiB 足够；超限回 413，不混成 JSON 格式错。
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := decodeJSON(r, &body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, 413, map[string]string{"error": "请求体超过 1 MiB 限制"})
			return
		}
		writeJSON(w, 400, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	if len(body.Values) == 0 {
		writeJSON(w, 400, map[string]string{"error": "没有提交任何改动"})
		return
	}

	changed, warn, err := s.saveConfig(body.Values)
	if err != nil {
		var fe *cfgFieldError
		if errors.As(err, &fe) {
			writeJSON(w, 400, map[string]string{"error": err.Error(), "field": fe.field})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	keys := make([]string, 0, len(body.Values))
	for k := range body.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s.audit(r, "config", "", strings.Join(keys, " "), true)

	resp := map[string]any{"ok": true, "applied": changed}
	if warn != "" {
		resp["warn"] = warn
	}
	writeJSON(w, 200, resp)
}

// WithConfig 让 Server 持有可编辑的 cfg 副本与配置路径。
// 存副本而不是外部指针：发布后的配置对象必须只读，避免调用方后续修改。
func WithConfig(c *config.Config, path string) Option {
	return func(s *Server) {
		if c != nil {
			s.cfg.Store(cloneConfig(c))
		}
		s.cfgPath = path
	}
}

// decodeJSON 解析请求体 JSON。
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return fmt.Errorf("空请求体")
	}
	return json.NewDecoder(r.Body).Decode(v)
}

// splitLines 把多行文本框转成网段列表，顺便丢掉空行。
func splitLines(v string) []string {
	out := make([]string, 0, 4)
	for _, line := range strings.Split(v, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
