// Package config 集中管理启动参数。
//
// 参数变多之后命令行已经不好用了：这里只留 -config / -addr / -data / -no-seed
// 四个开关，其余全部写在配置文件里，带默认值、带注释。
// 优先级：命令行 > 环境变量 > 配置文件 > 内置默认。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/limits"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"gopkg.in/yaml.v3"
)

// Duration 让配置文件里能写 "5m" / "30s"，也兼容直接写秒数。
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err == nil {
		if s == "" {
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			// 纯数字按秒处理，老习惯
			var secs int64
			if _, e2 := fmt.Sscanf(s, "%d", &secs); e2 == nil {
				d.Duration = time.Duration(secs) * time.Second
				return nil
			}
			return fmt.Errorf("时间格式 %q 看不懂，写 30s / 5m / 300 这样", s)
		}
		d.Duration = v
		return nil
	}
	var secs int64
	if err := n.Decode(&secs); err != nil {
		return fmt.Errorf("时间格式不合法: %s", n.Value)
	}
	d.Duration = time.Duration(secs) * time.Second
	return nil
}

type Cache struct {
	Dir       string  `yaml:"dir"`         // 空 = <data>/cache
	MaxFileMB int64   `yaml:"max_file_mb"` // 单文件上限，超了只透传
	MaxGB     int64   `yaml:"max_gb"`      // 总量上限，0 = 不限
	WarnAt    float64 `yaml:"warn_at"`     // 用量到多少报一次警
	RecoverAt float64 `yaml:"recover_at"`  // 回落到多少以下才重新武装
}

// Janitor 是后台巡检。清半截文件和过期条目都按时间判断，
// 临时文件固定留 1 小时，不单独配。
type Janitor struct {
	Interval     Duration `yaml:"interval"`
	PartialTTL   Duration `yaml:"partial_ttl"`
	ExpiredGrace Duration `yaml:"expired_grace"`
}

type Raw struct {
	TTL Duration `yaml:"ttl"`
}

type Git struct {
	TTL Duration `yaml:"ttl"`
}

type Registry struct {
	Enabled   bool     `yaml:"enabled"`
	Upstream  string   `yaml:"upstream"`
	MaxFileMB int64    `yaml:"max_file_mb"`
	TagTTL    Duration `yaml:"tag_ttl"`
}

// Sources 是多源调度。
//
// enabled=false 时只走官方源，行为与阶段 5 完全一致。
type Sources struct {
	Enabled       bool     `yaml:"enabled"`
	MaxCandidates int      `yaml:"max_candidates"`
	StickyTTL     Duration `yaml:"sticky_ttl"`
	FailCooldown  Duration `yaml:"fail_cooldown"`
	// MaxPerSource 是单台源同时进行的回源请求上限（预检与分片）。0 取默认 4，
	// 负数表示不限。
	MaxPerSource int `yaml:"max_per_source"`
	// Dynamic 打开后，每台源的并发在 1..MaxPerSource 之间按实测吞吐自动调整
	// （AIMD：排队且吞吐不退步就加一路，吞吐掉 40% 或失败就减一路/减半）。
	Dynamic bool `yaml:"dynamic"`

	// 源站生命周期。三个都是天数/分钟，0 表示用默认值。
	//   ArchiveDays：dead/blocked 维持这么久自动归档（默认 14）
	//   CleanupDays：归档维持这么久彻底删除（默认 180；负数 = 永不删除）
	//   RecoveryMinutes：死源/封禁源最多多久探测一次（默认 60）
	ArchiveDays     int `yaml:"archive_days"`
	CleanupDays     int `yaml:"cleanup_days"`
	RecoveryMinutes int `yaml:"recovery_minutes"`
}

// Parallel 是多源分片并行下载。
//
// 只在「有可信整份摘要」时启用：分片来自不同源，拼错一片只有最终哈希能发现，
// 没有摘要就等于拿缓存赌运气。
type Parallel struct {
	Enabled   bool  `yaml:"enabled"`
	MinSizeMB int64 `yaml:"min_size_mb"` // 小于这个尺寸不值得分片（多几次握手反而更慢）
	ChunkMB   int64 `yaml:"chunk_mb"`    // 单片大小
	Workers   int   `yaml:"workers"`     // 同时下载几片
}

// Auth 控制代理路径（/raw /http /git /v2）的访问控制。
//
// 默认放行：这个服务本身就是代理，当公共加速站用时不该先要 token。
// require_token=true 时，非浏览器客户端必须带 Access Token；面板登录不受影响。
type Auth struct {
	RequireToken bool `yaml:"require_token"`
}

type Config struct {
	Addr     string         `yaml:"addr"`
	Data     string         `yaml:"data"`
	NoSeed   bool           `yaml:"no_seed"`
	Cache    Cache          `yaml:"cache"`
	Janitor  Janitor        `yaml:"janitor"`
	Raw      Raw            `yaml:"raw"`
	Git      Git            `yaml:"git"`
	Registry Registry       `yaml:"registry"`
	Sources  Sources        `yaml:"sources"`
	Parallel Parallel       `yaml:"parallel"`
	Probe    Probe          `yaml:"probe"`
	Auth     Auth           `yaml:"auth"`
	Limits   limits.Config  `yaml:"limits"`
	Metrics  Metrics        `yaml:"metrics"`
	CDN      CDN            `yaml:"cdn"`
	Logging  logging.Config `yaml:"logging"`
}

// CDN 控制边缘缓存语义。默认关闭：关着的时候只发 60 秒的保守 TTL，
// 打开后把条目 TTL 让给 CDN，并加 Age / Cache-Tag 方便清理。
type CDN struct {
	Enabled bool `yaml:"enabled"`
	// MaxAge 是给边缘的 TTL 上限（秒）。0 表示沿用条目自己的 TTL。
	// 有上限才安全：release 资产同名覆盖很常见，缓存一年会发旧包。
	MaxAge int `yaml:"max_age"`
}

// Metrics 控制 Prometheus 抓取口。
type Metrics struct {
	Enabled bool `yaml:"enabled"`
	// RequireToken 为 true 时 /metrics 要面板登录或 Access Token。
	RequireToken bool `yaml:"require_token"`
}

// Probe 是探测行为的开关。探测会去请求用户填的地址，默认只允许公网，
// 免得「加个源」变成访问内网服务的入口。
type Probe struct {
	AllowPrivate bool `yaml:"allow_private"`
}

// Default 是没写配置文件时也成立的一套值。
func Default() *Config {
	return &Config{
		Addr: ":8080",
		Data: "/data",
		Cache: Cache{
			MaxFileMB: 256,
			MaxGB:     100,
			WarnAt:    0.9,
			RecoverAt: 0.85,
		},
		Janitor: Janitor{
			Interval:     Duration{10 * time.Minute},
			PartialTTL:   Duration{24 * time.Hour},
			ExpiredGrace: Duration{72 * time.Hour},
		},
		Raw: Raw{TTL: Duration{300 * time.Second}},
		Git: Git{TTL: Duration{30 * time.Second}},
		Registry: Registry{
			Enabled:   true,
			Upstream:  "registry-1.docker.io",
			MaxFileMB: 4096,
			TagTTL:    Duration{300 * time.Second},
		},
		Sources: Sources{
			Enabled:       true,
			MaxCandidates: 3,
			StickyTTL:     Duration{10 * time.Minute},
			FailCooldown:  Duration{5 * time.Minute},
			MaxPerSource:  4,
			Dynamic:       true,

			ArchiveDays:     14,
			CleanupDays:     180,
			RecoveryMinutes: 60,
		},
		Parallel: Parallel{
			Enabled:   true,
			MinSizeMB: 8,
			ChunkMB:   4,
			Workers:   4,
		},
		Probe:   Probe{AllowPrivate: false},
		Auth:    Auth{RequireToken: false},
		Limits:  limits.Default(),
		Metrics: Metrics{Enabled: true},
		CDN:     CDN{Enabled: false, MaxAge: 300},
	}
}

// Load 读配置文件；文件不存在时用默认值（path 为空也一样）。
// 显式指定的路径读不到会报错——打错文件名却静默跑默认值最难查。
func Load(path string, required bool) (*Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return c, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr 不能为空")
	}
	if c.Data == "" {
		return fmt.Errorf("data 不能为空")
	}
	switch strings.ToLower(c.Logging.Format) {
	case "", "text", "json":
	default:
		return fmt.Errorf("logging.format 只支持 text/json，收到 %q", c.Logging.Format)
	}
	switch strings.ToLower(c.Logging.Level) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("logging.level 只支持 debug/info/warn/error，收到 %q", c.Logging.Level)
	}
	if c.Cache.MaxFileMB < 0 || c.Registry.MaxFileMB < 0 || c.Cache.MaxGB < 0 {
		return fmt.Errorf("容量上限不能是负数")
	}
	if c.Raw.TTL.Duration < 0 || c.Git.TTL.Duration < 0 || c.Registry.TagTTL.Duration < 0 {
		return fmt.Errorf("TTL 不能是负数")
	}
	if c.CDN.MaxAge < 0 {
		return fmt.Errorf("cdn.max_age 不能是负数")
	}
	if c.Cache.WarnAt < 0 || c.Cache.WarnAt > 1 || c.Cache.RecoverAt < 0 || c.Cache.RecoverAt > 1 {
		return fmt.Errorf("cache.warn_at / recover_at 是 0~1 的比例")
	}
	if c.Cache.WarnAt > 0 && c.Cache.RecoverAt >= c.Cache.WarnAt {
		return fmt.Errorf("cache.recover_at 必须小于 warn_at，否则告警永远不会复位")
	}
	if c.Janitor.Interval.Duration <= 0 {
		return fmt.Errorf("janitor.interval 必须大于 0")
	}
	if c.Janitor.PartialTTL.Duration < 0 || c.Janitor.ExpiredGrace.Duration < 0 {
		return fmt.Errorf("janitor 的时限不能是负数")
	}
	if c.Sources.Enabled {
		if c.Sources.MaxCandidates < 1 || c.Sources.MaxCandidates > 10 {
			return fmt.Errorf("sources.max_candidates 取值 1~10")
		}
		if c.Sources.MaxPerSource > 64 {
			return fmt.Errorf("sources.max_per_source 过大：%d", c.Sources.MaxPerSource)
		}
		if c.Sources.StickyTTL.Duration < 0 || c.Sources.FailCooldown.Duration < 0 {
			return fmt.Errorf("sources 的时间不能是负数")
		}
	}
	if c.Parallel.Enabled {
		if c.Parallel.MinSizeMB < 0 || c.Parallel.ChunkMB <= 0 {
			return fmt.Errorf("parallel.chunk_mb 必须大于 0，min_size_mb 不能是负数")
		}
		if c.Parallel.Workers < 2 || c.Parallel.Workers > 16 {
			return fmt.Errorf("parallel.workers 取值 2~16")
		}
	}
	if c.Registry.Enabled {
		u := c.Registry.Upstream
		if u == "" {
			return fmt.Errorf("registry.upstream 不能为空")
		}
		// 只写主机名，别把 https:// 和路径塞进来
		if strings.Contains(u, "://") || strings.Contains(u, "/") {
			return fmt.Errorf("registry.upstream 只写主机名，比如 registry-1.docker.io")
		}
	}
	return nil
}

// CacheDir 返回最终缓存目录。
func (c *Config) CacheDir() string {
	if c.Cache.Dir != "" {
		return c.Cache.Dir
	}
	return strings.TrimRight(c.Data, "/") + "/cache"
}
