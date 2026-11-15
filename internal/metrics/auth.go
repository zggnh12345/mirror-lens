package metrics

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// 3.5.11a 认证身份观测。
//
// 这些计数器只用于 11b 门禁和排障，不是运行时 dual-read 开关：
//   - identity_missing：Registry 请求进入 transport 时没有策略快照；
//   - policy_missing：resolver 明确拒绝生成快照；
//   - token_refresh_error：拿到 401 但换 token 失败。
var (
	authIdentityMissing atomic.Uint64
	authPolicyMissing   atomic.Uint64
	authTokenRefreshErr atomic.Uint64
)

func AuthIdentityMissing() { authIdentityMissing.Add(1) }
func AuthPolicyMissing()   { authPolicyMissing.Add(1) }
func TokenRefreshError()   { authTokenRefreshErr.Add(1) }

func (r *Registry) writeAuth(b *strings.Builder) {
	b.WriteString("# HELP " + prefix + "auth_identity_missing_total Registry 请求缺少策略快照的次数\n")
	b.WriteString("# TYPE " + prefix + "auth_identity_missing_total counter\n")
	fmt.Fprintf(b, "%sauth_identity_missing_total %d\n", prefix, authIdentityMissing.Load())
	b.WriteString("# HELP " + prefix + "auth_policy_missing_total 策略解析器拒绝生成快照的次数\n")
	b.WriteString("# TYPE " + prefix + "auth_policy_missing_total counter\n")
	fmt.Fprintf(b, "%sauth_policy_missing_total %d\n", prefix, authPolicyMissing.Load())
	b.WriteString("# HELP " + prefix + "auth_refresh_error_total 401 后换凭据失败的次数\n")
	b.WriteString("# TYPE " + prefix + "auth_refresh_error_total counter\n")
	fmt.Fprintf(b, "%sauth_refresh_error_total %d\n", prefix, authTokenRefreshErr.Load())
}
