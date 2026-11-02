package api

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// sessionToken 取当前会话令牌，没有就空串。
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// externalOrigin 拼出这次请求对外的源（协议 + 主机 + 端口）。
//
// 反代后面 r.TLS 是 nil、r.Host 也可能被改写成上游地址，所以只有在
// 对端是可信反代时才采信 X-Forwarded-Proto / X-Forwarded-Host；
// 不可信对端塞的转发头一律忽略，否则伪造一个 Origin 就等于绕过校验。
func externalOrigin(r *http.Request) *url.URL {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if auth.IsTrustedProxy(r) {
		if p := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); p == "http" || p == "https" {
			scheme = p
		}
		if h := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); h != "" {
			host = h
		}
	}
	return &url.URL{Scheme: scheme, Host: host}
}

// portOf 取有效端口：显式端口优先，否则按协议取默认端口。
// 比较 http://a:80 与 http://a 时必须相等，否则同源会被误判成跨站。
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// sameOrigin 比较协议、主机、端口三项，缺一不可。
func sameOrigin(o string, r *http.Request) bool {
	u, err := url.Parse(strings.TrimSpace(o))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return false
	}
	w := externalOrigin(r)
	return strings.EqualFold(u.Scheme, w.Scheme) &&
		strings.EqualFold(u.Hostname(), w.Hostname()) &&
		portOf(u) == portOf(w)
}

// crossSite 判断请求是不是别的站点发来的。任一命中即拒绝：
//   - Origin 存在时必须与本站同源（协议/主机/端口），"null" 也拒；
//   - Sec-Fetch-Site 存在时必须是 same-origin / none。
//
// 这两个头老浏览器可能没有，所以真正的兜底是下面的 token。
func crossSite(r *http.Request) bool {
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
		if !sameOrigin(o, r) {
			return true
		}
	}
	switch sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs {
	case "", "none", "same-origin":
		return false
	default:
		return true
	}
}

// csrfOK 校验写操作。token 按会话派生，攻击者拿不到也猜不出。
func (s *Server) csrfOK(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if crossSite(r) {
		return false
	}
	want := auth.CSRF(s.st, sessionToken(r))
	got := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// audit 记一条管理动作。detail 只放非敏感摘要。
func (s *Server) audit(r *http.Request, action, target, detail string, ok bool) {
	runeDetail := []rune(detail)
	if len(runeDetail) > 200 {
		detail = string(runeDetail[:200])
	}
	s.st.Audit(store.AuditEvent{
		TS:     time.Now().Unix(),
		IP:     auth.ClientIP(r),
		User:   s.st.GetSetting(auth.KeyUser, auth.DefaultUser),
		Action: action,
		Target: target,
		Detail: detail,
		OK:     ok,
	})
}

// auditAs 用于登录这类「还没登录成功」的场景：用户是尝试值，不是当前账号。
func (s *Server) auditAs(r *http.Request, user, action, target, detail string, ok bool) {
	runeDetail := []rune(detail)
	if len(runeDetail) > 200 {
		detail = string(runeDetail[:200])
	}
	s.st.Audit(store.AuditEvent{
		TS: time.Now().Unix(), IP: auth.ClientIP(r), User: user,
		Action: action, Target: target, Detail: detail, OK: ok,
	})
}

// auditList 返回最近的管理动作。
func (s *Server) auditList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, 405, map[string]string{"error": "方法不对"})
		return
	}
	n := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 && x <= 500 {
			n = x
		}
	}
	list, err := s.st.ListAudit(n)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"events": list})
}
