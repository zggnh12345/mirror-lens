// Package endpoint 实现 3.5.24 的地址身份模型。
//
// 四个概念在本项目中的边界：
//   - RegistryIdentity：纯 Host，无 Scheme/Port，由 internal/registryhost 负责；
//   - Endpoint：Scheme + CanonicalHost + EffectivePort；
//   - Authority：CanonicalHost:EffectivePort（IPv6 带方括号）；
//   - Origin：Endpoint 的规范字符串（Scheme + Authority，端口始终显式）。
//
// 本包只做解析与规范化，不做网络访问、不做白名单判断。
package endpoint

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Scheme 是 Endpoint 允许的协议。
type Scheme string

const (
	HTTP  Scheme = "http"
	HTTPS Scheme = "https"
)

// Valid 报告 Scheme 是否为受支持的 http/https。
func (s Scheme) Valid() bool { return s == HTTP || s == HTTPS }

// DefaultPort 返回 Scheme 的默认端口；未知 Scheme 返回 0。
func (s Scheme) DefaultPort() int {
	switch s {
	case HTTP:
		return 80
	case HTTPS:
		return 443
	default:
		return 0
	}
}

// Endpoint 是 Scheme + CanonicalHost + EffectivePort 的不可变值。
// 零值表示「未设置」，不是合法的 http/https Endpoint。
type Endpoint struct {
	scheme Scheme
	host   string
	port   int
}

// Scheme 返回规范化后的 Scheme。
func (e Endpoint) Scheme() Scheme { return e.scheme }

// Host 返回规范化后的 Host：域名小写且无尾点；IPv6 为压缩形式且不带方括号。
func (e Endpoint) Host() string { return e.host }

// Port 返回有效端口（默认端口已补齐）。
func (e Endpoint) Port() int { return e.port }

// IsZero 报告是否为零值。
func (e Endpoint) IsZero() bool { return e.scheme == "" && e.host == "" && e.port == 0 }

// Equal 按值比较两个 Endpoint。
func (e Endpoint) Equal(o Endpoint) bool { return e == o }

// Authority 返回 host:port；IPv6 自动加方括号。零值返回空串。
func (e Endpoint) Authority() string {
	if e.IsZero() {
		return ""
	}
	return net.JoinHostPort(e.host, strconv.Itoa(e.port))
}

// Origin 返回 Scheme://Authority，端口始终显式。零值返回空串。
func (e Endpoint) Origin() string {
	if e.IsZero() {
		return ""
	}
	return string(e.scheme) + "://" + e.Authority()
}

// String 与 Origin 相同，便于日志与错误信息。
func (e Endpoint) String() string { return e.Origin() }

// New 由 Scheme、Host、Port 构造 Endpoint。port 为 0 时使用 Scheme 默认端口。
func New(scheme Scheme, host string, port int) (Endpoint, error) {
	s := Scheme(strings.ToLower(strings.TrimSpace(string(scheme))))
	if !s.Valid() {
		return Endpoint{}, fmt.Errorf("不支持的 Scheme %q（只允许 http/https）", scheme)
	}
	ch, err := CanonicalHost(host)
	if err != nil {
		return Endpoint{}, err
	}
	if port == 0 {
		port = s.DefaultPort()
	}
	if port < 1 || port > 65535 {
		return Endpoint{}, fmt.Errorf("端口 %d 超出范围 1-65535", port)
	}
	return Endpoint{scheme: s, host: ch, port: port}, nil
}

// Parse 解析一个基地址并返回其 Origin。允许路径（如 /v2、/owner/repo），
// 但拒绝用户信息、query、fragment 与歧义输入。
func Parse(raw string) (Endpoint, error) { return parse(raw, true) }

// ParseOrigin 解析一个严格 Origin：除空路径或 "/" 外不接受任何路径。
func ParseOrigin(raw string) (Endpoint, error) { return parse(raw, false) }

func parse(raw string, allowPath bool) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, errors.New("地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("地址解析失败: %w", err)
	}
	if u.Scheme == "" {
		return Endpoint{}, errors.New("地址缺少 Scheme（只支持 http/https）")
	}
	if u.User != nil {
		return Endpoint{}, errors.New("地址不允许包含用户信息")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Endpoint{}, errors.New("地址不允许包含 query 或 fragment")
	}
	if !allowPath && u.Path != "" && u.Path != "/" {
		return Endpoint{}, errors.New("Origin 不允许包含路径")
	}
	// 裸 IPv6：authority 里出现多个冒号且没有以 [ 开头，说明用户漏了方括号。
	// 单冒号是 host:port，交给下面的 Port() 解析。
	if strings.Count(u.Host, ":") > 1 && !strings.HasPrefix(u.Host, "[") {
		return Endpoint{}, errors.New("IPv6 地址必须使用方括号")
	}
	host := u.Hostname()
	if host == "" {
		return Endpoint{}, errors.New("地址缺少 Host")
	}
	// 显式空端口：https://host:
	if strings.HasSuffix(u.Host, ":") {
		return Endpoint{}, errors.New("端口不能为空")
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Endpoint{}, fmt.Errorf("端口不是数字: %q", p)
		}
		if n == 0 {
			return Endpoint{}, errors.New("端口不能为 0")
		}
		port = n
	}
	return New(Scheme(u.Scheme), host, port)
}

// CanonicalHost 规范化 Host：
//   - IPv4/IPv6 使用 netip 的规范形式，IPv4-mapped IPv6 还原为 IPv4；
//   - 域名小写、去尾点，按既有 DNS Label 规则校验；
//   - 拒绝 zone、方括号、非法 IPv4 形态与非 ASCII。
func CanonicalHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", errors.New("Host 不能为空")
	}
	if strings.HasPrefix(h, "[") || strings.HasSuffix(h, "]") {
		return "", errors.New("Host 不应包含方括号")
	}
	if strings.Contains(h, "%") {
		return "", errors.New("Host 不允许包含 zone")
	}
	if strings.Contains(h, ":") {
		ip, err := netip.ParseAddr(h)
		if err != nil {
			return "", fmt.Errorf("IPv6 地址不合法: %w", err)
		}
		if ip.Zone() != "" {
			return "", errors.New("Host 不允许包含 zone")
		}
		return ip.Unmap().String(), nil
	}
	if looksLikeIPv4(h) {
		ip, err := netip.ParseAddr(h)
		if err != nil || !ip.Is4() {
			return "", fmt.Errorf("IPv4 地址不合法: %q", h)
		}
		return ip.String(), nil
	}
	return canonicalDomain(h)
}

func looksLikeIPv4(h string) bool {
	if !strings.Contains(h, ".") {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

func canonicalDomain(h string) (string, error) {
	h = strings.ToLower(h)
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return "", errors.New("Host 不能为空")
	}
	if len(h) > 253 {
		return "", errors.New("Host 太长")
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return "", errors.New("Host 含空标签")
		}
		if len(label) > 63 {
			return "", fmt.Errorf("Host 标签太长: %q", label)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("Host 标签不能以连字符开头或结尾: %q", label)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return "", fmt.Errorf("Host 含非法字符: %q", label)
		}
	}
	return h, nil
}
