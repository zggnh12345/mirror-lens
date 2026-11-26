package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// errRelayUnpinned：既没有钉住地址，也不在 24h 过渡窗口内，禁止拨号。
var errRelayUnpinned = errors.New("信任中转没有可用的钉住地址")

// pinIPChecker 是 Pin 严格地址检查的测试缝：生产恒 nil（走 CheckIPStrict）。
// 测试可注入较宽的 checker，在回环 listener 上验证“只拨钉住 IP / 竞速 / 回落”
// 等连接语义，而不必在生产代码里给回环、TEST-NET 留任何例外。
var pinIPChecker func(netip.Addr) error

func checkPinIP(ip netip.Addr) error {
	if pinIPChecker != nil {
		return pinIPChecker(ip)
	}
	return safehttp.Default().CheckIPStrict(ip)
}

// sameOrigin 比较完整 Origin = Scheme + 规范化 Host + EffectivePort。
// 任一变化都视为 Origin 变化，Pin 场景默认拒绝（不做升级例外）。
func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Scheme != b.Scheme {
		return false
	}
	ha := strings.TrimSuffix(strings.ToLower(a.Hostname()), ".")
	hb := strings.TrimSuffix(strings.ToLower(b.Hostname()), ".")
	if ha == "" || ha != hb {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

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

// fallbackClient 返回未钉住中转的回落客户端：强制严格 public-only。
// 拨号与每跳重定向都忽略 AllowPrivate/AllowLoopback，绝不继承 g.guard 的放宽权限
// （生产 Guard 开了内网/回环也不影响 Pin 回落）。这是 Pin 的 public-only 裁定要求。
// 用 Default().StrictClient() 双保险：Default 无放宽开关，StrictClient 又忽略放宽。
func (g *Gateway) fallbackClient() *http.Client {
	g.relayFallbackOnce.Do(func() {
		g.relayFallbackHTTP = safehttp.Default().StrictClient()
	})
	return g.relayFallbackHTTP
}

// setFallbackClientForTest 仅供测试注入连本机回环桩的回落客户端。
// 生产路径绝不调用；它不新增任何用户可见配置或参数，只是把测试 Guard
// （含 AllowLoopback）装进回落槽并提前消费 sync.Once，使 fallbackClient()
// 不再用严格 public-only 客户端覆盖它。生产强制严格见 fallbackClient()。
func (g *Gateway) setFallbackClientForTest(cli *http.Client) {
	g.relayFallbackOnce.Do(func() { g.relayFallbackHTTP = cli })
}

// clientFor 返回该中转应当使用的客户端：已钉住就只拨钉住的 IP，
// 未钉住则走 g.fallbackClient()（继承本机 Guard 的 SafeHTTP），
// 绝不复用完全裸的 privClient。按 ID+地址缓存，地址变了自然换新的。
func (g *Gateway) clientFor(t TrustedRelay) *http.Client {
	pinned, _ := g.pinUsable(t)
	if !pinned {
		return g.fallbackClient()
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
		// 每个候选拨号前先解析并严格检查（public-only）；非法候选不拨。
		// 不依赖 store.RelayPinIPs 的写入校验作为唯一防线——DB 篡改/缓存污染
		// 都可能让非法地址走到这里（PIN-10/11/12）。
		type candidate struct {
			raw string
		}
		var cands []candidate
		for _, s := range ips {
			a, err := netip.ParseAddr(s)
			if err != nil {
				continue
			}
			a = a.Unmap()
			if err := checkPinIP(a); err != nil {
				continue
			}
			cands = append(cands, candidate{raw: a.String()})
		}
		if len(cands) == 0 {
			return nil, safehttp.ErrBlocked
		}
		// 钉住的 IP 也一视同仁竞速：每 1ms 放一条，谁先握手成功用谁。
		// 串行拨号会被一个丢包的 IP 拖住（实测 v4 卡 1.1s 才轮到 v6）。
		cctx, cancel := context.WithCancel(ctx)
		type out struct {
			c   net.Conn
			err error
		}
		ch := make(chan out, len(cands))
		for i, cand := range cands {
			if i > 0 {
				select {
				case <-cctx.Done():
				case <-time.After(time.Millisecond):
				}
			}
			go func(cand candidate) {
				// Control 在连接建立前再查一次实际地址，堵住任何
				// 由解析/代理引入的地址漂移（DNS Rebinding 闭合）。
				d := &net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
					Control: func(network, address string, _ syscall.RawConn) error {
						host, _, err := net.SplitHostPort(address)
						if err != nil {
							return err
						}
						actual, err := netip.ParseAddr(host)
						if err != nil {
							return err
						}
						return checkPinIP(actual.Unmap())
					},
				}
				c, err := d.DialContext(cctx, "tcp", net.JoinHostPort(cand.raw, port))
				ch <- out{c: c, err: err}
			}(cand)
		}
		var firstErr error
		for i := 0; i < len(cands); i++ {
			var r out
			select {
			case <-ctx.Done():
				cancel()
				return nil, ctx.Err()
			case r = <-ch:
			}
			if r.err == nil {
				cancel()
				// 取消后仍可能有一条刚好连上，收掉它，别漏连接。
				go func(remaining int) {
					for j := 0; j < remaining; j++ {
						if o := <-ch; o.c != nil {
							o.c.Close()
						}
					}
				}(len(ips) - i - 1)
				return r.c, nil
			}
			if firstErr == nil {
				firstErr = r.err
			}
		}
		cancel()
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
	cli := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("重定向超过 5 次")
			}
			if len(via) == 0 {
				return errors.New("CheckRedirect 收到空的重定向链")
			}
			// Origin 基准按请求链隔离：始终取当前请求的 via[0].URL，
			// 绝不用客户端实例级状态缓存——同一个 Client 处理多个请求时，
			// 首次请求的端口不得污染后续请求的 Origin 判断（F3）。
			base := via[0].URL
			// 完整 Origin（Scheme + 规范化 Host + EffectivePort）任一变化，
			// 在第二次拨号之前拒绝，统一返回 ErrBlocked → 上层 403。
			if !sameOrigin(base, req.URL) {
				return fmt.Errorf("%w: 重定向目标 %q 与钉住 Origin 不符", safehttp.ErrBlocked, req.URL)
			}
			// 同 Origin 仍逐跳重查目标 URL（字面量 IP 等）。
			return safehttp.Default().CheckStrict(req.URL)
		},
	}
	return cli
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
