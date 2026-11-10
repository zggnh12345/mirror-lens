package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/config"
)

func policyWith(allow, deny []string, scope string) *accessPolicy {
	return newAccessPolicy(&config.Config{Access: config.Access{Allow: allow, Deny: deny, Scope: scope}})
}

func TestAccessEmptyAllowsAll(t *testing.T) {
	p := policyWith(nil, nil, "all")
	if !p.empty() {
		t.Fatal("空名单应视为未启用")
	}
	if p.block(netip.MustParseAddr("203.0.113.9")) {
		t.Fatal("空名单不该拦人")
	}
}

func TestAccessAllowDeny(t *testing.T) {
	p := policyWith([]string{"10.1.1.0/24", "2001:db8::/32"}, []string{"10.1.1.66"}, "panel")
	cases := []struct {
		ip    string
		block bool
	}{
		{"10.1.1.9", false},
		{"10.1.1.66", true}, // deny 优先
		{"10.2.0.1", true},
		{"2001:db8::1", false},
		{"2001:db9::1", true},
		{"127.0.0.1", false}, // 回环永远放行
		{"::1", false},
	}
	for _, c := range cases {
		if got := p.block(netip.MustParseAddr(c.ip)); got != c.block {
			t.Errorf("%s: 期望 block=%v，得到 %v", c.ip, c.block, got)
		}
	}
}

func TestAccessScope(t *testing.T) {
	panel := policyWith(nil, []string{"0.0.0.0/0"}, "panel")
	if panel.applies("/raw/x") || panel.applies("/v2/") || panel.applies("/gh/o/r") {
		t.Fatal("panel 范围不该约束加速路径")
	}
	if !panel.applies("/api/config") || !panel.applies("/") {
		t.Fatal("panel 范围应约束面板与 /api")
	}
	all := policyWith(nil, []string{"0.0.0.0/0"}, "all")
	if !all.applies("/v2/") {
		t.Fatal("all 范围应约束加速路径")
	}
}

func TestAccessMiddleware(t *testing.T) {
	s := &Server{}
	cfg := config.Default()
	cfg.Access = config.Access{Allow: []string{"10.1.1.0/24"}, Scope: "panel"}
	s.cfg.Store(cfg)

	h := s.access(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	send := func(path, remote string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := send("/api/config", "10.1.1.5:1234"); got != 200 {
		t.Fatalf("白名单内应 200，得到 %d", got)
	}
	if got := send("/api/config", "203.0.113.7:1234"); got != 403 {
		t.Fatalf("白名单外应 403，得到 %d", got)
	}
	if got := send("/raw/x", "203.0.113.7:1234"); got != 200 {
		t.Fatalf("panel 范围下 /raw 应放行，得到 %d", got)
	}
}

func TestParseCIDRSingleIP(t *testing.T) {
	p, err := config.ParseCIDR("192.168.1.10")
	if err != nil || !p.Contains(netip.MustParseAddr("192.168.1.10")) || p.Bits() != 32 {
		t.Fatalf("单 IP 应补 /32，得到 %v %v", p, err)
	}
	if _, err := config.ParseCIDR("10.0.0.0/33"); err == nil {
		t.Fatal("非法网段应报错")
	}
}

func TestAccessPolicyReload(t *testing.T) {
	s := &Server{}
	cfg := config.Default()
	cfg.Access = config.Access{Allow: []string{"10.1.1.0/24"}, Scope: "panel"}
	s.cfg.Store(cfg)
	if !s.accessPolicy().block(netip.MustParseAddr("203.0.113.7")) {
		t.Fatal("白名单外应被拒")
	}
	// 面板保存后 cfg 指针替换，策略应立刻跟着变，不需要重启。
	relaxed := config.Default()
	s.cfg.Store(relaxed)
	if s.accessPolicy().block(netip.MustParseAddr("203.0.113.7")) {
		t.Fatal("清空名单后应立即放行")
	}
}
