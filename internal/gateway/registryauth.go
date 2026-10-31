package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// registry 上游（Docker Hub / ghcr / quay）都要求先拿 Bearer token：
// 第一次请求回 401 + WWW-Authenticate: Bearer realm=...,service=...,scope=...，
// 拿 realm 换 token 再重发。这套流程是 registry v2 规范的一部分，
// 所以换上游不用改代码。
type tokenCache struct {
	mu sync.Mutex
	m  map[string]tokenEntry
}

type tokenEntry struct {
	tok string
	exp time.Time
}

func newTokenCache() *tokenCache { return &tokenCache{m: map[string]tokenEntry{}} }

// tokenCacheMax 是缓存条目上限。key 里带 scope（每个仓库一个），长期运行
// 会一直长下去；到上限时先把过期的清掉。
const tokenCacheMax = 256

// pruneLocked 在条目过多时清掉已过期的 token。调用方持锁。
func (c *tokenCache) pruneLocked() {
	if len(c.m) < tokenCacheMax {
		return
	}
	now := time.Now()
	for k, e := range c.m {
		if !now.Before(e.exp) {
			delete(c.m, k)
		}
	}
}

func (c *tokenCache) get(ctx context.Context, rt http.RoundTripper, realm, service, scope string) (string, error) {
	key := realm + "|" + service + "|" + scope
	c.mu.Lock()
	if e, ok := c.m[key]; ok && time.Now().Before(e.exp) {
		c.mu.Unlock()
		return e.tok, nil
	}
	c.mu.Unlock()

	u, err := url.Parse(realm)
	if err != nil {
		return "", err
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
		return "", err
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", &tokenError{code: resp.StatusCode}
	}
	var out struct {
		Token     string `json:"token"`
		Access    string `json:"access_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	tok := out.Token
	if tok == "" {
		tok = out.Access
	}
	if tok == "" {
		return "", &tokenError{code: 200}
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	// 提前 30 秒过期，别卡在边界上
	if ttl > 45*time.Second {
		ttl -= 30 * time.Second
	}

	c.mu.Lock()
	c.pruneLocked()
	c.m[key] = tokenEntry{tok: tok, exp: time.Now().Add(ttl)}
	c.mu.Unlock()
	return tok, nil
}

type tokenError struct{ code int }

func (e *tokenError) Error() string {
	return "上游 token 申请失败: HTTP " + strconv.Itoa(e.code)
}

// bearerTransport 只对指定上游生效：401 + Bearer 挑战时换 token 重发一次。
// 别的 401（比如 GitHub 的 Basic）原样返回，不掺和。
type bearerTransport struct {
	base     http.RoundTripper
	host     string
	tokens   *tokenCache
	maxRetry int
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	realm, service, scope, ok := parseBearer(resp.Header.Get("WWW-Authenticate"))
	if !ok {
		return resp, nil
	}
	// 官方源：realm 通常在 auth.docker.io，可以跨主机。
	// 镜像源：只接受「realm 就在这台镜像自己身上」的挑战——把镜像站给的
	// realm 发到别处，等于把凭据递给第三方。
	if !strings.EqualFold(req.URL.Host, t.host) && !sameHost(realm, req.URL.Host) {
		return resp, nil
	}
	if scope == "" {
		scope = scopeFromPath(req.URL.Path)
	}
	tok, err := t.tokens.get(req.Context(), t.base, realm, service, scope)
	if err != nil {
		return resp, nil // 拿不到 token 就把 401 原样给客户端，信息更准确
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r2)
}

func sameHost(raw, host string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// parseBearer 解析 `Bearer realm="x",service="y",scope="z"`。
func parseBearer(h string) (realm, service, scope string, ok bool) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(h)), "bearer ") {
		return "", "", "", false
	}
	body := strings.TrimSpace(h)[len("bearer "):]
	for _, part := range splitParams(body) {
		k, v, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "realm":
			realm = v
		case "service":
			service = v
		case "scope":
			scope = v
		}
	}
	return realm, service, scope, realm != ""
}

// splitParams 按逗号切，但跳过引号里的逗号。
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, c := range s {
		switch {
		case c == '"':
			inQuote = !inQuote
			cur.WriteRune(c)
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// scopeFromPath 从 /v2/<name>/manifests|blobs/... 推出 repository:<name>:pull。
func scopeFromPath(p string) string {
	p = strings.TrimPrefix(p, "/v2/")
	for _, sep := range []string{"/manifests/", "/blobs/"} {
		if i := strings.Index(p, sep); i > 0 {
			return "repository:" + p[:i] + ":pull"
		}
	}
	return ""
}
