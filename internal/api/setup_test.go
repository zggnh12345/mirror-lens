package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func newTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	return newTestServerOpts(t)
}

func newTestServerOpts(t *testing.T, opts ...Option) (*store.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := auth.EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(st, gateway.Options{Cache: cs})
	return st, New(st, runner.New(st), gw, nil, nil, opts...)
}

func postJSON(h http.Handler, path string, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func login(t *testing.T, h http.Handler, user, pass string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	rec := postJSON(h, "/api/login", `{"user":"`+user+`","pass":"`+pass+`"}`, nil)
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			return rec, c
		}
	}
	return rec, nil
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	s, _ := m["code"].(string)
	return s
}

// 默认密码状态下，面板接口一律拒绝，只留 /api/me 和设置本身。
func TestDefaultPasswordBlocksPanel(t *testing.T) {
	_, h := newTestServer(t)

	rec, ck := login(t, h, "admin", "admin")
	if rec.Code != 200 || ck == nil {
		t.Fatalf("默认账号应能登录，得到 %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["must_change"] != true {
		t.Fatalf("登录应告知必须改密：%v", out)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	req.AddCookie(ck)
	r2 := httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != 403 || code(t, r2) != "setup_required" {
		t.Fatalf("未设置账号前 /api/sources 应 403 setup_required，得到 %d %s", r2.Code, r2.Body.String())
	}

	// /api/me 仍然可用：设置页和登录跳转要靠它判断状态，它不泄露任何东西。
	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(ck)
	r2b := httptest.NewRecorder()
	h.ServeHTTP(r2b, req)
	if r2b.Code != 200 || !strings.Contains(r2b.Body.String(), `"must_change":true`) {
		t.Fatalf("/api/me 应可用并报告 must_change，得到 %d %s", r2b.Code, r2b.Body.String())
	}

	// 页面请求跳设置页，而不是给一个能点掉的面板。
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(ck)
	r3 := httptest.NewRecorder()
	h.ServeHTTP(r3, req)
	if r3.Code != http.StatusFound || r3.Header().Get("Location") != "/setup" {
		t.Fatalf("应跳到 /setup，得到 %d %q", r3.Code, r3.Header().Get("Location"))
	}
}

func TestSetupRejectsWeakInput(t *testing.T) {
	st, h := newTestServer(t)
	_, ck := login(t, h, "admin", "admin")

	cases := []struct{ body, want string }{
		{`{"user":"ab","pass":"abcdefgh"}`, "用户名"},
		{`{"user":"alex","pass":"short"}`, "至少 8 位"},
		{`{"user":"alex","pass":"admin"}`, "至少 8 位"},
	}
	for _, c := range cases {
		rec := postJSON(h, "/api/setup", c.body, ck)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("%s: 期望 400 含 %q，得到 %d %s", c.body, c.want, rec.Code, rec.Body.String())
		}
	}
	// 失败不应留下半个账号：还是默认密码状态。
	if !auth.Check(st, "admin", "admin") {
		t.Fatalf("失败的设置不该改掉账号")
	}
}

// 并发点「保存」：只能有一个成功，其余 409；最终账号必须是赢家的那一组。
func TestSetupConcurrentOnlyOneWins(t *testing.T) {
	_, h := newTestServer(t)
	_, ck := login(t, h, "admin", "admin")

	const n = 6
	type res struct {
		code int
		body string
	}
	out := make([]res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := "racer" + string(rune('a'+i))
			rec := postJSON(h, "/api/setup", `{"user":"`+user+`","pass":"pass-`+user+`"}`, ck)
			out[i] = res{rec.Code, rec.Body.String()}
		}(i)
	}
	wg.Wait()

	ok := 0
	winner := ""
	for i, r := range out {
		switch r.code {
		case 200:
			ok++
			winner = "racer" + string(rune('a'+i))
		case 409:
		default:
			t.Fatalf("第 %d 个请求应 200 或 409，得到 %d %s", i, r.code, r.body)
		}
	}
	if ok != 1 {
		t.Fatalf("应恰好一个成功，实际 %d 个：%v", ok, out)
	}
	if rec, _ := login(t, h, winner, "pass-"+winner); rec.Code != 200 {
		t.Fatalf("赢家应能登录，得到 %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < n; i++ {
		u := "racer" + string(rune('a'+i))
		if u == winner {
			continue
		}
		if rec, _ := login(t, h, u, "pass-"+u); rec.Code != 401 {
			t.Fatalf("输家 %q 不该能登录，得到 %d", u, rec.Code)
		}
	}
}

func TestMaskUser(t *testing.T) {
	cases := map[string]string{
		"":      "(空)",
		"a":     "a***(1)",
		"admin": "a***(5)",
		"张三丰":   "张***(3)",
	}
	for in, want := range cases {
		if got := maskUser(in); got != want {
			t.Errorf("maskUser(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestSetupThenPanelOpens(t *testing.T) {
	_, h := newTestServer(t)
	_, ck := login(t, h, "admin", "admin")

	rec := postJSON(h, "/api/setup", `{"user":"alex","pass":"correct-horse"}`, ck)
	if rec.Code != 200 {
		t.Fatalf("设置应成功，得到 %d %s", rec.Code, rec.Body.String())
	}
	var newCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			newCookie = c
		}
	}
	if newCookie == nil {
		t.Fatalf("设置成功后应换发新会话")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	req.AddCookie(newCookie)
	r2 := httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != 200 {
		t.Fatalf("设置完面板应可用，得到 %d %s", r2.Code, r2.Body.String())
	}

	// 旧会话作废，旧密码失效，新密码可登录且不再要求改密。
	req = httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	req.AddCookie(ck)
	r3 := httptest.NewRecorder()
	h.ServeHTTP(r3, req)
	if r3.Code != 401 {
		t.Fatalf("设置前的会话应失效，得到 %d", r3.Code)
	}
	if rec, _ := login(t, h, "admin", "admin"); rec.Code != 401 {
		t.Fatalf("默认密码应失效，得到 %d", rec.Code)
	}
	rec4, _ := login(t, h, "alex", "correct-horse")
	if rec4.Code != 200 || strings.Contains(rec4.Body.String(), `"must_change":true`) {
		t.Fatalf("新账号应能登录且不再要求改密：%d %s", rec4.Code, rec4.Body.String())
	}

	// 已经设置过就不能再走这个口子。
	if rec := postJSON(h, "/api/setup", `{"user":"other","pass":"another-pass"}`, newCookie); rec.Code != 409 {
		t.Fatalf("重复设置应 409，得到 %d", rec.Code)
	}
}

// 首次进入不需要先用默认密码登录：needSetup 期间 /setup 与 /login 都直接给设置页，
// 匿名直接 POST /api/setup 即可设账号，设完后面板可用、默认密码失效，重复设置返回 409。
func TestFirstRunSetupNoPriorLogin(t *testing.T) {
	_, h := newTestServer(t)

	// 首次进入根路径应直接引到设置页，而不是被登录校验挡到 /login。
	rootReq := httptest.NewRequest(http.MethodGet, "/", nil)
	rRoot := httptest.NewRecorder()
	h.ServeHTTP(rRoot, rootReq)
	if rRoot.Code != http.StatusFound || rRoot.Header().Get("Location") != "/setup" {
		t.Fatalf("首次 GET / 应跳到 /setup，得到 %d %q", rRoot.Code, rRoot.Header().Get("Location"))
	}

	// needSetup 期间，匿名直接打开 /setup 拿到设置页 HTML。
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	rSetup := httptest.NewRecorder()
	h.ServeHTTP(rSetup, req)
	if rSetup.Code != 200 || !strings.Contains(rSetup.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/setup 应对匿名返回设置页，得到 %d %q", rSetup.Code, rSetup.Header().Get("Content-Type"))
	}

	// needSetup 期间，/login 也直接给设置页（而不是登录页）。
	req = httptest.NewRequest(http.MethodGet, "/login", nil)
	rLogin := httptest.NewRecorder()
	h.ServeHTTP(rLogin, req)
	if rLogin.Code != 200 || !strings.Contains(rLogin.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/login 在首次期间应给设置页，得到 %d %q", rLogin.Code, rLogin.Header().Get("Content-Type"))
	}

	// 没登录也能设账号（needSetup 期间 /api/setup 对匿名开放）。
	rec := postJSON(h, "/api/setup", `{"user":"boss","pass":"super-secret"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("首次设置应成功（匿名），得到 %d %s", rec.Code, rec.Body.String())
	}
	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatalf("设置成功应发新会话")
	}

	// 设完后面板可用。
	req = httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	req.AddCookie(ck)
	r2 := httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != 200 {
		t.Fatalf("设置完面板应可用，得到 %d %s", r2.Code, r2.Body.String())
	}

	// 默认密码应已失效。
	if rec, _ := login(t, h, "admin", "admin"); rec.Code != 401 {
		t.Fatalf("默认密码应失效，得到 %d", rec.Code)
	}

	// needSetup 之后 /setup 跳回面板（不再给设置页）。
	req = httptest.NewRequest(http.MethodGet, "/setup", nil)
	r3 := httptest.NewRecorder()
	h.ServeHTTP(r3, req)
	if r3.Code != http.StatusFound {
		t.Fatalf("设过之后 /setup 应跳回面板，得到 %d", r3.Code)
	}

	// 已经设过就不能再走这个口子（匿名也不行）。
	if rec := postJSON(h, "/api/setup", `{"user":"other","pass":"another-pass"}`, nil); rec.Code != 409 {
		t.Fatalf("重复设置应 409，得到 %d", rec.Code)
	}
}
