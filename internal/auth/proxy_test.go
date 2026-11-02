package auth

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// Traefik 后面所有登录都来自同一个容器地址：不信转发头的话，
// 一个公网爆破者就能把「所有 IP」的额度吃完，管理员被锁在门外。
func TestClientIPUsesForwardedForOnlyBehindTrustedProxy(t *testing.T) {
	SetTrustedProxies([]string{"10.1.1.0/24"})
	defer SetTrustedProxies(nil)

	behind := func(xff string) string {
		r := httptest.NewRequest("POST", "/api/login", nil)
		r.RemoteAddr = "10.1.1.5:5000"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return ClientIP(r)
	}
	if got := behind("1.2.3.4, 10.1.1.7"); got != "1.2.3.4" {
		t.Fatalf("应取右边第一个非可信地址，得到 %q", got)
	}
	// 最左是攻击者随手写的，右边才是真实跳数
	if got := behind("9.9.9.9, 8.8.8.8"); got != "8.8.8.8" {
		t.Fatalf("应取 8.8.8.8，得到 %q", got)
	}
	// 全链可信：退回对端，不要拿一个可信地址当客户端
	if got := behind("10.1.1.6"); got != "10.1.1.5" {
		t.Fatalf("全可信时应退回对端，得到 %q", got)
	}

	// 不可信对端自带 XFF：一律忽略，否则限流可被绕过
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = "8.8.8.8:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(r); got != "8.8.8.8" {
		t.Fatalf("不可信对端不该改判 IP，得到 %q", got)
	}
}

func TestSecureCookieFlag(t *testing.T) {
	st := newStore(t)
	SetTrustedProxies([]string{"10.1.1.0/24"})
	defer SetTrustedProxies(nil)

	cases := []struct {
		name string
		tls  bool
		xff  string
		want bool
	}{
		{"反代声明 https", false, "https", true},
		{"反代声明 http", false, "http", false},
		{"直连 TLS", true, "", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/api/login", nil)
		r.RemoteAddr = "10.1.1.5:5000"
		if c.xff != "" {
			r.Header.Set("X-Forwarded-Proto", c.xff)
		}
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		w := httptest.NewRecorder()
		if err := Login(w, r, st); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		ck := w.Result().Cookies()[0]
		if ck.Secure != c.want {
			t.Fatalf("%s: Secure=%v 期望 %v", c.name, ck.Secure, c.want)
		}
		if !ck.HttpOnly {
			t.Fatalf("%s: HttpOnly 丢了", c.name)
		}
	}

	// 显式关闭：即使内网反代说 https 也不加 Secure，留给纯 HTTP 部署。
	if err := st.SetSetting("cookie_secure", "0"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = "10.1.1.5:5000"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	if err := Login(w, r, st); err != nil {
		t.Fatal(err)
	}
	if w.Result().Cookies()[0].Secure {
		t.Fatal("cookie_secure=0 时应关闭 Secure")
	}
}
