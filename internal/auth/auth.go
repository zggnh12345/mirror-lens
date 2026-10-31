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

// EnsureAdmin 首次启动时写入默认账号 admin/admin，并标记需要改密。
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

// ErrSetupDone 表示首次设置已经被人抢先做完了。
var ErrSetupDone = errors.New("账号已经设置过了")

// Setup 首次设置账号：认领 must_change、写用户名、写密码，三件事在同一个事务里。
// 认领是一条带条件的 UPDATE（value='1' → '0'），所以并发时只有一个调用能成功，
// 失败的那个整体回滚，不会留下「用户名是 A、密码是 B」的半截状态。
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

// SetPassword 改密码，并作废所有已有会话（当前会话由调用方重新签发）。
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

// SetUser 改用户名。会话不受影响（令牌与用户名无关）。
func SetUser(st *store.Store, name string) error {
	return st.SetSetting(KeyUser, name)
}

// ValidUser 用户名规则：3-32 位，字母数字和 _ - . ，不能是纯空白。
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

// Check 校验用户名密码。
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

// Login 成功后签发会话并写 cookie。
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
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// Allowed 判断该 IP 是否还能继续尝试登录。
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
