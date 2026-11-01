// Package auth handles the single admin account, session cookies and login throttling.
package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
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

// Login 签发会话并写 cookie。
func Login(w http.ResponseWriter, st *store.Store) error {
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
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
	return nil
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

func clientIP(r *http.Request) string {
	h := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		h = host
	}
	if h == "" {
		// 空串会让所有请求共用一个键，限流不准还会撑大 map。
		return "unknown"
	}
	return h
}

// Allowed 判断该 IP 还能否继续尝试登录。
func Allowed(r *http.Request) bool {
	loginLimiter.mu.Lock()
	defer loginLimiter.mu.Unlock()
	ip := clientIP(r)
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
	ip := clientIP(r)
	loginLimiter.hits[ip] = append(loginLimiter.hits[ip], time.Now())
}

// Reset 登录成功后清掉失败计数。
func Reset(r *http.Request) {
	loginLimiter.mu.Lock()
	defer loginLimiter.mu.Unlock()
	delete(loginLimiter.hits, clientIP(r))
}

// CleanPath 去掉查询串，用于日志。
func CleanPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
