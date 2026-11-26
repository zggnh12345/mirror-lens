// P2-2 复核修复的 RED 用例：F2 Pin 回落强制严格 / F3 Origin 按请求隔离。
// 这些用例在修复前必须失败（先 RED 再 GREEN）。
package gateway

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/safehttp"
)

// F2-RED：即使 Gateway 的 Guard 开了 AllowPrivate + AllowLoopback，
// Pin 回落客户端也必须严格 public-only，拒绝 127.0.0.1 与 10/8。
func TestFallbackClientIgnoresGuardRelaxationRED(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无本地监听:", err)
	}
	var hit atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		io.WriteString(w, "should-not-happen")
	})}
	go srv.Serve(l)
	defer srv.Close()
	port := l.Addr().(*net.TCPAddr).Port

	// 直接构造带放宽 Guard 的 Gateway（包内可见字段），模拟有人给 gateway 配了内网权限。
	gw := &Gateway{guard: safehttp.New(safehttp.Options{
		AllowPrivate: true, AllowLoopback: true,
	})}
	cli := gw.fallbackClient()

	// 127.0.0.1 字面量：StrictClient 的 CheckRedirect/CheckIP 应拦。
	u := "http://127.0.0.1:" + strconv.Itoa(port) + "/f"
	_, err = cli.Get(u)
	if !errors.Is(err, safehttp.ErrBlocked) && err == nil {
		t.Fatalf("放宽 Guard 下回落仍应拒绝 127.0.0.1，实际 err=%v", err)
	}
	if hit.Load() {
		t.Fatal("放宽 Guard 下回落不应命中回环 listener")
	}

	// 10/8 字面量同理（不需要 listener，拨号前就应被拦）。
	_, err = cli.Get("http://10.0.0.1/f")
	if err == nil {
		t.Fatal("放宽 Guard 下回落应拒绝 10/8，实际成功")
	}
}

// F3-RED：同一个 pinnedClient 连续处理两个不同初始端口的请求，
// 各自的 Redirect Origin 判断必须互不影响（不能用客户端实例级状态缓存 Origin）。
func TestPinnedClientOriginPerRequestRED(t *testing.T) {
	allowPinnedAddrsForTest(t) // 允许回环 listener，只影响连接语义，不影响 Origin 判断

	newOriginServer := func() (string, func()) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skip("无本地监听:", err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/f" {
				http.Redirect(w, r, "/ok", http.StatusFound) // 同 Origin 重定向
				return
			}
			io.WriteString(w, "ok")
		})}
		go srv.Serve(l)
		port := l.Addr().(*net.TCPAddr).Port
		return "http://relay.test:" + strconv.Itoa(port) + "/f", func() { srv.Close() }
	}

	u1, c1 := newOriginServer()
	defer c1()
	u2, c2 := newOriginServer()
	defer c2()

	relay := TrustedRelay{
		ID: 1, PinHost: "relay.test",
		PinIPv4: "127.0.0.1", PinState: "pending",
	}
	cli := pinnedClient(relay) // 同一个 Client

	// 第一次请求建立某端口的 Origin 基准。
	resp1, err := cli.Get(u1)
	if err != nil {
		t.Fatalf("第一次同 Origin 重定向应成功，实际 %v", err)
	}
	resp1.Body.Close()

	// 第二次请求用另一个初始端口；其同 Origin 重定向不应被第一次的端口污染。
	resp2, err := cli.Get(u2)
	if err != nil {
		t.Fatalf("第二次请求不应受第一次 Origin 基准影响，实际 %v", err)
	}
	resp2.Body.Close()

	// 上面两个 URL 都是 127.0.0.1 监听、relay PinHost=relay.test，
	// 服务端重定向到相对路径（保留 relay.test:port 同 Origin），因此成功即证明
	// Origin 判断按请求隔离，而不是碰巧失败。
}
