package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// registry 上游（Docker Hub/ghcr/quay）先回 401 + Bearer 挑战：
// WWW-Authenticate: Bearer realm=...,service=...,scope=...，
// 拿 realm 换 token 再重发。v2 规范流程，换上游不用改代码。

// tokenKey 是 token 缓存的身份。
//
// Host 是逻辑仓库 Host，不是镜像站 Host；SourceID/CredGen/PolicyGen
// 让 3.5.15 的用户凭据不会跨源串用。11a 两个 Gen 恒为 0，但结构先立住。
type tokenKey struct {
	SourceID  int64
	Host      string
	Realm     string
	Service   string
	Scope     string
	CredGen   int64
	PolicyGen int64
}

type tokenEntry struct {
	tok string
	exp time.Time
}

// tokenFlight 合并同一个 key 的并发刷新。
type tokenFlight struct {
	done chan struct{}
	tok  string
	err  error
}

type tokenCache struct {
	mu      sync.Mutex
	m       map[tokenKey]tokenEntry
	flights map[tokenKey]*tokenFlight
	max     int
}

const defaultTokenCacheMax = 1024

func newTokenCache(max ...int) *tokenCache {
	n := defaultTokenCacheMax
	if len(max) > 0 && max[0] > 0 {
		n = max[0]
	}
	c := &tokenCache{m: map[tokenKey]tokenEntry{}, flights: map[tokenKey]*tokenFlight{}, max: n}
	metrics.TokenCacheCapacity(n)
	return c
}

// pruneLocked 清掉已过期 token；仍然超限就按到期时间淘汰最旧的一批。
// 调用方持锁。只清过期是不够的：全部未过期时条目会无限长下去。
func (c *tokenCache) pruneLocked() {
	if len(c.m) < c.max {
		return
	}
	now := time.Now()
	removed := 0
	for k, e := range c.m {
		if !now.Before(e.exp) {
			delete(c.m, k)
			removed++
		}
	}
	if removed > 0 {
		metrics.TokenCacheEvicted(removed)
		metrics.TokenCacheEntries(len(c.m))
	}
	if len(c.m) < c.max {
		return
	}
	// 留 3/4 的余量，避免每次写入都触发一次全排序。
	keep := c.max * 3 / 4
	type kv struct {
		k   tokenKey
		exp time.Time
	}
	all := make([]kv, 0, len(c.m))
	for k, e := range c.m {
		all = append(all, kv{k, e.exp})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].exp.Before(all[j].exp) })
	for _, x := range all[:len(all)-keep] {
		delete(c.m, x.k)
		removed++
	}
	if removed > 0 {
		metrics.TokenCacheEvicted(removed)
		metrics.TokenCacheEntries(len(c.m))
	}
}

// get 返回 token；同一个 key 同时只有一个 leader 打 Token 端点。
func (c *tokenCache) get(ctx context.Context, rt http.RoundTripper, key tokenKey, realm, service, scope string) (string, error) {
	c.mu.Lock()
	if e, ok := c.m[key]; ok && time.Now().Before(e.exp) {
		c.mu.Unlock()
		metrics.TokenCacheHit()
		return e.tok, nil
	}
	metrics.TokenCacheMiss()
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.done:
			return f.tok, f.err
		}
	}
	f := &tokenFlight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	// leader 不能跟着第一个请求的取消一起死：否则同 key 的等待者全被拖垮。
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	tok, ttl, err := fetchToken(fetchCtx, rt, realm, service, scope)
	cancel()

	c.mu.Lock()
	if err == nil {
		c.pruneLocked()
		c.m[key] = tokenEntry{tok: tok, exp: time.Now().Add(ttl)}
		metrics.TokenCacheEntries(len(c.m))
	}
	f.tok, f.err = tok, err
	delete(c.flights, key)
	close(f.done)
	c.mu.Unlock()
	return tok, err
}

func fetchToken(ctx context.Context, rt http.RoundTripper, realm, service, scope string) (string, time.Duration, error) {
	u, err := url.Parse(realm)
	if err != nil {
		return "", 0, err
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", 0, &tokenError{code: resp.StatusCode}
	}
	var out struct {
		Token     string `json:"token"`
		Access    string `json:"access_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", 0, err
	}
	tok := out.Token
	if tok == "" {
		tok = out.Access
	}
	if tok == "" {
		return "", 0, &tokenError{code: 200}
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	// 提前 30 秒过期，别卡在边界上
	if ttl > 45*time.Second {
		ttl -= 30 * time.Second
	}
	return tok, ttl, nil
}

type tokenError struct{ code int }

func (e *tokenError) Error() string {
	return "上游 token 申请失败: HTTP " + strconv.Itoa(e.code)
}

// bearerTransport 对所有 Registry 上游生效：401 + Bearer 挑战时换 token 重发。
// Realm 只接受策略快照允许的 Host；没有身份时 fail closed，不能退回按 Host 查表。
type bearerTransport struct {
	base   http.RoundTripper
	tokens *tokenCache
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !v2auth.IsRegistryRequest(req.Context()) {
		// Git/Private/Raw 走同一个 http.Client，但不属于 Registry 身份链。
		return t.base.RoundTrip(req)
	}
	policy, ok := v2auth.PolicyFromContext(req.Context())
	if !ok {
		metrics.AuthIdentityMissing()
		return authErrorResponse(req, http.StatusServiceUnavailable, "auth_policy_missing"), nil
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	ch := v2auth.ParseChallenge(resp.Header.Get("WWW-Authenticate"))
	if ch.Realm == "" {
		return resp, nil
	}
	ru, err := url.Parse(ch.Realm)
	if err != nil {
		return resp, nil
	}
	if err := policy.Allows(ru); err != nil {
		return resp, nil
	}
	scope := ch.Scope
	if scope == "" {
		scope = scopeFromPath(req.URL.Path, policy.LogicalHost)
	}
	host := policy.LogicalHost
	if host == "" {
		host = req.URL.Host
	}
	key := tokenKey{
		SourceID:  policy.SourceID,
		Host:      host,
		Realm:     ch.Realm,
		Service:   ch.Service,
		Scope:     scope,
		CredGen:   policy.CredGen,
		PolicyGen: policy.PolicyGen,
	}
	tok, err := t.tokens.get(req.Context(), t.base, key, ch.Realm, ch.Service, scope)
	if err != nil {
		metrics.TokenRefreshError()
		return resp, nil // 拿不到 token 就把 401 原样给客户端，信息更准确
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r2)
}

func authErrorResponse(req *http.Request, code int, msg string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-ML-Error", msg)
	return &http.Response{
		StatusCode: code,
		Status:     fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(msg + "\n")),
		Request:    req,
	}
}

// scopeFromPath 从 /v2/<name>/manifests|blobs/...
// 推出 repository:<name>:pull。host_prefix 下 path 前面多了一段逻辑 Host，
// 必须先剥掉，否则 scope 会变成 repository:registry.k8s.io/pause:pull。
func scopeFromPath(p, logicalHost string) string {
	p = strings.TrimPrefix(p, "/v2/")
	if logicalHost != "" {
		h := strings.TrimSuffix(logicalHost, "/")
		if p == h {
			return ""
		}
		if strings.HasPrefix(p, h+"/") {
			p = strings.TrimPrefix(p, h+"/")
		}
	}
	for _, sep := range []string{"/manifests/", "/blobs/"} {
		if i := strings.Index(p, sep); i > 0 {
			return "repository:" + p[:i] + ":pull"
		}
	}
	return ""
}
