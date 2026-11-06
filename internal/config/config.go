// Package config 集中管理启动参数：命令行只留四个开关，其余走配置文件。
// 优先级：命令行 > 环境变量 > 配置文件 > 内置默认。
package config

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/limits"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"gopkg.in/yaml.v3"
)

// Duration 让配置文件里能写 "5m" / "30s"，也兼容直接写秒数。
type Duration struct{ time.Duration }

// MarshalYAML 输出可被 UnmarshalYAML 读回的字符串（如 5m0s）。
// 没有它时 yaml 会把结构体展开成 {duration: ...}，整份写出后无法再解析。
func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err == nil {
		if s == "" {
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			// 纯数字按秒处理。必须整串都是数字：Sscanf("%d") 会把
			// "300abc" 读成 300 并报成功，等于把写错的配置静默吞掉。
			secs, e2 := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if e2 == nil && secs >= 0 {
				d.Duration = time.Duration(secs) * time.Second
				return nil
			}
			return fmt.Errorf("时间格式 %q 看不懂，写 30s / 5m / 300 这样", s)
		}
		if v < 0 {
			return fmt.Errorf("时间不能为负: %q", s)
		}
		d.Duration = v
		return nil
	}
	var secs int64
	if err := n.Decode(&secs); err != nil {
		return fmt.Errorf("时间格式不合法: %s", n.Value)
	}
	if secs < 0 {
		return fmt.Errorf("时间不能为负: %s", n.Value)
	}
	d.Duration = time.Duration(secs) * time.Second
	return nil
}

type Cache struct {
	Dir       string  `yaml:"dir"`         // 空 = <data>/cache
	MaxFileMB int64   `yaml:"max_file_mb"` // 单文件上限，超了只透传
	MaxGB     int64   `yaml:"max_gb"`      // 总量上限，0 = 不限
	MinFreeGB int64   `yaml:"min_free_gb"` // 磁盘剩余低于此值就只透传，0 = 不设底线
	WarnAt    float64 `yaml:"warn_at"`     // 用量到多少报一次警
	RecoverAt float64 `yaml:"recover_at"`  // 回落到多少以下才重新武装
}

// Janitor 是后台巡检：容量、过期、半截文件、索引对账。
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

// Sources 是多源调度；enabled=false 时只走官方源。
type Sources struct {
	Enabled       bool     `yaml:"enabled"`
	MaxCandidates int      `yaml:"max_candidates"`
	StickyTTL     Duration `yaml:"sticky_ttl"`
	FailCooldown  Duration `yaml:"fail_cooldown"`
	// MaxPerSource 单源并发回源上限；0 取默认 4，负数不限。
	MaxPerSource int `yaml:"max_per_source"`
	// Dynamic 按实测吞吐在 1..MaxPerSource 间自动调并发（AIMD）：
	// 排队且吞吐不退步就加一路，吞吐掉 40% 或失败就减。
	Dynamic bool `yaml:"dynamic"`

	// 源站生命周期，0 表示用默认值。
	// ArchiveDays：dead/blocked 维持 14 天自动归档
	// CleanupDays：归档 180 天后删除，负数永不删
	// RecoveryMinutes：死源最多 60 分钟探测一次
	ArchiveDays     int `yaml:"archive_days"`
	CleanupDays     int `yaml:"cleanup_days"`
	RecoveryMinutes int `yaml:"recovery_minutes"`
}

// Parallel 是多源分片并行下载，只在有可信整份摘要时启用：
// 分片来自不同源，拼错一片只能靠最终哈希发现。
type Parallel struct {
	Enabled   bool  `yaml:"enabled"`
	MinSizeMB int64 `yaml:"min_size_mb"` // 小于此尺寸不值得分片，握手更慢
	ChunkMB   int64 `yaml:"chunk_mb"`    // 单片大小
	Workers   int   `yaml:"workers"`     // 同时下载几片
	// DisableTailVerify 关掉「尾部扣留」：正常情况下最后一段字节要等
	// 整份 SHA-256 对上才放给客户端，关掉就退回边下边发的旧行为。
	DisableTailVerify bool `yaml:"disable_tail_verify"`
	// RangeTTL 是「该源不支持 Range」这条结论的缓存时长。过期后重新预检，
	// 免得把后来修好 Range 的源永久挡在分片之外。0 表示每次都探。
	RangeTTL Duration `yaml:"range_ttl"`
}

// Private 是带凭据（PAT）的私有请求的加速策略：凭据只发给显式信任的
// 自建中转；没配置或中转故障就回退官方多连接并行。
type Private struct {
	// BreakAfter 连续失败几次熔断；BreakSeconds 熔断时长。
	BreakAfter   int `yaml:"break_after"`
	BreakSeconds int `yaml:"break_seconds"`
	// FallbackConcurrency 回退官方时最多几个文件同时在下载。
	FallbackConcurrency int `yaml:"fallback_concurrency"`
}

// Hedge 控制上游响应头的延迟对冲：首选源迟迟不回响应头时，
// 并行向第二个源发同一请求，谁先交出「符合本次请求语义」的响应头用谁。
// 只在首选源慢的时候多打一个请求，正常情况零成本。
type Hedge struct {
	Enabled bool `yaml:"enabled"`
	// AfterMS 是耐心预算：首选源这么多毫秒还没吐响应头，才启动第二个源。
	AfterMS int `yaml:"after_ms"`
	// GraceMS 只用于条件请求：备用源先回 200 时，再等首选源这么久，
	// 看它是否回 304。304 能省掉一次整文件下载，值得这几十毫秒。
	GraceMS int `yaml:"grace_ms"`
}

// Auth 控制代理路径（/raw /http /git /v2）的访问。
// 默认放行：本服务就是代理，公共加速站不该先要 token；
// require_token=true 时非浏览器客户端必须带 Access Token。
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
	Private  Private        `yaml:"private"`
	Hedge    Hedge          `yaml:"hedge"`
	Probe    Probe          `yaml:"probe"`
	Auth     Auth           `yaml:"auth"`
	Limits   limits.Config  `yaml:"limits"`
	Metrics  Metrics        `yaml:"metrics"`
	CDN      CDN            `yaml:"cdn"`
	Logging  logging.Config `yaml:"logging"`
}

// CDN 控制边缘缓存语义。默认关闭时只发 60 秒保守 TTL，
// 开启后把条目 TTL 让给 CDN，并加 Age / Cache-Tag 便于清理。
type CDN struct {
	Enabled bool `yaml:"enabled"`
	// MaxAge 是边缘 TTL 上限（秒），0 沿用条目 TTL。
	// 必须设上限：release 资产同名覆盖常见，缓存太久会发旧包。
	MaxAge int `yaml:"max_age"`
}

// Metrics 控制 Prometheus 抓取口。
type Metrics struct {
	Enabled bool `yaml:"enabled"`
	// RequireToken 为 true 时 /metrics 要面板登录或 Access Token。
	RequireToken bool `yaml:"require_token"`
}

// Probe 探测开关。探测会请求用户填的地址，默认只允许公网，防 SSRF。
type Probe struct {
	AllowPrivate bool `yaml:"allow_private"`
}

// Default 是没有配置文件时的默认值。
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
			// 4 是实测拐点：南大 GHCR 对同一出口的连接数限速很凶，
			// 8 并发反而把每连接压到 0.2-0.4MB/s，总速不如 4。
			Workers:  4,
			RangeTTL: Duration{6 * time.Hour},
		},
		Private: Private{
			BreakAfter:          3,
			BreakSeconds:        60,
			FallbackConcurrency: 2,
		},
		Hedge:   Hedge{Enabled: true, AfterMS: 1000, GraceMS: 300},
		Probe:   Probe{AllowPrivate: false},
		Auth:    Auth{RequireToken: false},
		Limits:  limits.Default(),
		Metrics: Metrics{Enabled: true},
		CDN:     CDN{Enabled: false, MaxAge: 300},
	}
}

// Load 读配置文件，不存在则用默认值（path 为空也一样）。
// 显式指定的路径读不到要报错，否则打错文件名会静默跑默认值。
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
	// 拼错的键会被 yaml 静默忽略并回落到默认值，启动时只提示、不阻止。
	for _, f := range unknownYAMLFields(b) {
		log.Printf("配置警告: %s 中的 %q 不是已知配置项，已忽略；若是拼写错误请修正", path, f)
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
	if c.Cache.MaxFileMB < 0 || c.Registry.MaxFileMB < 0 || c.Cache.MaxGB < 0 || c.Cache.MinFreeGB < 0 {
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
	if c.Private.BreakAfter < 0 || c.Private.BreakSeconds < 0 || c.Private.FallbackConcurrency < 0 {
		return fmt.Errorf("private 的熔断与并发参数不能是负数")
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
		// 只写主机名，不带 https:// 和路径
		if strings.Contains(u, "://") || strings.Contains(u, "/") {
			return fmt.Errorf("registry.upstream 只写主机名，比如 registry-1.docker.io")
		}
	}
	return nil
}

// unknownYAMLFields 列出 Config 结构体里不存在的 YAML 路径，按字典序返回。
// 只用于启动提示：拼错的键会被 yaml 静默忽略，最后悄悄用默认值。
func unknownYAMLFields(b []byte) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	known := yamlPaths(reflect.TypeOf(Config{}))
	var out []string
	var walk func(*yaml.Node, string)
	walk = func(n *yaml.Node, prefix string) {
		if n == nil || n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := k.Value
			if prefix != "" {
				p = prefix + "." + k.Value
			}
			if !known[p] {
				out = append(out, p)
				continue
			}
			walk(v, p)
		}
	}
	walk(doc.Content[0], "")
	sort.Strings(out)
	return out
}

// yamlPaths 用反射展开结构体的 yaml 键，得到完整路径集合。
// map 的键是动态的，不参与校验；Duration 有自己的反序列化，视为叶子。
func yamlPaths(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	durationType := reflect.TypeOf(Duration{})
	var walk func(reflect.Type, string)
	walk = func(t reflect.Type, prefix string) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t == durationType || t.Kind() == reflect.Map || t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" { // 未导出字段
				continue
			}
			tag := f.Tag.Get("yaml")
			if tag == "-" {
				continue
			}
			parts := strings.Split(tag, ",")
			name := parts[0]
			inline := false
			for _, opt := range parts[1:] {
				if opt == "inline" {
					inline = true
				}
			}
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if inline || (name == "" && f.Anonymous) {
				walk(ft, prefix)
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			out[p] = true
			walk(ft, p)
		}
	}
	walk(t, "")
	return out
}

// CacheDir 返回最终缓存目录。
func (c *Config) CacheDir() string {
	if c.Cache.Dir != "" {
		return c.Cache.Dir
	}
	return strings.TrimRight(c.Data, "/") + "/cache"
}
