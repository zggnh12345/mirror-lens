package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// errRelayUnpinned：既没有钉住地址，也不在 24h 过渡窗口内，禁止拨号。
var errRelayUnpinned = errors.New("信任中转没有可用的钉住地址")

// pinUsable 判断这个中转现在能不能拨，以及要不要退回旧版系统解析。
// pinned=false 时走 g.privClient（系统解析），这是升级过渡期的唯一让步。
func (g *Gateway) pinUsable(t TrustedRelay) (pinned bool, err error) {
	switch t.PinState {
	case "expired":
		return false, errRelayUnpinned
	case "", "unresolved":
		if t.PinState == "unresolved" && !t.GraceEnd.IsZero() && time.Now().After(t.GraceEnd) {
			return false, errRelayUnpinned
		}
		return false, nil
	}
	if len(store.RelayPinIPs(t.PinIPv4))+len(store.RelayPinIPs(t.PinIPv6)) == 0 {
		return false, errRelayUnpinned
	}
	return true, nil
}

// clientFor 返回该中转应当使用的客户端：已钉住就只拨钉住的 IP，
// 过渡期/未初始化仍用旧客户端。按 ID+地址缓存，地址变了自然换新的。
func (g *Gateway) clientFor(t TrustedRelay) *http.Client {
	pinned, _ := g.pinUsable(t)
	if !pinned {
		return g.privClient
	}
	key := fmt.Sprintf("%d|%s|%s", t.ID, t.PinIPv4, t.PinIPv6)
	if v, ok := g.pinClients.Load(key); ok {
		return v.(*http.Client)
	}
	cli := pinnedClient(t)
	actual, _ := g.pinClients.LoadOrStore(key, cli)
	return actual.(*http.Client)
}

// pinnedClient 的 DialContext 只认钉住地址，且拨号目标必须就是钉住的域名：
// 中转返回 3xx 到别的主机时不会拿钉住的 IP 去连它，重定向另过 Guard。
func pinnedClient(t TrustedRelay) *http.Client {
	ips := append(store.RelayPinIPs(t.PinIPv4), store.RelayPinIPs(t.PinIPv6)...)
	pinHost := t.PinHost
	if h, _, err := net.SplitHostPort(pinHost); err == nil {
		pinHost = h
	}
	pinHost = strings.TrimSuffix(strings.ToLower(pinHost), ".")

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(strings.TrimSuffix(strings.ToLower(host), "."), pinHost) {
			return nil, fmt.Errorf("拨号目标 %q 与钉住的域名 %q 不符，拒绝", host, pinHost)
		}
		var firstErr error
		for _, ip := range ips {
			d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
			if err == nil {
				return c, nil
			}
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				break
			}
		}
		if firstErr == nil {
			firstErr = errRelayUnpinned
		}
		return nil, fmt.Errorf("钉住地址全部连不上：%w", firstErr)
	}
	tr := &http.Transport{
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("重定向超过 5 次")
			}
			return safehttp.Default().Check(req.URL)
		},
	}
}

// pinWarn 给「不能拨」的中转限速告警：一个站挂了不该把日志刷爆。
func (g *Gateway) pinWarn(id int64, format string, args ...any) {
	g.pinMu.Lock()
	if g.pinWarnAt == nil {
		g.pinWarnAt = make(map[int64]time.Time)
	}
	last := g.pinWarnAt[id]
	now := time.Now()
	if now.Sub(last) < 5*time.Minute {
		g.pinMu.Unlock()
		return
	}
	g.pinWarnAt[id] = now
	g.pinMu.Unlock()
	log.Printf(format, args...)
}
