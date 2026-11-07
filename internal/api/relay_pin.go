package api

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// relayLookup 可被测试替换，避免用例真的去查 DNS。
var relayLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// 这些网段不是“能不能连”的问题，而是“这个地址根本不是这台机器的公网地址”：
// fakeip 的 198.18.0.0/15 / fc00::/7、CGNAT、保留段、文档段、组播、链路本地。
// 钉住它们等于把中转永久钉死在一个假地址上，比不钉更糟，所以解析阶段就丢掉。
var bogusPinNets = func() []netip.Prefix {
	raw := []string{
		"0.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32",
		"fc00::/7", "fe80::/10", "ff00::/8",
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, r := range raw {
		out = append(out, netip.MustParsePrefix(r))
	}
	return out
}()

// bogusPinAddr 判断解析结果是否属于“不可能是这台机器真实地址”的段，返回原因。
func bogusPinAddr(a netip.Addr) string {
	a = a.Unmap()
	for _, p := range bogusPinNets {
		if p.Contains(a) {
			return p.String()
		}
	}
	return ""
}

// ensurePins 推进信任中转的钉住状态机。只在启动和面板改动时跑：
//
//	""            → 解析成功：pending（钉住本次结果）；失败：unresolved + 迁移窗口
//	pending       → 不动（永不重新解析）
//	confirmed     → 不动（只认管理员填的地址）
//	stale         → 不动（沿用上一次可用地址，拒绝新 DNS）
//	unresolved    → 窗口内重试，成功转 pending；窗口到期转 expired
//	expired       → 不动（禁止拨号）
//
// 失败不阻断信任源加载：窗口内仍按旧版系统解析走，只是单独计数并告警。
func (s *Server) ensurePins(list []store.TrustedRelay) {
	if len(list) == 0 {
		return
	}
	timeout := 5 * time.Second
	grace := 24 * time.Hour
	if cfg := s.cfg.Load(); cfg != nil {
		if d := cfg.Relay.ResolverTimeout.Duration; d > 0 {
			timeout = d
		}
		if h := cfg.Relay.PinGraceHours; h > 0 {
			grace = time.Duration(h) * time.Hour
		}
	}
	now := time.Now()
	var wg sync.WaitGroup
	for _, r := range list {
		switch r.PinState {
		case "pending", "confirmed", "stale", "expired":
			continue
		case "unresolved":
			if r.PinGraceUntil > 0 && !now.Before(time.Unix(r.PinGraceUntil, 0)) {
				if err := s.st.SetRelayPin(r.ID, "expired", r.PinIPv4, r.PinIPv6, r.PinGraceUntil); err != nil {
					log.Printf("api: 信任中转 %s 标记 expired 失败: %v", r.Name, err)
				}
				log.Printf("信任中转 %s 迁移窗口已过仍未解析成功，停止拨号（expired）", r.Name)
				continue
			}
		}

		wg.Add(1)
		go func(r store.TrustedRelay) {
			defer wg.Done()
			v4, v6, err := resolveRelayHost(r.PinHost, timeout)
			if err == nil {
				if e := s.st.SetRelayPin(r.ID, "pending", strings.Join(v4, ","), strings.Join(v6, ","), 0); e != nil {
					log.Printf("api: 信任中转 %s 保存钉住结果失败: %v", r.Name, e)
					return
				}
				log.Printf("信任中转 %s 已钉住解析结果：v4=%v v6=%v", r.Name, v4, v6)
				return
			}
			if r.PinState == "unresolved" {
				log.Printf("信任中转 %s 仍解析失败：%v；继续沿用系统解析至 %s",
					r.Name, err, time.Unix(r.PinGraceUntil, 0).Format("2006-01-02 15:04"))
				return
			}
			until := now.Add(grace)
			if e := s.st.SetRelayPin(r.ID, "unresolved", "", "", until.Unix()); e != nil {
				log.Printf("api: 信任中转 %s 保存过渡状态失败: %v", r.Name, e)
				return
			}
			log.Printf("信任中转 %s 首次解析失败：%v；%s 内沿用系统解析（未钉住，高危）",
				r.Name, err, grace)
		}(r)
	}
	wg.Wait()
}

// checkPinList 校验管理员手填的地址：逐个合法、地址族对得上、且必须是
// 公网地址——否则「钉 IP」就成了绕过 Guard 连内网的后门。
func checkPinList(v string, want4 bool) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	ips := store.RelayPinIPs(v)
	if len(ips) != strings.Count(v, ",")+1 {
		return fmt.Errorf("含非法地址，逗号分隔只填 IP")
	}
	guard := safehttp.Default()
	for _, s := range ips {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("%q 不是合法 IP", s)
		}
		if ip.Unmap().Is4() != want4 {
			return fmt.Errorf("%s 与地址族不符", s)
		}
		if r := bogusPinAddr(ip); r != "" {
			return fmt.Errorf("%s 属于保留/伪造网段 %s，不能作为拨号地址", s, r)
		}
		if err := guard.CheckIP(ip); err != nil {
			return fmt.Errorf("%s 不能作为拨号地址：%v", s, err)
		}
	}
	return nil
}

// resolveRelayHost 解析并过滤结果：只留公网地址，按解析器返回顺序
// 分别保留 v4/v6，去重。域名已经是 IP 字面量时直接用它。
func resolveRelayHost(hostport string, timeout time.Duration) (v4, v6 []string, err error) {
	host := hostport
	if h, _, e := net.SplitHostPort(hostport); e == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return nil, nil, fmt.Errorf("缺少域名")
	}
	guard := safehttp.Default()
	if ip, e := netip.ParseAddr(host); e == nil {
		if e := guard.CheckIP(ip); e != nil {
			return nil, nil, e
		}
		if ip.Unmap().Is4() {
			return []string{ip.Unmap().String()}, nil, nil
		}
		return nil, []string{ip.Unmap().String()}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := relayLookup(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, a := range addrs {
		a = a.Unmap()
		if !a.IsValid() || seen[a.String()] {
			continue
		}
		if r := bogusPinAddr(a); r != "" {
			log.Printf("信任中转 %s 的解析结果 %s 属于保留/伪造网段 %s，拒绝钉住", host, a, r)
			continue
		}
		if e := guard.CheckIP(a); e != nil {
			log.Printf("信任中转 %s 的解析结果被拒绝：%v", host, e)
			continue
		}
		seen[a.String()] = true
		if a.Is4() {
			v4 = append(v4, a.String())
		} else {
			v6 = append(v6, a.String())
		}
	}
	if len(v4)+len(v6) == 0 {
		return nil, nil, fmt.Errorf("%s 没有可用的公网地址", host)
	}
	return v4, v6, nil
}
