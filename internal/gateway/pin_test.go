package gateway

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPinUsableStates(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	cases := []struct {
		name   string
		t      TrustedRelay
		pinned bool
		bad    bool
	}{
		{"expired", TrustedRelay{PinState: "expired"}, false, true},
		{"pending 有地址", TrustedRelay{PinState: "pending", PinIPv4: "1.1.1.1"}, true, false},
		{"confirmed 只有 v6", TrustedRelay{PinState: "confirmed", PinIPv6: "2606:4700::1111"}, true, false},
		{"stale 沿用旧地址", TrustedRelay{PinState: "stale", PinIPv4: "1.1.1.1"}, true, false},
		{"pending 没地址", TrustedRelay{PinState: "pending"}, false, true},
		{"未初始化走旧解析", TrustedRelay{}, false, false},
		{"unresolved 窗口内", TrustedRelay{PinState: "unresolved", GraceEnd: future}, false, false},
		{"unresolved 窗口外", TrustedRelay{PinState: "unresolved", GraceEnd: past}, false, true},
	}
	g := &Gateway{}
	for _, c := range cases {
		pinned, err := g.pinUsable(c.t)
		if pinned != c.pinned || (err != nil) != c.bad {
			t.Fatalf("%s: pinned=%v err=%v", c.name, pinned, err)
		}
	}
}

// 钉住后必须只拨钉住的 IP，域名本身不参与解析。
func TestPinnedClientDialsPinnedIP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无本地监听:", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pinned")
	})}
	go srv.Serve(l)
	defer srv.Close()
	port := l.Addr().(*net.TCPAddr).Port

	relay := TrustedRelay{
		ID: 1, Name: "r", PinHost: "relay.test",
		PinIPv4: "127.0.0.1", PinState: "pending",
	}
	url := "http://relay.test:" + strconv.Itoa(port) + "/f"
	resp, err := pinnedClient(relay).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "pinned" {
		t.Fatalf("正文 %q", b)
	}
}

// 中转把请求指到别的域名时，不能拿钉住的 IP 去连它。
func TestPinnedClientRejectsOtherHost(t *testing.T) {
	relay := TrustedRelay{ID: 1, PinHost: "relay.test", PinIPv4: "127.0.0.1", PinState: "pending"}
	_, err := pinnedClient(relay).Get("http://other.test/f")
	if err == nil || !strings.Contains(err.Error(), "不符") {
		t.Fatalf("应拒绝非钉住域名，实际 %v", err)
	}
}

// IPv4 连不上时按顺序试已钉的 IPv6，不重新解析。
func TestPinnedClientV4FailFallsToV6(t *testing.T) {
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("环境无 IPv6 回环:", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "v6")
	})}
	go srv.Serve(l)
	defer srv.Close()
	port := l.Addr().(*net.TCPAddr).Port

	relay := TrustedRelay{
		ID: 2, Name: "r6", PinHost: "relay.test",
		PinIPv4:  "127.0.0.2", // 本机回环，没有监听 → 立即拒绝
		PinIPv6:  "::1",
		PinState: "confirmed",
	}
	resp, err := pinnedClient(relay).Get("http://relay.test:" + strconv.Itoa(port) + "/f")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "v6" {
		t.Fatalf("正文 %q", b)
	}
}

// SetTrusted 换地址时要丢掉旧的连接池，否则还会拨旧 IP。
func TestSetTrustedDropsPinnedClients(t *testing.T) {
	g := &Gateway{}
	t1 := TrustedRelay{ID: 7, PinHost: "relay.test", PinIPv4: "1.1.1.1", PinState: "pending"}
	g.trusted.Store([]TrustedRelay{t1})
	_ = g.clientFor(t1)
	if _, ok := g.pinClients.Load("7|1.1.1.1|"); !ok {
		t.Fatal("客户端应已缓存")
	}
	g.SetTrusted([]TrustedRelay{t1})
	if _, ok := g.pinClients.Load("7|1.1.1.1|"); ok {
		t.Fatal("热更新后旧客户端应被丢弃")
	}
}

// 钉住的多个 IP 必须一起竞速：串行拨号会被一个丢包的 IP 拖住（实测 v4 卡 1.1s 才轮到 v6）。
// 这里第一个 IP 指向不可路由的 TEST-NET，正常应毫秒级由第二个 IP 接管。
func TestPinnedClientRacesIPs(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})}
	go srv.Serve(l)
	defer srv.Close()

	relay := TrustedRelay{
		ID: 3, Name: "race", PinHost: "relay.test",
		PinIPv4:  "192.0.2.1", // TEST-NET-1，不可路由
		PinIPv6:  "127.0.0.1",
		PinState: "confirmed",
	}
	t0 := time.Now()
	resp, err := pinnedClient(relay).Get("http://relay.test:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port) + "/f")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok" {
		t.Fatalf("正文 %q", b)
	}
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("拨号用了 %v，说明还在串行等第一个 IP", el)
	}
}
