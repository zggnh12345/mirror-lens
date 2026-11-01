package safehttp

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 第一层：地址分类。真实风险在拨号阶段，但分类错了后面全错。
func TestCheckIPBlocksMoreRanges(t *testing.T) {
	g := New(Options{})
	blocked := []string{
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "127.0.0.1", "0.0.0.0",
		"100.64.0.1", "169.254.169.254", "224.0.0.1", "255.255.255.255",
		"::1", "fc00::1", "fd12:3456::1", "fe80::1", "ff02::1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1",
	}
	for _, s := range blocked {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("解析 %s: %v", s, err)
		}
		if err := g.CheckIP(ip); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s 应被拦，实际 %v", s, err)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888"}
	for _, s := range allowed {
		ip, _ := netip.ParseAddr(s)
		if err := g.CheckIP(ip); err != nil {
			t.Errorf("%s 不该被拦: %v", s, err)
		}
	}
}

func TestCheckURLRejectsSchemesAndCredentials(t *testing.T) {
	g := New(Options{})
	cases := []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/x",
		"http://user:pass@example.com/",
		"http:///nohost",
		"http://[::1]/",
		"http://127.0.0.1/",
	}
	for _, raw := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if err := g.Check(u); err == nil {
			t.Errorf("%s 应被拒", raw)
		}
	}
	if err := g.Check(nil); err == nil {
		t.Error("nil URL 应被拒")
	}
	if err := g.Check(mustURL(t, "https://example.com/a")); err != nil {
		t.Errorf("正常 URL 不该被拒: %v", err)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 拨号层看的是「真正要连的地址」，不是 URL 里的主机名。
// DNS 返回多个地址、或者解析后又变（rebinding），每一个候选地址都要过这一关。
func TestDialControlChecksResolvedAddress(t *testing.T) {
	g := New(Options{})
	ctl := g.Dialer().Control

	if err := ctl("tcp", "1.1.1.1:443", nil); err != nil {
		t.Fatalf("公网地址不该被拦: %v", err)
	}
	// 同一个域名，DNS 同时给出公网和内网两个答案：内网那个必须连不上。
	for _, addr := range []string{"10.0.0.7:443", "[fd00::7]:443", "[::ffff:192.168.1.7]:443"} {
		if err := ctl("tcp", addr, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s 应被拦，实际 %v", addr, err)
		}
	}
	// 非 IP 字面量不该走到这里；真到了也不能放行。
	if err := ctl("tcp", "example.com:443", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("主机名应被拒，实际 %v", err)
	}
}

// 第二层：整条链路。域名形式的回环地址在 Check 阶段看不出来，
// 只能靠拨号层拦——这正是 DNS rebinding 的防线。
func TestDialLayerBlocksLoopbackHostname(t *testing.T) {
	if _, err := net.LookupHost("localhost"); err != nil {
		t.Skip("本机解析不了 localhost")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should not reach"))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL) // http://127.0.0.1:PORT
	port := u.Port()

	g := New(Options{DialTimeout: 2 * time.Second})
	resp, err := g.Client().Get("http://localhost:" + port + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("localhost 应被拨号层拦下")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("应是策略拦截，实际 %v", err)
	}
}

// 重定向到内网 IP 字面量：每一跳都重新校验。
func TestRedirectToPrivateBlocked(t *testing.T) {
	targets := []string{"http://10.255.255.1/x", "http://[fd00::1]/x", "http://169.254.169.254/latest/meta-data/"}
	for _, target := range targets {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusFound)
		}))

		// 首跳在回环上，必须放行才能走到重定向；目标是内网字面量，CheckRedirect 拦。
		g := New(Options{AllowLoopback: true, DialTimeout: 2 * time.Second})
		resp, err := g.Client().Get(srv.URL + "/")
		srv.Close()
		if err == nil {
			resp.Body.Close()
			t.Fatalf("重定向到 %s 应被拦", target)
		}
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("重定向到 %s 应是策略拦截，实际 %v", target, err)
		}
	}
}

// 重定向次数上限。
func TestRedirectLimit(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	g := New(Options{AllowLoopback: true, MaxRedirects: 3, DialTimeout: 2 * time.Second})
	resp, err := g.Client().Get(srv.URL + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("无限重定向应报错")
	}
	if errors.Is(err, ErrBlocked) {
		t.Fatalf("应是次数超限，不是策略拦截: %v", err)
	}
	if !strings.Contains(err.Error(), "重定向超过") {
		t.Fatalf("错误信息应说明次数超限: %v", err)
	}
}
