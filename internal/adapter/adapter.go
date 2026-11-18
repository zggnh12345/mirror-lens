// Package adapter 定义协议适配层的边界。
//
// 适配器负责「看懂请求」：上游怎么拼、缓存身份是什么、能不能缓存、
// 要不要带认证头、允许多少次重定向、响应怎么校验。
// 下载引擎只负责执行，不猜协议规则。
package adapter

import (
	"errors"
	"net/http"
	"net/url"
	"time"
)

var (
	ErrBadPath  = errors.New("路径不合法")
	ErrNoMatch  = errors.New("没有适配器认识这个路径")
	ErrNoTarget = errors.New("无法确定上游地址")
	ErrReadOnly = errors.New("只支持只读操作")
)

// 摘要来源。与 store 里的同名字符串保持一致；这里不引 store，避免把 sqlite 拖进适配器。
const (
	DigestSourceDocker = "docker_digest"
	DigestSourceGitHub = "github_asset_api"
	DigestSourceSUMS   = "publisher_checksum_file"
)

// ResourceKind 是资源的协议语义，不是缓存键格式。
// 认证、重定向、缓存准入等安全逻辑必须依赖它，不能解析 CacheKey 字符串。
type ResourceKind int

const (
	ResourceUnknown ResourceKind = iota
	ResourceManifest
	ResourceBlob
	ResourceOther
)

// Plan 是一次代理请求的完整执行计划。
type Plan struct {
	Kind string // raw / release / http / git / docker / registry

	// ResourceKind 是协议层资源类型；Registry 的 Manifest/Blob 必须显式填写。
	ResourceKind ResourceKind

	// SchedKey 是调度身份：Candidates / Rewrite / Report 的 kind 参数。
	// 空表示等于 Kind。Registry 多源用它把每个 Host 拆成独立调度族
	// （registry:<host>），Kind 仍保持单一 "registry" 供健康族与指标使用——
	// 直接把 Host 写进 Kind 会让熔断、粘性、样本缓存各建一套，而健康族
	// 又会因为未知 kind 落进 github 或按 Host 碎片化。
	SchedKey string
	// LogicalHost 是 Registry 的逻辑仓库 Host（如 registry.k8s.io），
	// 不是镜像站 Host。认证策略和 scope 都按它算，host_prefix 不能混用。
	LogicalHost string
	// Registry 为真表示这是 /v2/ 的 Registry plan。认证策略必须显式声明，
	// 不能靠 LogicalHost 是否为空来猜；11a 之前这里只有 LogicalHost。
	Registry bool
	Upstream *url.URL    // 真正要请求的上游
	Method   string      // 一般 GET；git 的 POST 走透明转发
	Header   http.Header // 追加到上游请求的头（认证、Accept 等）

	Redirects int // 允许的重定向跳数，0 用默认值

	// 缓存身份与策略
	CacheKey  string        // 逻辑资源键，不是上游 URL
	Cacheable bool          // 是否允许落盘
	Immutable bool          // 不可变资源（commit / digest）：命中后不回源
	TTL       time.Duration // 可变资源的重新验证间隔
	RangeOK   bool          // 上游是否支持 Range（决定能否续传/分片）

	// Validate 先看响应头（如 Content-Type），拿到全部字节后再算 SHA-256。
	Validate func(resp *http.Response) error

	// Stream 为真时边下边发，不等整个文件落盘。
	Stream bool

	// MaxBytes 是这次请求单独的落盘上限（0 用网关默认）。
	// Docker layer 动辄上 GB，不能和普通文件共用阈值。
	MaxBytes int64

	// ExpectSHA 非空时，落盘前必须校验内容哈希等于它。
	// Docker blob 的 digest 就是内容哈希，比 ETag 硬。
	ExpectSHA string

	// ExpectSource 说明 ExpectSHA 从哪来（docker_digest / github_api / publisher_sums）。
	// 只用于记录内容可信度，不参与校验判断。
	ExpectSource string

	// OfficialOnly 为真时不试镜像源：上游不是 Docker Hub 时，
	// Hub 镜像站不认 ghcr.io/quay.io 的路径，只会白等一轮 401。
	OfficialOnly bool

	// Claim 非空时是并行竞速链的提交令牌：返回 true 才允许写缓存索引。
	// 两条链同时下载同一资源时，只有第一条能拿到令牌，避免后完成的
	// 那条把索引覆盖成另一份内容。
	Claim func() bool

	// NoStore 表示内容带凭据、任何情况下都不能落盘：入口分流到直通，
	// 保存路径再做一次兜底。与 Cacheable=false 区分开——后者只表示
	// 「不值得按 TTL 缓存」（如 git info/refs），仍可临时落盘。
	NoStore bool

	// Source 是调度选中的镜像源名称，仅用于响应头与日志。
	Source string

	// Label 是给人看的资源名（镜像名/文件名），只用于面板展示。
	// 空的就退回 CacheKey。
	Label string

	// OnResponse 拿到响应头后再决定能不能缓存、缓存多久。
	// Cache-Control 只有这一刻才知道，不能全在 Plan 里定死。
	OnResponse func(resp *http.Response) (cacheable bool, ttl time.Duration)

	// Fallbacks 是主上游全部失败后按顺序再试的站点（裸格式镜像名自动
	// 找 ghcr.io 用）。每个备用站点带自己的 kind，调度按它选加速源。
	Fallbacks []Fallback

	// OnFallback 在开始试备用站点时回调一次，参数是备用站点 host。
	// 适配器用它记住「这个 repo 其实在别处」，下次直接走对站点，
	// 不用再白等一轮 Docker Hub 的 401。
	OnFallback func(host string)

	// ---- 3.5.16 执行策略（不是缓存身份，不参与 CacheKey）----

	// RaceReady 表示本次请求已经过 Registry 竞速准入判定，允许执行层
	// 在同 Host 合格镜像源之间做 Manifest 竞速。判定点只有一个
	// （serveManifest），执行层只读这个结果，不自己再推一遍资格。
	RaceReady bool

	// FlightPolicy 是 singleflight 的执行策略命名空间。空值表示沿用
	// CacheKey（默认执行策略）；registry-race / registry-no-race 用于
	// 隔离竞速执行态。它只在 star_race 实际形成竞速时才会非空，
	// 关闭态与「开启但合格源不足两个」都保持空值，行为与基线一致。
	FlightPolicy string

	// Sequential 强制按顺序回源：客户端 HEAD 必须为真。它同时禁止
	// Registry 竞速和通用响应头对冲，且只对 Registry Manifest 设置，
	// 不能拿去全局关掉 HEAD 的 Hedge。
	Sequential bool

	// FallbackChain 表示这个 plan 副本属于 fetchRace 的备用链：
	// 备用链不再叠加内部对冲，避免「每链两路 × 多链」的四路并发。
	FallbackChain bool
}

// Sched 返回调度身份，未显式设置时回落到 Kind。
func (p *Plan) Sched() string {
	if p.SchedKey != "" {
		return p.SchedKey
	}
	return p.Kind
}

// Fallback 是一个备用上游站点。路径与主上游相同，只换 host。
type Fallback struct {
	URL  *url.URL
	Kind string
}

// Adapter 是每种协议的实现。
type Adapter interface {
	Name() string
	Match(path string) bool
	Plan(r *http.Request) (*Plan, error)
}
