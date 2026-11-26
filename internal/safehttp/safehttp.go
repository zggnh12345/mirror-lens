// Package safehttp 提供一个只允许访问公网的 http.Client。
//
// 代理等于替任意人发请求，检查放在拨号层（Dialer.Control）：
// Control 拿到的是解析后的真实 IP，DNS Rebinding
// （先解析成公网、再重解析成内网）在这一层被挡住。
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// Options 控制超时与例外。
// AllowLoopback 只给测试；AllowPrivate 只放行 RFC1918 与 IPv6 ULA。
// 两者默认关，打开等于关掉对应层防护，别在公网部署上开。
// FakeIP、CGNAT、文档段、基准测试段、链路本地、组播等特殊用途地址
// 不属于 AllowPrivate 的放行范围，始终拒绝。
type Options struct {
	DialTimeout   time.Duration
	HeaderTimeout time.Duration
	MaxRedirects  int
	AllowLoopback bool
	AllowPrivate  bool
	// IPFamily：auto = 双栈竞速（两族各发一条，先握手成功的赢）；
	// v4/v6 = 只解析并拨该族。空值等同 auto。
	IPFamily string
	// FallbackDelay 覆盖竞速起跑差：0 = 默认 1ms（几乎同时）；负值在 Go
	// 里含义是「关掉竞速」，不要用。主要给实测对比留个口子。
	FallbackDelay time.Duration
}

type Guard struct {
	o    Options
	nets []netip.Prefix
}

var errBlocked = errors.New("目标地址不允许访问")

// ErrBlocked 供上层判断被拦，好回 403 而不是 502。
var ErrBlocked = errBlocked

func New(o Options) *Guard {
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.HeaderTimeout <= 0 {
		o.HeaderTimeout = 20 * time.Second
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = 5
	}
	// 这些是无论 AllowPrivate/AllowLoopback 如何配置都必须拒绝的特殊用途段。
	// RFC1918 与 IPv6 ULA 不在此列，它们由 AllowPrivate 单独控制。
	var extra = []string{
		"0.0.0.0/8",          // 本网络
		"100.64.0.0/10",      // CGNAT，运营商内网
		"169.254.0.0/16",     // IPv4 链路本地 / 云元数据
		"192.0.0.0/24",       // IETF 协议分配
		"192.0.2.0/24",       // TEST-NET-1
		"192.88.99.0/24",     // 6to4 中继任播
		"198.18.0.0/15",      // 基准测试 / FakeIP
		"198.51.100.0/24",    // 文档示例
		"203.0.113.0/24",     // 文档示例
		"224.0.0.0/4",        // 组播
		"240.0.0.0/4",        // 保留
		"255.255.255.255/32", // 广播
		"::/128",             // 未指定
		"64:ff9b::/96",       // NAT64，可能映射到内网
		"64:ff9b:1::/48",     // 本地 NAT64
		"100::/64",           // 丢弃前缀
		"2001::/23",          // IETF 协议分配（含 Teredo、基准测试）
		"2001:2::/48",        // 基准测试
		"2001:db8::/32",      // 文档示例
		"2002::/16",          // 6to4
		"3fff::/20",          // 文档示例（RFC 9637）
		"5f00::/16",          // SRv6 SID
		"fe80::/10",          // 链路本地
		"fec0::/10",          // 站点本地（已废弃，仍不该连）
		"ff00::/8",           // 组播
	}
	g := &Guard{o: o}
	for _, c := range extra {
		if p, err := netip.ParsePrefix(c); err == nil {
			g.nets = append(g.nets, p)
		}
	}
	return g
}

func Default() *Guard { return New(Options{}) }

// Check 校验 URL 本身：协议、凭据、IP 字面量主机。
// 域名不在这里解析，把关在拨号层。
func (g *Guard) Check(u *url.URL) error { return g.checkURL(u, g.CheckIP) }

// CheckStrict 与 Check 相同，但忽略 AllowPrivate/AllowLoopback。
func (g *Guard) CheckStrict(u *url.URL) error { return g.checkURL(u, g.CheckIPStrict) }

func (g *Guard) checkURL(u *url.URL, check func(netip.Addr) error) error {
	if u == nil {
		return errors.New("空 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("URL 里不允许带用户名密码")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("缺少主机名")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return check(ip)
	}
	return nil
}

// CheckIP 判断地址能否连接。拒绝顺序：
//  1. 无论任何开关都拒绝的特殊用途段；
//  2. 回环：只由 AllowLoopback 放行；
//  3. RFC1918 / ULA：只由 AllowPrivate 放行；
//  4. 其余未指定/组播/链路本地一律拒绝。
func (g *Guard) CheckIP(ip netip.Addr) error {
	return g.checkIP(ip, g.o.AllowPrivate, g.o.AllowLoopback)
}

// CheckIPStrict 忽略 AllowPrivate/AllowLoopback，供 Pin 等严格路径使用。
func (g *Guard) CheckIPStrict(ip netip.Addr) error { return g.checkIP(ip, false, false) }

func (g *Guard) checkIP(ip netip.Addr, allowPrivate, allowLoopback bool) error {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return errBlocked
	}
	for _, p := range g.nets {
		if p.Contains(ip) {
			return fmt.Errorf("%w: %s 属于保留网段 %s", errBlocked, ip, p)
		}
	}
	if ip.IsLoopback() {
		if allowLoopback {
			return nil
		}
		return fmt.Errorf("%w: %s 属于回环地址", errBlocked, ip)
	}
	if ip.IsPrivate() {
		if allowPrivate {
			return nil
		}
		return fmt.Errorf("%w: %s 属于内网地址", errBlocked, ip)
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: %s 属于未指定/组播/链路本地", errBlocked, ip)
	}
	return nil
}

// Dialer 返回带地址检查的拨号器。TLS 探测这类自己握手的场景也得用它，
// 否则就是绕过 Guard 的另一条路。
func (g *Guard) Dialer() *net.Dialer { return g.dialerFor(g.CheckIP) }

// StrictDialer 与 Dialer 相同，但忽略 AllowPrivate/AllowLoopback。
func (g *Guard) StrictDialer() *net.Dialer { return g.dialerFor(g.CheckIPStrict) }

func (g *Guard) dialerFor(check func(netip.Addr) error) *net.Dialer {
	return &net.Dialer{
		Timeout:   g.o.DialTimeout,
		KeepAlive: 30 * time.Second,
		FallbackDelay: func() time.Duration {
			if g.o.FallbackDelay != 0 {
				return g.o.FallbackDelay
			}
			return time.Millisecond
		}(),
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%w: 无法识别的地址 %q", errBlocked, host)
			}
			return check(ip)
		},
	}
}

// orderIPs 按 IPFamily 过滤并排序候选地址。
// auto 把 v4 排前面，供 OrderIPs 这类自己握手的场景使用；
// 正常拨号（dialContext）在 auto 下走标准库竞速，不经过这里。
func (g *Guard) family() string {
	if g.o.IPFamily == "" {
		return "auto"
	}
	return g.o.IPFamily
}

func (g *Guard) orderIPs(ips []netip.Addr) []netip.Addr {
	mode := g.family()
	var v4, v6 []netip.Addr
	for _, ip := range ips {
		ip = ip.Unmap()
		switch {
		case ip.Is4():
			v4 = append(v4, ip)
		case ip.Is6():
			v6 = append(v6, ip)
		}
	}
	switch mode {
	case "v4":
		return v4
	case "v6":
		return v6
	default:
		return append(v4, v6...)
	}
}

// OrderIPs 供需要自己握手的场景（TLS 探测）复用同一套族偏好。
// 注意：这里不走竞速，auto 仍按 v4 优先排序。
func (g *Guard) OrderIPs(ips []netip.Addr) []netip.Addr { return g.orderIPs(ips) }

// dialContext：auto 交给标准库做 Happy Eyeballs（两族同时起跑，先握手成功的赢），
// v4/v6 强制档才自己解析、过滤、顺序拨。两条路都保留 Control 里的内网校验。
func (g *Guard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return g.dialContextFor(ctx, network, addr, g.CheckIP)
}

func (g *Guard) strictDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return g.dialContextFor(ctx, network, addr, g.CheckIPStrict)
}

func (g *Guard) dialContextFor(ctx context.Context, network, addr string, check func(netip.Addr) error) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := g.dialerFor(check)
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := check(ip); err != nil {
			return nil, err
		}
		return d.DialContext(ctx, network, addr)
	}
	if mode := g.family(); mode == "auto" {
		// 不预解析：把域名交给拨号器，由它按 RFC 6724 排序后双栈竞速。
		// FallbackDelay=1ms 等于同时起跑；负值在 Go 里是关掉竞速、退化成顺序拨号。
		return d.DialContext(ctx, network, addr)
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	ips = g.orderIPs(ips)
	if len(ips) == 0 {
		return nil, fmt.Errorf("没有可用的 %s 地址", g.o.IPFamily)
	}
	var lastErr error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// Client 返回带防护的客户端：超时留在 Transport 上、整体 Timeout 设 0，
// 否则大文件会被整体超时切断。
func (g *Guard) Client() *http.Client {
	return g.clientFor(g.dialContext, g.Check)
}

// StrictClient 与 Client 相同，但拨号与重定向都忽略 AllowPrivate/AllowLoopback。
// 调用方（Pin 等）不得再用裸 http.Client 代替它。
func (g *Guard) StrictClient() *http.Client {
	return g.clientFor(g.strictDialContext, g.CheckStrict)
}

func (g *Guard) clientFor(dial func(context.Context, string, string) (net.Conn, error), check func(*url.URL) error) *http.Client {
	tr := &http.Transport{
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: g.o.HeaderTimeout,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   8,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= g.o.MaxRedirects {
				return fmt.Errorf("重定向超过 %d 次", g.o.MaxRedirects)
			}
			// 每跳都重新校验：内网 IP 字面量在这里拦，域名交给拨号层。
			return check(req.URL)
		},
	}
}
