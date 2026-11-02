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

// crossSite 判断请求是不是别的站点发来的。任一命中即拒绝：
//   - Origin 存在时必须和本站同源（"null" 也拒）；
//   - Sec-Fetch-Site 存在时必须是 same-origin / none。
//
// 这两个头老浏览器可能没有，所以真正的兜底是下面的 token。
func crossSite(r *http.Request) bool {
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			return true
		}
		if !strings.EqualFold(u.Host, r.Host) {
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
