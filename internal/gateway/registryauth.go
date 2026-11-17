package gateway

import (
	"context"
	"encoding/json"
	"errors"
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
//
// creds 为 nil 表示匿名流程；非 nil 时按凭据模式决定 Token 端点是否带 Basic。
func (c *tokenCache) get(ctx context.Context, rt http.RoundTripper, key tokenKey, ch v2auth.Challenge, scope string, creds *v2auth.Credentials) (string, error) {
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
	tok, ttl, err := fetchToken(fetchCtx, rt, ch, scope, creds)
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

// invalidate 删除单个 key：401 失效刷新与凭据变更都走这里。
func (c *tokenCache) invalidate(key tokenKey) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
	metrics.PrivateTokenInvalidated()
}

// invalidateHost 清掉某 Host 的全部 Token（凭据变更/删除时调用）。
func (c *tokenCache) invalidateHost(host string) int {
	c.mu.Lock()
	n := 0
	for k := range c.m {
		if k.Host == host {
			delete(c.m, k)
			n++
		}
	}
	c.mu.Unlock()
	if n > 0 {
		metrics.TokenCacheEntries(len(c.m))
	}
	return n
}

// clear 清空整个 Token Cache（恢复流程的硬性步骤）。
func (c *tokenCache) clear() {
	c.mu.Lock()
	c.m = map[tokenKey]tokenEntry{}
	c.mu.Unlock()
	metrics.TokenCacheEntries(0)
}

// fetchToken 换取 Token。
//
// Token 端点规则（15-C.3）：
//   - 最多 3 跳，只允许同源 HTTPS；跨源重定向直接拒绝；
//   - Basic 只发往 Token 端点本身，任何来源变化都清除；
//   - Basic 模式补 account=<username>（不支持的端点会忽略）。
func fetchToken(ctx context.Context, rt http.RoundTripper, ch v2auth.Challenge, scope string, creds *v2auth.Credentials) (string, time.Duration, error) {
	u, err := url.Parse(ch.Realm)
	if err != nil {
		return "", 0, err
	}
	q := u.Query()
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	method := http.MethodGet
	useBasic := false
	if creds != nil {
		switch creds.AuthMode() {
		case v2auth.ModeBasic:
			// 直接 Basic 不走 Token 端点；到这里说明配置矛盾，fail closed。
			return "", 0, errors.New("basic 模式不应请求 Token 端点")
		case v2auth.ModeBearerTokenBasic:
			useBasic = true
		case v2auth.ModeBearerTokenNone:
			useBasic = false
		default:
			return "", 0, v2auth.ErrCredentialModeInvalid
		}
		if strings.EqualFold(strings.TrimSpace(creds.TokenMethod), "POST") {
			return "", 0, v2auth.ErrTokenMethodUnsupported
		}
		if useBasic {
			q.Set("account", creds.Username)
		}
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "mirror-lens/1.0 (+https://github.com/zggnh12345/mirror-lens)")
	if useBasic {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	cli := &http.Client{
		Transport: rt,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			from := via[len(via)-1].URL
			if _, err := v2auth.RedirectPolicy(v2auth.RedirectToken, http.MethodGet, from, r.URL, len(via)); err != nil {
				return err
			}
			// 同源才保留 Basic；规则已保证同源，这里再显式清一次防御。
			if !strings.EqualFold(from.Host, r.URL.Host) || !strings.EqualFold(from.Scheme, r.URL.Scheme) {
				r.Header.Del("Authorization")
			}
			return nil
		},
	}
	resp, err := cli.Do(req)
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

// bearerTransport 是 registryAuthTransport 的历史别名：
// 3.5.11 的调用点与测试仍用这个名字，实现只有一份。
type bearerTransport = registryAuthTransport

func authErrorResponse(req *http.Request, code int, msg string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-ML-Error", msg)
	// 稳定错误码：与 withAuthCode 同名，客户端/验收脚本只看这个。
	h.Set("X-ML-Auth-Error", msg)
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

// drainClose 读完并关闭上游响应体，避免连接池里留下半截连接。
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}
