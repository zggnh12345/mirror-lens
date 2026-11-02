package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func post(h http.Handler, path, body string, hdr map[string]string, ck *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func put(h http.Handler, path, body string, hdr map[string]string, ck *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// signedIn 把默认账号改成测试密码并登录，返回会话 cookie 和 CSRF token。
func signedIn(t *testing.T, st *store.Store, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	if err := auth.SetPassword(st, "test-pass-123"); err != nil {
		t.Fatal(err)
	}
	rec := post(h, "/api/login", `{"user":"admin","pass":"test-pass-123"}`, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("登录应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("登录没发会话 cookie")
	}
	me := get(h, "/api/me", nil, ck)
	var out struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &out); err != nil || out.CSRF == "" {
		t.Fatalf("me 应带 csrf：%s", me.Body.String())
	}
	return ck, out.CSRF
}

// 写接口没有 token 一律 403；带上正确 token 才放行。
func TestCSRFRequiredForWrites(t *testing.T) {
	st, h := newAuthServer(t, false)
	ck, tok := signedIn(t, st, h)

	if r := put(h, "/api/settings", `{"l1_minutes":7}`, nil, ck); r.Code != 403 {
		t.Fatalf("无 token 应 403，得到 %d", r.Code)
	}
	if r := put(h, "/api/settings", `{"l1_minutes":7}`, map[string]string{"X-CSRF-Token": "forged"}, ck); r.Code != 403 {
		t.Fatalf("伪造 token 应 403，得到 %d", r.Code)
	}
	if got := st.GetSetting("l1_minutes", ""); got != "" {
		t.Fatalf("被拒的请求不该落库，得到 %q", got)
	}
	if r := put(h, "/api/settings", `{"l1_minutes":7}`, map[string]string{"X-CSRF-Token": tok}, ck); r.Code != 200 {
		t.Fatalf("正确 token 应 200，得到 %d %s", r.Code, r.Body.String())
	}
	if got := st.GetSetting("l1_minutes", ""); got != "7" {
		t.Fatalf("设置没写进去：%q", got)
	}
	// GET 不需要 token，也不能改状态。
	if r := get(h, "/api/settings", nil, ck); r.Code != 200 {
		t.Fatalf("GET 不该要 token，得到 %d", r.Code)
	}
}

// 跨站来源直接拒：不依赖浏览器发不发 token。
func TestCSRFRejectsCrossOrigin(t *testing.T) {
	st, h := newAuthServer(t, false)
	ck, tok := signedIn(t, st, h)

	evil := map[string]string{"X-CSRF-Token": tok, "Origin": "https://evil.example"}
	if r := put(h, "/api/settings", `{"l1_minutes":9}`, evil, ck); r.Code != 403 {
		t.Fatalf("跨站 Origin 应 403，得到 %d", r.Code)
	}
	same := map[string]string{"X-CSRF-Token": tok, "Sec-Fetch-Site": "same-origin"}
	if r := put(h, "/api/settings", `{"l1_minutes":9}`, same, ck); r.Code != 200 {
		t.Fatalf("同站应 200，得到 %d %s", r.Code, r.Body.String())
	}
	// 登录口没有会话，只能靠来源判断。
	if r := post(h, "/api/login", `{"user":"admin","pass":"x"}`, map[string]string{"Origin": "https://evil.example"}, nil); r.Code != 403 {
		t.Fatalf("跨站登录应 403，得到 %d", r.Code)
	}
}

// 反代场景：TLS 在前面终止，r.TLS 是 nil，Origin 却是 https。
// 只有对端是可信任的反代时才采信 X-Forwarded-Proto/Host，否则就是白送一个绕过口。
func TestCSRFOriginBehindTrustedProxy(t *testing.T) {
	st, h := newAuthServer(t, false)
	ck, tok := signedIn(t, st, h)
	defer auth.SetTrustedProxies(nil)
	auth.SetTrustedProxies([]string{"10.0.0.0/8"})

	with := func(kv map[string]string) map[string]string {
		m := map[string]string{"X-CSRF-Token": tok}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	req := func(origin, xfp, xfh, remote, host string, want int) {
		t.Helper()
		hdr := with(map[string]string{"Origin": origin})
		if xfp != "" {
			hdr["X-Forwarded-Proto"] = xfp
		}
		if xfh != "" {
			hdr["X-Forwarded-Host"] = xfh
		}
		if got := putRaw(h, "/api/settings", `{"l1_minutes":11}`, hdr, remote, host, ck); got != want {
			t.Fatalf("Origin=%s xfp=%s xfh=%s remote=%s host=%s 应为 %d，得到 %d",
				origin, xfp, xfh, remote, host, want, got)
		}
	}

	// 反代后面：协议和主机都从可信转发头还原，端口一致 → 放行。
	req("https://ml.xnat.cc:3333", "https", "ml.xnat.cc:3333", "10.1.1.5:51000", "mirror-lens:8080", 200)
	// 端口不同 → 跨站。
	req("https://ml.xnat.cc:8443", "https", "ml.xnat.cc:3333", "10.1.1.5:51000", "mirror-lens:8080", 403)
	// 协议不同 → 跨站（http 页面伪造 https Origin 也算）。
	req("http://ml.xnat.cc:3333", "https", "ml.xnat.cc:3333", "10.1.1.5:51000", "mirror-lens:8080", 403)
	// 不可信对端自带转发头：一律忽略，伪造 Origin 仍然拦下。
	req("https://evil.example", "https", "evil.example", "203.0.113.5:40000", "mirror-lens:8080", 403)
	// 默认端口归一：http://a:80 与 http://a 同源。
	req("http://ml.example.com:80", "", "", "203.0.113.5:40000", "ml.example.com", 200)
}

func putRaw(h http.Handler, path, body string, hdr map[string]string, remote, host string, ck *http.Cookie) int {
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if remote != "" {
		req.RemoteAddr = remote
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// 探测和发现是写操作，GET 不许触发。
func TestProbeRequiresPost(t *testing.T) {
	st, h := newAuthServer(t, false)
	ck, _ := signedIn(t, st, h)
	if r := get(h, "/api/probe", nil, ck); r.Code != 405 {
		t.Fatalf("GET /api/probe 应 405，得到 %d", r.Code)
	}
	if r := get(h, "/api/discover", nil, ck); r.Code != 405 {
		t.Fatalf("GET /api/discover 应 405，得到 %d", r.Code)
	}
}

// 审计异步落库：登录失败要留痕，且不含密码。
func TestAuditRecordsLoginFailure(t *testing.T) {
	st, h := newAuthServer(t, false)
	if r := post(h, "/api/login", `{"user":"admin","pass":"wrong-pass"}`, nil, nil); r.Code != 401 {
		t.Fatalf("错误密码应 401，得到 %d", r.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := st.ListAudit(20)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list {
			if e.Action == "login" && !e.OK {
				if strings.Contains(e.Detail, "wrong-pass") {
					t.Fatal("审计里出现了密码明文")
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("登录失败没有写审计")
}
