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

// PIN-10：Pin 的拨号层必须是严格 public-only；回环 pin 不得建立连接。
func TestPinnedClientRejectsLoopbackPin(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无本地监听:", err)
	}
	var hit atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		io.WriteString(w, "pinned")
	})}
	go srv.Serve(l)
	defer srv.Close()
	port := l.Addr().(*net.TCPAddr).Port

	relay := TrustedRelay{
		ID: 1, PinHost: "relay.test",
		PinIPv4: "127.0.0.1", PinState: "pending",
	}
	_, err = pinnedClient(relay).Get("http://relay.test:" + strconv.Itoa(port) + "/f")
	if !errors.Is(err, safehttp.ErrBlocked) {
		t.Fatalf("回环 pin 应返回 ErrBlocked，实际 %v", err)
	}
	if hit.Load() {
		t.Fatal("回环 pin 不应建立连接")
	}
}

// PIN-14：同 Host 不同端口属于 Origin 变化，必须在 CheckRedirect 阶段拒绝。
func TestPinnedClientRejectsSameHostDifferentPortRedirect(t *testing.T) {
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无本地监听:", err)
	}
	var secondHit atomic.Bool
	srv2 := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit.Store(true)
		io.WriteString(w, "second")
	})}
	go srv2.Serve(l2)
	defer srv2.Close()
	port2 := l2.Addr().(*net.TCPAddr).Port

	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无本地监听:", err)
	}
	srv1 := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://relay.test:"+strconv.Itoa(port2)+"/next", http.StatusFound)
	})}
	go srv1.Serve(l1)
	defer srv1.Close()
	port1 := l1.Addr().(*net.TCPAddr).Port

	relay := TrustedRelay{
		ID: 1, PinHost: "relay.test",
		PinIPv4: "127.0.0.1", PinState: "pending",
	}
	_, err = pinnedClient(relay).Get("http://relay.test:" + strconv.Itoa(port1) + "/f")
	if !errors.Is(err, safehttp.ErrBlocked) {
		t.Fatalf("同 Host 异端口重定向应返回 ErrBlocked，实际 %v", err)
	}
	if secondHit.Load() {
		t.Fatal("不得产生第二次拨号")
	}
}

// PIN-15：不同 Host 重定向必须在 CheckRedirect 阶段拒绝，而不是靠 Dial 阶段碰巧失败。
func TestPinnedClientRejectsOtherHostRedirect(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无本地监听:", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://other.test/next", http.StatusFound)
	})}
	go srv.Serve(l)
	defer srv.Close()
	port := l.Addr().(*net.TCPAddr).Port

	relay := TrustedRelay{
		ID: 1, PinHost: "relay.test",
		PinIPv4: "127.0.0.1", PinState: "pending",
	}
	_, err = pinnedClient(relay).Get("http://relay.test:" + strconv.Itoa(port) + "/f")
	if !errors.Is(err, safehttp.ErrBlocked) {
		t.Fatalf("跨 Host 重定向应返回 ErrBlocked，实际 %v", err)
	}
}
