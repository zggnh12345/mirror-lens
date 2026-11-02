// Package auth handles the single admin account, session cookies and login throttling.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

const (
	CookieName    = "ml_session"
	DefaultUser   = "admin"
	DefaultPass   = "admin"
	KeyUser       = "admin_user"
	KeyHash       = "admin_password_hash"
	KeyMustChg    = "must_change_password"
	KeySessionTTL = "session_ttl_hours"
	KeyCSRFSecret = "csrf_secret"
)

// EnsureAdmin 首次启动写入默认账号 admin/admin，并标记需改密。
func EnsureAdmin(st *store.Store) error {
	if st.GetSetting(KeyHash, "") != "" {
		return nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(DefaultPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := st.SetSetting(KeyUser, DefaultUser); err != nil {
		return err
	}
	if err := st.SetSetting(KeyHash, string(h)); err != nil {
		return err
	}
	return st.SetSetting(KeyMustChg, "1")
}

// ErrSetupDone 表示首次设置已被别人做完。
var ErrSetupDone = errors.New("账号已经设置过了")

// Setup 在同一事务里认领 must_change、写用户名和密码。
// 认领是条件 UPDATE，并发只有一个成功，失败者整体回滚。
func Setup(st *store.Store, user, pass string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	err = st.Tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE settings SET value='0' WHERE key=? AND value='1'`, KeyMustChg)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrSetupDone
		}
		for _, kv := range [][2]string{{KeyUser, user}, {KeyHash, string(h)}} {
			if _, err := tx.Exec(
				`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
				kv[0], kv[1],
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	st.DeleteAllSessions()
	return nil
}

// SetPassword 改密码并作废所有会话；当前会话由调用方重发。
func SetPassword(st *store.Store, pw string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := st.SetSetting(KeyHash, string(h)); err != nil {
		return err
	}
	return st.SetSetting(KeyMustChg, "0")
}

// SetUser 改用户名，不影响会话（令牌与用户名无关）。
func SetUser(st *store.Store, name string) error {
	return st.SetSetting(KeyUser, name)
}

// ValidUser 用户名 3-32 位，仅字母数字和 _ - .。
func ValidUser(name string) bool {
	if len(name) < 3 || len(name) > 32 {
		return false
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}

func Check(st *store.Store, user, pass string) bool {
	if user != st.GetSetting(KeyUser, DefaultUser) {
		// 用户名错也跑一次哈希，避免用响应时间区分"用户不存在"和"密码错"
		bcrypt.CompareHashAndPassword([]byte(st.GetSetting(KeyHash, "$2a$10$invalid")), []byte(pass))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(st.GetSetting(KeyHash, "")), []byte(pass)) == nil
}

func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func SessionTTL(st *store.Store) time.Duration {
	h := 168
	if v := st.GetSetting(KeySessionTTL, "168"); v != "" {
		if n, err := time.ParseDuration(v + "h"); err == nil && n > 0 {
			h = int(n.Hours())
		}
	}
	if h < 1 {
		h = 1
	}
	return time.Duration(h) * time.Hour
}

// 可信反代网段。空表示谁都不信：RemoteAddr 就是客户端。
var trustedNets atomic.Value // []*net.IPNet

// SetTrustedProxies 记住可信反代网段（"10.0.0.0/8" 或单 IP）。
// 只有来自这些地址的请求才允许根据 X-Forwarded-For 改判客户端 IP。
func SetTrustedProxies(spec []string) {
	nets := make([]*net.IPNet, 0, len(spec))
	for _, s := range spec {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			s = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			nets = append(nets, n)
		}
	}
	trustedNets.Store(nets)
}

func trusted(ip net.IP) bool {
	nets, _ := trustedNets.Load().([]*net.IPNet)
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Login 签发会话并写 cookie。
//
// Secure 的判定：显式设置优先（cookie_secure=1/0），否则看这一次请求
// 是不是 HTTPS。反代后面不能无条件看 X-Forwarded-Proto，只有对端可信时才算。
func Login(w http.ResponseWriter, r *http.Request, st *store.Store) error {
	tok := NewToken()
	if tok == "" {
		return errNoEntropy
	}
	ttl := SessionTTL(st)
	if err := st.CreateSession(tok, ttl); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookie(r, st),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
	return nil
}

// remoteIP 取对端地址（不带端口）。
func remoteIP(r *http.Request) net.IP {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// IsTrustedProxy 判断对端是不是可信反代。只有它才有资格用 X-Forwarded-*：
// 换句话说，来自不可信对端的 X-Forwarded-Proto / X-Forwarded-Host 一律当没看见。
func IsTrustedProxy(r *http.Request) bool {
	ip := remoteIP(r)
	return ip != nil && trusted(ip)
}

func secureCookie(r *http.Request, st *store.Store) bool {
	switch st.GetSetting("cookie_secure", "") {
	case "1":
		return true
	case "0":
		return false
	}
	if r.TLS != nil {
		return true
	}
	return IsTrustedProxy(r) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

var errNoEntropy = errors.New("无法生成随机会话令牌")

// Logout 作废当前会话并清 cookie。
func Logout(w http.ResponseWriter, r *http.Request, st *store.Store) {
	if c, err := r.Cookie(CookieName); err == nil {
		st.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteLaxMode,
	})
}

// MustChange 表示账号仍是默认密码。
// 此时会话不能算已认证：admin/admin 谁都能登录，Access Token 形同虚设。
// 面板仍允许登录，否则没人能改密码。
func MustChange(st *store.Store) bool {
	return st.GetSetting(KeyMustChg, "0") == "1"
}

// Authed 判断请求是否带着有效会话。
func Authed(r *http.Request, st *store.Store) bool {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	return st.ValidSession(c.Value)
}

// ---- 登录限流：同一 IP 5 分钟内最多 8 次失败 ----

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var loginLimiter = &limiter{hits: map[string][]time.Time{}}

// clientIP 返回登录限流用的键。
//
// 之前只认 RemoteAddr：Traefik 之后所有登录都来自同一个容器地址，
// 一个公网爆破者能把「所有 IP」的名额一起耗尽，等于把管理员锁在门外。
// 现在对端可信时按 X-Forwarded-For 从右往左取第一个非可信地址
// （取最左等于让攻击者每次换个假 IP）。
// ClientIP 返回可信代理之后的客户端地址，供限流和审计共用。
func ClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil {
		if host == "" {
			// 空串会让所有请求共用一个键，限流不准还会撑大 map。
			return "unknown"
		}
		return host
	}
	if !trusted(ip) {
		return ip.String()
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			c := net.ParseIP(strings.TrimSpace(parts[i]))
			if c == nil {
				continue
			}
			if trusted(c) {
				continue
			}
			return c.String()
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if c := net.ParseIP(xr); c != nil && !trusted(c) {
			return c.String()
		}
	}
	return ip.String()
}

// Allowed 判断该 IP 还能否继续尝试登录。
func Allowed(r *http.Request) bool {
	loginLimiter.mu.Lock()
	defer loginLimiter.mu.Unlock()
	ip := ClientIP(r)
	cut := time.Now().Add(-5 * time.Minute)
	keep := loginLimiter.hits[ip][:0]
	for _, t := range loginLimiter.hits[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		// 空切片不留 map，否则长期运行会被扫过的 IP 撑大。
		delete(loginLimiter.hits, ip)
		return true
	}
	loginLimiter.hits[ip] = keep
	return len(keep) < 8
}

// Fail 记一次失败。
func Fail(r *http.Request) {
	loginLimiter.mu.Lock()
	defer loginLimiter.mu.Unlock()
	ip := ClientIP(r)
	loginLimiter.hits[ip] = append(loginLimiter.hits[ip], time.Now())
}

// Reset 登录成功后清掉失败计数。
func Reset(r *http.Request) {
	loginLimiter.mu.Lock()
	defer loginLimiter.mu.Unlock()
	delete(loginLimiter.hits, ClientIP(r))
}

// CleanPath 去掉查询串，用于日志。
func CleanPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// EnsureCSRFSecret 首次启动生成一个进程外的随机密钥，用来派生 CSRF token。
// 存在 settings 里而不是内存：多进程/重启后已发出的面板还能继续用。
func EnsureCSRFSecret(st *store.Store) {
	if st.GetSetting(KeyCSRFSecret, "") != "" {
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return
	}
	_ = st.SetSetting(KeyCSRFSecret, hex.EncodeToString(b))
}

// CSRF 按会话派生 token：同一会话稳定，换会话即失效，不需要额外的 cookie。
// 攻击者读不到 /api/me（跨站读不了响应），也读不到这里。
func CSRF(st *store.Store, sessionToken string) string {
	secret := st.GetSetting(KeyCSRFSecret, "")
	if secret == "" || sessionToken == "" {
		return ""
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(sessionToken))
	return hex.EncodeToString(m.Sum(nil))
}
