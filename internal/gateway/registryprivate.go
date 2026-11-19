package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/unified"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// 凭据来源的状态错误：gateway 不 import secret，由装配层映射。
var (
	ErrCredPending       = errors.New("凭据等待对账")
	ErrCredNotReconciled = errors.New("凭据尚未对账")
	ErrCredQuarantined   = errors.New("凭据记录损坏")
)

// CredentialSource 是 Gateway 读取 Host 级凭据的唯一入口。
// 生产装配由 main 用 secret.Store 适配（CredentialView），gateway 不直接
// 依赖 secret 包的字段布局。
type CredentialSource interface {
	CredentialView(host string) (CredentialView, error)
}

// CredentialView 是 Gateway 需要的凭据版本视图。
type CredentialView struct {
	Host          string
	Generation    int64
	Digest        string
	HasCredential bool
	Username      string
	Password      string
	Mode          string
	TokenMethod   string
	TokenAuth     string
}

// PrivateAuthOptions 是 15-B 的运行时开关。
type PrivateAuthOptions struct {
	// Enabled 默认 false；true 时必须同时满足 require_token。
	Enabled bool
	// Hosts 是 private_auth_hosts，必须是代码 allowlist 的子集。
	Hosts map[string]bool
	// Creds 为 nil 表示没有凭据来源，私有认证一律 fail closed。
	Creds CredentialSource
	// RequireToken 是 auth.require_token 的运行时值。
	RequireToken bool
}

func (o PrivateAuthOptions) hostEnabled(host string) bool {
	if !o.Enabled || o.Creds == nil {
		return false
	}
	return o.Hosts[normalizeHostKey(host)]
}

func normalizeHostKey(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), "/")
}

// negCache 是认证失败负缓存：按 Host+模式 30s 起，指数退避到 5m。
// 目的是别把 Registry 账号打到锁定，而不是省一次请求。
type negCache struct {
	mu    sync.Mutex
	m     map[string]negEntry
	now   func() time.Time
	base  time.Duration
	limit time.Duration
}

type negEntry struct {
	until    time.Time
	failures int
}

func newNegCache() *negCache {
	return &negCache{m: map[string]negEntry{}, now: time.Now, base: 30 * time.Second, limit: 5 * time.Minute}
}

func (c *negCache) blocked(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return false
	}
	if c.now().Before(e.until) {
		return true
	}
	return false
}

func (c *negCache) fail(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[key]
	e.failures++
	d := c.base
	for i := 1; i < e.failures; i++ {
		d *= 2
		if d >= c.limit {
			d = c.limit
			break
		}
	}
	e.until = c.now().Add(d)
	c.m[key] = e
	metrics.AuthNegativeCache(len(c.m))
}

func (c *negCache) clear(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

// clearHost 在凭据变更后清掉该 Host 的全部模式。
func (c *negCache) clearHost(host string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for k := range c.m {
		if strings.HasPrefix(k, host+"|") {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

// registryAuthTransport 是 Registry 上游的唯一认证执行点。
//
// 流程（1.6 15-B.2）：
//  1. 匿名发一次（不带任何 Authorization）；
//  2. 401 + 合法 Challenge 时解析 Realm；
//  3. 有凭据且门禁通过 ⇒ Registry 直接 Basic，或 Bearer Token（Basic 只发往 Token 端点）；
//  4. 认证后仍 401 ⇒ 失效 Token、重取一次、重试一次，不循环；
//  5. 认证失败不回退匿名缓存（缓存绕过在 Gateway 层已保证）。
type registryAuthTransport struct {
	base     http.RoundTripper
	tokens   *tokenCache
	priv     PrivateAuthOptions
	neg      *negCache
	resolver v2auth.PolicyResolver
	now      func() time.Time
	ua       string
}

func (t *registryAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !v2auth.IsRegistryRequest(req.Context()) {
		// Git/Private/Raw 走同一个 http.Client，但不属于 Registry 身份链。
		return t.base.RoundTrip(req)
	}
	policy, ok := v2auth.PolicyFromContext(req.Context())
	if !ok {
		metrics.AuthIdentityMissing()
		return authErrorResponse(req, http.StatusServiceUnavailable, "auth_policy_missing"), nil
	}
	// 匿名优先：上游请求一律先不带认证头。
	anon := req.Clone(req.Context())
	anon.Header.Del("Authorization")
	resp, err := t.base.RoundTrip(anon)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	raw := resp.Header.Get("Www-Authenticate")
	scheme := challengeScheme(raw)
	if resp.StatusCode == http.StatusForbidden && scheme == "" {
		// 403 匿名且无 Challenge：不重试（F06）。
		return resp, nil
	}
	if scheme == "" {
		return withAuthCode(resp, "auth_challenge_missing"), nil
	}
	// Registry 直接 Basic：Challenge 是 Basic，没有 Realm/Token 端点。
	if scheme == "basic" {
		return t.directBasic(req, resp, policy), nil
	}
	ch := v2auth.ParseChallenge(raw)
	if ch.Realm == "" {
		return withAuthCode(resp, "auth_challenge_missing"), nil
	}
	ru, perr := url.Parse(ch.Realm)
	if perr != nil {
		return withAuthCode(resp, "auth_challenge_missing"), nil
	}
	if aerr := policy.Allows(ru); aerr != nil {
		metrics.AuthRealmDenied()
		return withAuthCode(resp, "auth_realm_denied"), nil
	}
	scope := normalizeScope(ch.Scope)
	if scope == "" {
		scope = scopeFromPath(req.URL.Path, policy.LogicalHost)
	}

	policy, snap, hasCred, cerr := t.authSnapshot(req.Context(), policy, req.URL.Host)
	if cerr != nil {
		drainClose(resp)
		return authErrorResponse(req, authStatusFor(cerr), authCodeFor(cerr)), nil
	}
	makeKey := func(p v2auth.PolicySnapshot) tokenKey {
		return tokenKey{
			SourceID:  p.SourceID,
			Host:      p.LogicalHost,
			Realm:     normalizeRealmKey(ch.Realm),
			Service:   ch.Service,
			Scope:     scope,
			CredGen:   p.CredGen,
			PolicyGen: p.PolicyGen,
		}
	}
	key := makeKey(policy)
	if !hasCred {
		// 匿名 Bearer：保持 3.5.11 的既有行为（公共 Registry 走这条）。
		tok, terr := t.tokens.get(req.Context(), t.base, key, ch, scope, nil)
		if terr != nil {
			metrics.TokenRefreshError()
			// 预算耗尽不是「拿不到 Token」：它是独立最高优先级通道，
			// 必须冒泡成 503，不能吞掉后退回 401。
			if unified.IsBudgetExhausted(terr) {
				drainClose(resp)
				return nil, terr
			}
			return resp, nil
		}
		drainClose(resp)
		r2 := req.Clone(req.Context())
		r2.Header.Del("Authorization")
		r2.Header.Set("Authorization", "Bearer "+tok)
		return t.base.RoundTrip(r2)
	}

	mode := snap.Credentials.AuthMode()
	// 负缓存 key 带 gen：凭据变更后旧 key 自然失效，不依赖显式清理。
	negKey := fmt.Sprintf("%s|%s|%d", normalizeHostKey(policy.LogicalHost), mode, policy.CredGen)
	if t.neg.blocked(negKey) {
		drainClose(resp)
		metrics.AuthNegativeHit()
		return authErrorResponse(req, http.StatusUnauthorized, "auth_backoff"), nil
	}

	switch mode {
	case v2auth.ModeBasic:
		// Registry 直接 Basic：只发往 Registry origin，不经过 Token 端点。
		drainClose(resp)
		r2 := req.Clone(req.Context())
		r2.Header.Del("Authorization")
		r2.SetBasicAuth(snap.Credentials.Username, snap.Credentials.Password)
		out, rerr := t.base.RoundTrip(r2)
		if rerr != nil {
			return out, rerr
		}
		if out.StatusCode == http.StatusUnauthorized || out.StatusCode == http.StatusForbidden {
			t.neg.fail(negKey)
			metrics.AuthFailed()
			return withAuthCode(out, "auth_failed"), nil
		}
		return out, nil
	case v2auth.ModeBearerTokenBasic, v2auth.ModeBearerTokenNone:
		creds := &snap.Credentials
		tok, terr := t.tokens.get(req.Context(), t.base, key, ch, scope, creds)
		if terr != nil {
			drainClose(resp)
			if unified.IsBudgetExhausted(terr) {
				return nil, terr
			}
			t.neg.fail(negKey)
			metrics.AuthFailed()
			return authErrorResponse(req, tokenStatusFor(terr), tokenCodeFor(terr)), nil
		}
		drainClose(resp)
		out, rerr := t.sendBearer(req, tok)
		if rerr != nil {
			return out, rerr
		}
		if out.StatusCode == http.StatusUnauthorized {
			// 401 失效：删 key、重取一次、重试一次；第二次仍 401 直接返回。
			t.tokens.invalidate(key)
			tok2, terr2 := t.tokens.get(req.Context(), t.base, key, ch, scope, creds)
			if terr2 != nil {
				drainClose(out)
				t.neg.fail(negKey)
				return authErrorResponse(req, tokenStatusFor(terr2), tokenCodeFor(terr2)), nil
			}
			drainClose(out)
			out, rerr = t.sendBearer(req, tok2)
			if rerr != nil {
				return out, rerr
			}
			if out.StatusCode == http.StatusUnauthorized {
				t.neg.fail(negKey)
				metrics.AuthFailed()
				return withAuthCode(out, "auth_failed"), nil
			}
		}
		if out.StatusCode == http.StatusForbidden {
			metrics.AuthFailed()
			return withAuthCode(out, "auth_forbidden"), nil
		}
		return out, nil
	default:
		drainClose(resp)
		return authErrorResponse(req, http.StatusServiceUnavailable, "auth_mode_invalid"), nil
	}
}

func (t *registryAuthTransport) sendBearer(req *http.Request, tok string) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.Header.Del("Authorization")
	r2.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r2)
}

// authSnapshot 读取凭据并执行身份/gen 门禁。
//
// C.1：策略解析后凭据可能刚好换代。只重解析一次；仍不一致就 fail closed，
// 不能拿旧 gen 的凭据继续认证。返回的 policy 是最终生效版本。
func (t *registryAuthTransport) authSnapshot(ctx context.Context, policy v2auth.PolicySnapshot, targetHost string) (v2auth.PolicySnapshot, v2auth.CredentialSnapshot, bool, error) {
	snap, has, err := t.credential(ctx, policy, targetHost)
	if err != nil || !has {
		return policy, snap, has, err
	}
	if err := v2auth.AuthorizeCredential(ctx, snap); err != nil {
		if !errors.Is(err, v2auth.ErrCredentialStale) || t.resolver == nil {
			return policy, snap, has, err
		}
		p2, rerr := t.resolver.Resolve(ctx, policy.SourceID, policy.LogicalHost)
		if rerr != nil {
			return policy, snap, has, err
		}
		p2.LogicalHost = policy.LogicalHost
		p2.Scheme = policy.Scheme
		ctx2 := v2auth.WithPolicy(ctx, p2)
		snap2, has2, cerr2 := t.credential(ctx2, p2, targetHost)
		if cerr2 != nil {
			return policy, snap, has, cerr2
		}
		if !has2 {
			return policy, snap, has, v2auth.ErrCredentialUnavailable
		}
		if aerr := v2auth.AuthorizeCredential(ctx2, snap2); aerr != nil {
			return policy, snap, has, aerr
		}
		return p2, snap2, true, nil
	}
	return policy, snap, true, nil
}

// credential 读取并校验凭据快照。返回 hasCred=false 表示按匿名流程走。
func (t *registryAuthTransport) credential(ctx context.Context, policy v2auth.PolicySnapshot, targetHost string) (v2auth.CredentialSnapshot, bool, error) {
	id := v2auth.IdentityFromContext(ctx)
	if id.Kind() != v2auth.IdentityOfficial || !policy.Official || policy.SourceID != 0 {
		return v2auth.CredentialSnapshot{}, false, nil
	}
	host := normalizeHostKey(policy.LogicalHost)
	// 凭据与目标 Host 绑定：重定向到别的 Host 后（Authorization 已被
	// CheckRedirect 清掉），这里必须拒绝重新认证，否则官方 Basic 会被
	// 发到任意 HTTPS Host（S07/S08）。
	if normalizeHostKey(targetHost) != host {
		return v2auth.CredentialSnapshot{}, false, nil
	}
	if _, verify := ctx.Value(verifyCtxKey{}).(bool); verify {
		// 凭据验证入口：只跳过 allowlist 开关，身份/策略/gen 门禁不变。
		if t.priv.Creds == nil {
			return v2auth.CredentialSnapshot{}, false, nil
		}
	} else {
		if !t.priv.hostEnabled(host) {
			return v2auth.CredentialSnapshot{}, false, nil
		}
		// 运行时 Access Token 被关闭：私有认证立即失效（D-15-15）。
		if !t.priv.RequireToken {
			return v2auth.CredentialSnapshot{}, false, v2auth.ErrCredentialUnavailable
		}
	}
	raw, err := t.priv.Creds.CredentialView(host)
	if err != nil {
		// 凭据状态 Pending/Quarantined/未对账：fail closed，不回退匿名。
		return v2auth.CredentialSnapshot{}, false, err
	}
	if !raw.HasCredential {
		return v2auth.CredentialSnapshot{}, false, nil
	}
	return v2auth.CredentialSnapshot{
		Host:          raw.Host,
		Generation:    raw.Generation,
		Digest:        raw.Digest,
		HasCredential: true,
		Credentials: v2auth.Credentials{
			Username:    raw.Username,
			Password:    raw.Password,
			Mode:        raw.Mode,
			TokenMethod: raw.TokenMethod,
			TokenAuth:   raw.TokenAuth,
		},
	}, true, nil
}

func authCodeFor(err error) string {
	switch {
	case errors.Is(err, v2auth.ErrIdentityUnknown):
		return "auth_identity_unknown"
	case errors.Is(err, v2auth.ErrIdentityNotOfficial):
		return "auth_identity_not_official"
	case errors.Is(err, v2auth.ErrCredentialHostMismatch):
		return "auth_host_mismatch"
	case errors.Is(err, v2auth.ErrCredentialStale):
		return "auth_stale"
	case errors.Is(err, v2auth.ErrCredentialModeInvalid):
		return "auth_mode_invalid"
	case errors.Is(err, v2auth.ErrCredentialUnavailable):
		return "auth_unavailable"
	case errors.Is(err, v2auth.ErrPolicyMissing):
		return "auth_policy_missing"
	// 凭据状态错误要能区分「等对账」和「记录损坏」，否则运维只能猜。
	case errors.Is(err, ErrCredPending):
		return "auth_cred_pending"
	case errors.Is(err, ErrCredNotReconciled):
		return "auth_cred_not_reconciled"
	case errors.Is(err, ErrCredQuarantined):
		return "auth_cred_quarantined"
	}
	return "auth_failed"
}

func authStatusFor(err error) int {
	switch {
	case errors.Is(err, v2auth.ErrCredentialStale), errors.Is(err, v2auth.ErrCredentialUnavailable),
		errors.Is(err, v2auth.ErrPolicyMissing), errors.Is(err, v2auth.ErrIdentityUnknown),
		errors.Is(err, ErrCredPending), errors.Is(err, ErrCredNotReconciled), errors.Is(err, ErrCredQuarantined):
		return http.StatusServiceUnavailable
	}
	return http.StatusUnauthorized
}

func tokenStatusFor(err error) int {
	var te *tokenError
	if errors.As(err, &te) {
		if te.code >= 500 {
			return http.StatusBadGateway
		}
		if te.code == http.StatusUnauthorized || te.code == http.StatusForbidden {
			return te.code
		}
	}
	return http.StatusBadGateway
}

func tokenCodeFor(err error) string {
	var te *tokenError
	if errors.As(err, &te) {
		if te.code >= 500 {
			return "token_upstream_error"
		}
		if te.code == http.StatusUnauthorized || te.code == http.StatusForbidden {
			return "token_auth_failed"
		}
	}
	return "token_upstream_error"
}

// withAuthCode 给上游 401/403 响应打稳定错误码；不改状态码，不改上游正文。
func withAuthCode(resp *http.Response, code string) *http.Response {
	if resp == nil {
		return resp
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	resp.Header.Set("X-ML-Auth-Error", code)
	return resp
}

// normalizeRealmKey 把 Realm 规范化为 scheme://host[:port]，去尾部斜杠。
func normalizeRealmKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), "/")
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// normalizeScope 排序、去重、去空，用固定分隔符编码后再入键。
func normalizeScope(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' })
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return strings.Join(out, "\x1f")
}

// VerifyResult 是官方私有凭据验证的结果；只暴露状态码与稳定错误码。
type VerifyResult struct {
	Status int
	Code   string
}

type verifyCtxKey struct{}

// VerifyCredential 用已存储的官方凭据对 Host 做一次真实认证验证。
//
// 独立入口、独立身份：IdentityKind=Official、SourceID=0、不走缓存；
// 结果只用于凭据验证/运维展示，不写 source_host_caps / registry_host_recovery，
// 也不产生候选健康样本。整个流程复用同一个认证 transport，不存在第二条
// 认证实现。
func (g *Gateway) VerifyCredential(ctx context.Context, host string) VerifyResult {
	host = normalizeHostKey(host)
	if g == nil || g.cli == nil || host == "" {
		return VerifyResult{Status: http.StatusServiceUnavailable, Code: "verify_unavailable"}
	}
	if g.privAuth.Creds == nil {
		return VerifyResult{Status: http.StatusServiceUnavailable, Code: "verify_no_credentials"}
	}
	// D-15-15：verify 只允许在私有认证实际启用时使用；不能拿它当
	// 绕过 private_auth_enabled 的旁路。
	if !g.privAuth.Enabled {
		return VerifyResult{Status: http.StatusServiceUnavailable, Code: "verify_private_auth_disabled"}
	}
	// 运行时 Access Token 关闭时，verify 也不能继续使用 Host 凭据。
	// 这是运维验证入口，不是绕过认证开关的旁路。
	if !g.privAuth.RequireToken {
		return VerifyResult{Status: http.StatusServiceUnavailable, Code: "verify_requires_token"}
	}
	plan := &adapter.Plan{
		Registry:    true,
		LogicalHost: host,
		Upstream:    &url.URL{Scheme: "https", Host: host},
	}
	vctx, err := g.withRegistryPolicy(ctx, plan, 0, host, "https")
	if err != nil {
		return VerifyResult{Status: http.StatusServiceUnavailable, Code: authCodeFor(err)}
	}
	// 标记这是「凭据验证」：只影响凭据查找，不放宽任何门禁。
	vctx = context.WithValue(vctx, verifyCtxKey{}, true)
	req, rerr := http.NewRequestWithContext(vctx, http.MethodGet, "https://"+host+"/v2/", nil)
	if rerr != nil {
		return VerifyResult{Status: http.StatusBadRequest, Code: "verify_request_invalid"}
	}
	req.Header.Set("User-Agent", "mirror-lens/"+Version+" (+https://github.com/zggnh12345/mirror-lens)")
	resp, derr := g.cli.Do(req)
	if derr != nil {
		return VerifyResult{Status: http.StatusBadGateway, Code: "verify_transport_error"}
	}
	defer resp.Body.Close()
	code := resp.Header.Get("X-ML-Auth-Error")
	if code == "" {
		switch resp.StatusCode {
		case http.StatusOK:
			code = "ok"
		case http.StatusUnauthorized:
			code = "auth_failed"
		case http.StatusForbidden:
			code = "auth_forbidden"
		default:
			code = "upstream_status"
		}
	}
	return VerifyResult{Status: resp.StatusCode, Code: code}
}

// challengeScheme 解析 WWW-Authenticate 的认证方案。
// Registry 直接 Basic 与 Bearer 是两条独立语义，不能混用。
func challengeScheme(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if i := strings.IndexByte(h, ' '); i > 0 {
		switch strings.ToLower(h[:i]) {
		case "bearer":
			return "bearer"
		case "basic":
			return "basic"
		}
		return ""
	}
	if strings.EqualFold(h, "basic") {
		return "basic"
	}
	return ""
}

// directBasic 是 Registry 直接 Basic 的唯一实现：只对 basic 模式生效，
// 且仍要过完整身份门禁。Basic 只发往 Registry origin，不经过 Token 端点。
func (t *registryAuthTransport) directBasic(req *http.Request, resp *http.Response, policy v2auth.PolicySnapshot) *http.Response {
	policy, snap, hasCred, cerr := t.authSnapshot(req.Context(), policy, req.URL.Host)
	if cerr != nil {
		drainClose(resp)
		return authErrorResponse(req, authStatusFor(cerr), authCodeFor(cerr))
	}
	if !hasCred || snap.Credentials.AuthMode() != v2auth.ModeBasic {
		drainClose(resp)
		return authErrorResponse(req, http.StatusUnauthorized, "auth_challenge_missing")
	}
	negKey := fmt.Sprintf("%s|basic|%d", normalizeHostKey(policy.LogicalHost), policy.CredGen)
	if t.neg.blocked(negKey) {
		drainClose(resp)
		metrics.AuthNegativeHit()
		return authErrorResponse(req, http.StatusUnauthorized, "auth_backoff")
	}
	drainClose(resp)
	r2 := req.Clone(req.Context())
	r2.Header.Del("Authorization")
	r2.SetBasicAuth(snap.Credentials.Username, snap.Credentials.Password)
	out, err := t.base.RoundTrip(r2)
	if err != nil {
		return authErrorResponse(req, http.StatusBadGateway, "auth_transport_error")
	}
	if out.StatusCode == http.StatusUnauthorized || out.StatusCode == http.StatusForbidden {
		t.neg.fail(negKey)
		metrics.AuthFailed()
		return withAuthCode(out, "auth_failed")
	}
	return out
}
