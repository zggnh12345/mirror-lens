package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
)

// HeaderRequestID 是请求标识头名，nginx/Traefik/Cloudflare 都用它。
const HeaderRequestID = "X-Request-Id"

type ctxKey struct{}

// validID 只放行字母数字和 - _、长度 8~64：否则日志里能塞换行和伪造字段。
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// Middleware 分配 request_id：沿用合法的上游值，否则生成，
// 写回响应头并放进 context。
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validID.MatchString(id) {
			id = newID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID 取不到时返回空串，调用点不必判断。
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(ctxKey{}).(string)
	return s
}
