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
)

func AuthIdentityMissing() { authIdentityMissing.Add(1) }
func AuthPolicyMissing()   { authPolicyMissing.Add(1) }
func TokenRefreshError()   { authTokenRefreshErr.Add(1) }

// Token Cache 观测。Entries/Capacity 是 gauge，其余是 counter。
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
	b.WriteString("# HELP " + prefix + "auth_cache_capacity 认证缓存容量上限\n")
	b.WriteString("# TYPE " + prefix + "auth_cache_capacity gauge\n")
	fmt.Fprintf(b, "%sauth_cache_capacity %d\n", prefix, tokenCacheCapacity.Load())
}
