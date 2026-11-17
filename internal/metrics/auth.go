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
	tokenCacheHits      atomic.Uint64
	tokenCacheMisses    atomic.Uint64
	tokenCacheEvicted   atomic.Uint64
	tokenCacheEntries   atomic.Uint64
	tokenCacheCapacity  atomic.Uint64
	// 3.5.15 私有认证观测。
	authDenied       atomic.Uint64
	authFailed       atomic.Uint64
	authRealmDenied  atomic.Uint64
	authNegHits      atomic.Uint64
	authNegEntries   atomic.Uint64
	privBypassReqs   atomic.Uint64
	privTokenInvalid atomic.Uint64
)

func AuthIdentityMissing() { authIdentityMissing.Add(1) }
func AuthPolicyMissing()   { authPolicyMissing.Add(1) }
func TokenRefreshError()   { authTokenRefreshErr.Add(1) }

// Token Cache 观测。Entries/Capacity 是 gauge，其余是 counter。
// 3.5.15：门禁拒绝、认证失败、Realm 拒绝、负缓存与缓存绕过。
func AuthDenied()              { authDenied.Add(1) }
func AuthFailed()              { authFailed.Add(1) }
func AuthRealmDenied()         { authRealmDenied.Add(1) }
func AuthNegativeHit()         { authNegHits.Add(1) }
func AuthNegativeCache(n int)  { authNegEntries.Store(uint64(n)) }
func PrivateBypass()           { privBypassReqs.Add(1) }
func PrivateTokenInvalidated() { privTokenInvalid.Add(1) }

func TokenCacheHit()           { tokenCacheHits.Add(1) }
func TokenCacheMiss()          { tokenCacheMisses.Add(1) }
func TokenCacheEvicted(n int)  { tokenCacheEvicted.Add(uint64(n)) }
func TokenCacheEntries(n int)  { tokenCacheEntries.Store(uint64(n)) }
func TokenCacheCapacity(n int) { tokenCacheCapacity.Store(uint64(n)) }

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
	b.WriteString("# HELP " + prefix + "auth_cache_hits_total 认证缓存命中次数\n")
	b.WriteString("# TYPE " + prefix + "auth_cache_hits_total counter\n")
	fmt.Fprintf(b, "%sauth_cache_hits_total %d\n", prefix, tokenCacheHits.Load())
	b.WriteString("# HELP " + prefix + "auth_cache_misses_total 认证缓存未命中次数\n")
	b.WriteString("# TYPE " + prefix + "auth_cache_misses_total counter\n")
	fmt.Fprintf(b, "%sauth_cache_misses_total %d\n", prefix, tokenCacheMisses.Load())
	b.WriteString("# HELP " + prefix + "auth_cache_evictions_total 认证缓存过期或容量淘汰次数\n")
	b.WriteString("# TYPE " + prefix + "auth_cache_evictions_total counter\n")
	fmt.Fprintf(b, "%sauth_cache_evictions_total %d\n", prefix, tokenCacheEvicted.Load())
	b.WriteString("# HELP " + prefix + "auth_cache_entries 认证缓存当前条目数\n")
	b.WriteString("# TYPE " + prefix + "auth_cache_entries gauge\n")
	fmt.Fprintf(b, "%sauth_cache_entries %d\n", prefix, tokenCacheEntries.Load())
	b.WriteString("# HELP " + prefix + "auth_denied_total 凭据门禁拒绝次数\n")
	b.WriteString("# TYPE " + prefix + "auth_denied_total counter\n")
	fmt.Fprintf(b, "%sauth_denied_total %d\n", prefix, authDenied.Load())
	b.WriteString("# HELP " + prefix + "auth_failed_total 认证后仍失败次数\n")
	b.WriteString("# TYPE " + prefix + "auth_failed_total counter\n")
	fmt.Fprintf(b, "%sauth_failed_total %d\n", prefix, authFailed.Load())
	b.WriteString("# HELP " + prefix + "auth_realm_denied_total Realm 不在策略内的次数\n")
	b.WriteString("# TYPE " + prefix + "auth_realm_denied_total counter\n")
	fmt.Fprintf(b, "%sauth_realm_denied_total %d\n", prefix, authRealmDenied.Load())
	b.WriteString("# HELP " + prefix + "auth_negative_hits_total 负缓存拦截次数\n")
	b.WriteString("# TYPE " + prefix + "auth_negative_hits_total counter\n")
	fmt.Fprintf(b, "%sauth_negative_hits_total %d\n", prefix, authNegHits.Load())
	b.WriteString("# HELP " + prefix + "auth_negative_entries 负缓存条目数\n")
	b.WriteString("# TYPE " + prefix + "auth_negative_entries gauge\n")
	fmt.Fprintf(b, "%sauth_negative_entries %d\n", prefix, authNegEntries.Load())
	b.WriteString("# HELP " + prefix + "private_cache_bypass_total 私有认证 Host 的缓存绕过次数\n")
	b.WriteString("# TYPE " + prefix + "private_cache_bypass_total counter\n")
	fmt.Fprintf(b, "%sprivate_cache_bypass_total %d\n", prefix, privBypassReqs.Load())
	b.WriteString("# HELP " + prefix + "private_cred_invalidated_total 401 后失效认证缓存的次数\n")
	b.WriteString("# TYPE " + prefix + "private_cred_invalidated_total counter\n")
	fmt.Fprintf(b, "%sprivate_cred_invalidated_total %d\n", prefix, privTokenInvalid.Load())
	b.WriteString("# HELP " + prefix + "auth_cache_capacity 认证缓存容量上限\n")
	b.WriteString("# TYPE " + prefix + "auth_cache_capacity gauge\n")
	fmt.Fprintf(b, "%sauth_cache_capacity %d\n", prefix, tokenCacheCapacity.Load())
}
