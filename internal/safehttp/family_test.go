package safehttp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOrderIPs(t *testing.T) {
	v4 := netip.MustParseAddr("1.1.1.1")
	v4b := netip.MustParseAddr("8.8.8.8")
	v6 := netip.MustParseAddr("2606:4700::1111")
	in := []netip.Addr{v6, v4, v4b}

	cases := []struct {
		mode string
		want []netip.Addr
	}{
		{"auto", []netip.Addr{v4, v4b, v6}}, // v4 优先，v6 兜底
		{"", []netip.Addr{v4, v4b, v6}},
		{"v4", []netip.Addr{v4, v4b}},
		{"v6", []netip.Addr{v6}},
	}
	for _, c := range cases {
		g := New(Options{IPFamily: c.mode})
		got := g.orderIPs(in)
		if len(got) != len(c.want) {
			t.Fatalf("mode=%q got %v want %v", c.mode, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("mode=%q got %v want %v", c.mode, got, c.want)
			}
		}
	}
	// 输入不能被就地打乱：调用方可能复用切片。
	if in[0] != v6 {
		t.Fatalf("orderIPs 改了入参: %v", in)
	}
	// 只有 v6 时 auto 也要能拨出去（v4 为空不算错）。
	if got := New(Options{IPFamily: "auto"}).orderIPs([]netip.Addr{v6}); len(got) != 1 || got[0] != v6 {
		t.Fatalf("auto 纯 v6: %v", got)
	}
	// 只要 v4 但域名只有 AAAA：返回空，由调用方报错，而不是偷偷用 v6。
	if got := New(Options{IPFamily: "v4"}).orderIPs([]netip.Addr{v6}); len(got) != 0 {
		t.Fatalf("v4 模式不该返回 v6: %v", got)
	}
}

// auto 模式把域名交给拨号器竞速：能连通、且仍然过 Control 的内网校验。
func TestDialAutoHostnameRespectsGuard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("本机不能监听: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()

	// 放开内网例外：auto 拨 localhost 应能连上（v6 若无监听，竞速会落到 v4）。
	ok := New(Options{AllowPrivate: true, DialTimeout: 3 * time.Second})
	c, err := ok.dialContext(context.Background(), "tcp", "localhost:"+strings.Split(addr, ":")[1])
	if err != nil {
		t.Fatalf("auto 拨 localhost 失败: %v", err)
	}
	c.Close()

	// 不放开：同样的域名必须被 Control 拦下，说明竞速没绕过 SSRF 校验。
	strict := New(Options{DialTimeout: 3 * time.Second})
	if _, err := strict.dialContext(context.Background(), "tcp", "localhost:"+strings.Split(addr, ":")[1]); err == nil {
		t.Fatal("auto 模式下内网地址应被拦")
	} else if !errors.Is(err, ErrBlocked) {
		t.Fatalf("应返回 ErrBlocked，得到 %v", err)
	}
}

// 手动实测用：RACE_TARGET=host:port 时真拨一次，打印耗时。
// 配合双栈 docker 网络（v6 地址无人应答、v4 正常）验证竞速是否生效。
func TestDialRaceManual(t *testing.T) {
	target := os.Getenv("RACE_TARGET")
	if target == "" {
		t.Skip("设置 RACE_TARGET 后手动实测")
	}
	g := New(Options{AllowPrivate: true, DialTimeout: 10 * time.Second})
	start := time.Now()
	c, err := g.dialContext(context.Background(), "tcp", target)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	el := time.Since(start)
	la := c.LocalAddr().String()
	c.Close()
	t.Logf("dial %s -> %v local=%s", target, el.Round(time.Millisecond), la)
}
