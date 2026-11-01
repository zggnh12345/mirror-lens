// Package safehttp 提供一个只允许访问公网的 http.Client。
//
// 代理等于替任意人发请求，检查放在拨号层（Dialer.Control）：
// Control 拿到的是解析后的真实 IP，DNS Rebinding
// （先解析成公网、再重解析成内网）在这一层被挡住。
package safehttp

import (
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
// AllowLoopback 只给测试；AllowPrivate 给「探测局域网源」这类场景。
// 两者默认关，打开等于关掉整层防护，别在公网部署上开。
type Options struct {
	DialTimeout   time.Duration
	HeaderTimeout time.Duration
	MaxRedirects  int
	AllowLoopback bool
	AllowPrivate  bool
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
	// netip 已覆盖内网与回环段，这里补它不管的几段。
	var extra = []string{
		"0.0.0.0/8",     // 本网络
		"100.64.0.0/10", // CGNAT，运营商内网
		"192.0.0.0/24",  // IETF 协议分配
		"240.0.0.0/4",   // 保留
		"255.255.255.255/32",
		"fe80::/10",    // 链路本地
		"ff00::/8",     // 组播
		"64:ff9b::/96", // NAT64，可能映射到内网
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
func (g *Guard) Check(u *url.URL) error {
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
		return g.CheckIP(ip)
	}
	return nil
}

// CheckIP 判断地址能否连接。
func (g *Guard) CheckIP(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return errBlocked
	}
	if g.o.AllowPrivate {
		return nil
	}
	if g.o.AllowLoopback && ip.IsLoopback() {
		return nil
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
		return fmt.Errorf("%w: %s 属于内网/回环/链路本地", errBlocked, ip)
	}
	for _, p := range g.nets {
		if p.Contains(ip) {
			return fmt.Errorf("%w: %s 属于保留网段 %s", errBlocked, ip, p)
		}
	}
	return nil
}

// Dialer 返回带地址检查的拨号器。TLS 探测这类自己握手的场景也得用它，
// 否则就是绕过 Guard 的另一条路。
func (g *Guard) Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   g.o.DialTimeout,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%w: 无法识别的地址 %q", errBlocked, host)
			}
			return g.CheckIP(ip)
		},
	}
}

// Client 返回带防护的客户端：超时留在 Transport 上、整体 Timeout 设 0，
// 否则大文件会被整体超时切断。
func (g *Guard) Client() *http.Client {
	d := g.Dialer()
	tr := &http.Transport{
		DialContext:           d.DialContext,
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
			return g.Check(req.URL)
		},
	}
}
