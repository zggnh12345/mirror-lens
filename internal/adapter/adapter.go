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

// Plan 是一次代理请求的完整执行计划。
type Plan struct {
	Kind     string      // raw / release / http / git / docker
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

	// OfficialOnly 为真时不试镜像源：上游不是 Docker Hub 时，
	// Hub 镜像站不认 ghcr.io/quay.io 的路径，只会白等一轮 401。
	OfficialOnly bool

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
