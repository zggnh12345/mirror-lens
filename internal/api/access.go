package api

import (
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/config"
)

// accessPolicy 是解析好的白/黑名单。名单为空表示不启用，行为和以前完全一致。
type accessPolicy struct {
	allow []netip.Prefix
	deny  []netip.Prefix
	scope string
}

// proxyPrefixes 是面向公网客户端的加速路径；scope=panel 时不受名单约束。
var proxyPrefixes = []string{"/raw/", "/gh/", "/http/", "/git/", "/v2/"}

func newAccessPolicy(c *config.Config) *accessPolicy {
	p := &accessPolicy{scope: "panel"}
	if c == nil {
		return p
	}
	p.scope = c.Access.Scope
	if p.scope == "" {
		p.scope = "panel"
	}
	p.allow = parseCIDRList(c.Access.Allow)
	p.deny = parseCIDRList(c.Access.Deny)
	return p
}

func parseCIDRList(list []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(list))
	for _, raw := range list {
		if p, err := config.ParseCIDR(raw); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func (p *accessPolicy) empty() bool { return len(p.allow) == 0 && len(p.deny) == 0 }

// applies 判断路径是否在约束范围内：panel 只护面板与 /api，加速路径照常开放。
func (p *accessPolicy) applies(path string) bool {
	if p.scope == "all" {
		return true
	}
	for _, pre := range proxyPrefixes {
		if strings.HasPrefix(path, pre) {
			return false
		}
	}
	return true
}

// block 判定是否拒绝。回环永远放行，避免把自己锁在门外；
// deny 优先，其次 allow 非空且不命中。
func (p *accessPolicy) block(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return false
	}
	if containsPrefix(p.deny, ip) {
		return true
	}
	return len(p.allow) > 0 && !containsPrefix(p.allow, ip)
}

func containsPrefix(list []netip.Prefix, ip netip.Addr) bool {
	for _, pre := range list {
		if pre.Contains(ip) {
			return true
		}
	}
	return false
}

// access 是入口访问控制中间件。名单为空时直接放行，零开销。
func (s *Server) access(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := s.accessPolicy()
		if p.empty() || !p.applies(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		raw := auth.ClientIP(r)
		ip, err := netip.ParseAddr(raw)
		if err != nil {
			// 非 IP 传输（unix socket）没法判定，按本机处理。
			next.ServeHTTP(w, r)
			return
		}
		if p.block(ip) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "403 来源不在允许范围", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// accessPolicy 取当前配置对应的策略，配置指针没变就复用解析结果。
func (s *Server) accessPolicy() *accessPolicy {
	cfg := s.cfg.Load()
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if s.accessPol != nil && s.accessCfg == cfg {
		return s.accessPol
	}
	s.accessCfg, s.accessPol = cfg, newAccessPolicy(cfg)
	if s.accessPol.blocksEverything() {
		slog.Warn("访问控制黑名单覆盖全部地址：除回环外所有请求都会被拒绝，确认这是你要的")
	}
	return s.accessPol
}

// blocksEverything 用于启动/保存后的显眼警告：白名单为空且黑名单含 /0。
func (p *accessPolicy) blocksEverything() bool {
	if len(p.allow) != 0 {
		return false
	}
	for _, pre := range p.deny {
		if pre.Bits() == 0 {
			return true
		}
	}
	return false
}
