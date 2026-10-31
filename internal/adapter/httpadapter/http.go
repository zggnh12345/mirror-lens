// Package httpadapter 实现通用 HTTP/HTTPS 的 URL Prefix 适配。
//
//	/http/https://example.com/file.iso
//	/http/https/example.com/file.iso
//	/http/https%3A%2F%2Fexample.com%2Ffile.iso
//
// 三种写法等价：反代会折叠 //（Traefik 还拒收 %2F），见 normalizeTarget。
package httpadapter

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

const Prefix = "/http/"

type Adapter struct {
	MaxCache int64 // 超过这个大小就不落盘，只透传
}

func New(maxCache int64) *Adapter {
	if maxCache <= 0 {
		maxCache = 256 << 20
	}
	return &Adapter{MaxCache: maxCache}
}

func (a *Adapter) Name() string { return "http" }

// normalizeTarget 把几种写法归一成完整 URL。
//
// 容忍被折叠的斜杠是被反代逼出来的：Traefik 会把路径里的 // 合并成 /，
// 于是 https://host/a 到这里已经变成 https:/host/a；而整体百分号编码的写法
// 虽然能躲过折叠，Traefik 默认又拒收含 %2F 的路径（直接 400）。
// 两条路都堵着，所以 /http/https/host/path 和 /http/https:/host/path 都得认。
func normalizeTarget(raw string) (string, bool) {
	low := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(low, "https://"), strings.HasPrefix(low, "http://"):
		return raw, true
	case strings.HasPrefix(low, "https:/"), strings.HasPrefix(low, "http:/"):
		// https:/host → https://host（可能被折叠掉不止一个斜杠）
		i := strings.Index(raw, ":/")
		rest := strings.TrimLeft(raw[i+1:], "/")
		if rest == "" {
			return raw, false
		}
		return raw[:i] + "://" + rest, true
	case strings.HasPrefix(low, "https/"), strings.HasPrefix(low, "http/"):
		// https/host/path → https://host/path
		i := strings.Index(raw, "/")
		rest := raw[i+1:]
		if rest == "" {
			return raw, false
		}
		return raw[:i] + "://" + rest, true
	}
	return raw, false
}

func (a *Adapter) Match(p string) bool { return strings.HasPrefix(p, Prefix) }

func (a *Adapter) Plan(r *http.Request) (*adapter.Plan, error) {
	enc := strings.TrimPrefix(r.URL.EscapedPath(), Prefix)
	if enc == "" {
		return nil, adapter.ErrBadPath
	}
	raw, err := url.PathUnescape(enc)
	if err != nil {
		return nil, adapter.ErrBadPath
	}
	raw = strings.TrimSpace(raw)
	raw, ok := normalizeTarget(raw)
	if !ok {
		return nil, fmt.Errorf("%w: 目标要写成 http(s):// 开头的完整地址；反代会折叠 //，也可以写成 /http/https/host/path", adapter.ErrBadPath)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: 地址解析失败", adapter.ErrBadPath)
	}
	u.Fragment = ""

	return &adapter.Plan{
		Kind:      "http",
		Upstream:  u,
		Method:    http.MethodGet,
		Header:    http.Header{},
		Redirects: 5,
		CacheKey:  "http:" + normalize(u),
		Cacheable: true,
		TTL:       5 * time.Minute, // 响应头里的 Cache-Control 会覆盖它
		RangeOK:   true,
		Stream:    true,
		OnResponse: func(resp *http.Response) (bool, time.Duration) {
			return policy(resp)
		},
	}, nil
}

// normalize 让同一个资源的不同写法落到同一个缓存键上。
func normalize(u *url.URL) string {
	c := *u
	c.Fragment = ""
	c.Scheme = strings.ToLower(c.Scheme)
	host := strings.ToLower(c.Hostname())
	port := c.Port()
	if (c.Scheme == "http" && port == "80") || (c.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		c.Host = net.JoinHostPort(host, port)
	} else {
		c.Host = host
	}
	if c.Path == "" {
		c.Path = "/"
	}
	return c.String()
}

// policy 按响应头决定能不能缓存、缓存多久。
func policy(resp *http.Response) (bool, time.Duration) {
	cc := strings.ToLower(resp.Header.Get("Cache-Control"))
	if strings.Contains(cc, "no-store") || strings.Contains(cc, "private") {
		return false, 0
	}
	if strings.Contains(cc, "no-cache") {
		return true, 0 // 存，但每次都要回源验证
	}
	if m := maxAge(cc); m >= 0 {
		return true, time.Duration(m) * time.Second
	}
	if exp := resp.Header.Get("Expires"); exp != "" {
		if t, err := http.ParseTime(exp); err == nil {
			d := time.Until(t)
			if d < 0 {
				d = 0
			}
			return true, d
		}
	}
	return true, 5 * time.Minute
}

func maxAge(cc string) int64 {
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "max-age=") {
			if n, err := strconv.ParseInt(strings.TrimPrefix(part, "max-age="), 10, 64); err == nil {
				return n
			}
		}
	}
	return -1
}
