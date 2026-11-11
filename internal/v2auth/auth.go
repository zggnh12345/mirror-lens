// Package v2auth 处理 Registry 的 Bearer 认证：解析 WWW-Authenticate、
// 校验 Realm 白名单、按请求换取 Token。
//
// 两条硬规则：
//  1. Realm 只认按 Host 配置的白名单，绝不直接使用上游 Challenge 里的 Realm。
//  2. 每次资源请求独立走一遍 401 → Challenge → Realm 校验 → Token。
//     不缓存 Token：缓存会让「这个请求该用哪份凭据」变得不可判定。
package v2auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var (
	// ErrRealmDenied 表示 Challenge 给出的 Realm 不在该 Host 的白名单里。
	ErrRealmDenied = errors.New("认证 Realm 不在该 Host 的白名单内")
	// ErrNoRealm 表示 401 响应里没有可用的 Realm。
	ErrNoRealm = errors.New("401 未提供可用的认证 Realm")
	// ErrTokenMethodUnsupported 表示配置要求了尚未实现的 Token 获取方式。
	// 必须显式失败，不能静默按 GET 处理。
	ErrTokenMethodUnsupported = errors.New("Token 获取方式暂不支持")
	// ErrTokenAuthUnsupported 表示 Token 端点的认证方式不在支持范围。
	ErrTokenAuthUnsupported = errors.New("Token 认证方式暂不支持")
	// ErrCredentialsMissing 表示 token_auth=basic 但快照里没有凭据。
	ErrCredentialsMissing = errors.New("token_auth=basic 但快照没有凭据")
)

// Credentials 是某 Host 的一组凭据与 Token 端点配置；零值表示匿名 + GET。
//
// TokenMethod 与 TokenAuth 是凭据模型的一部分，不能在实现层被忽略：
//   - TokenMethod: ""/"GET" 支持；"POST" 暂不支持，显式报错。
//   - TokenAuth:   "" 表示有凭据则 basic、无凭据则 none；
//     "none" 永不附带 Basic（即使快照里残留用户名密码）；
//     "basic" 必须带 Basic，缺凭据时报错。
type Credentials struct {
	Username    string
	Password    string
	TokenMethod string
	TokenAuth   string
}

// Empty 报告是否没有凭据。
func (c Credentials) Empty() bool { return c.Username == "" && c.Password == "" }

// CredentialSnapshot 是 Probe 开始前一次性取得的版本快照。
// Host、Generation、Digest 必须和本次使用的凭据同源，运行中途不得重读。
type CredentialSnapshot struct {
	Host        string
	Generation  int64
	Digest      string
	Credentials Credentials
}

// Challenge 是 WWW-Authenticate: Bearer 的字段。
type Challenge struct {
	Realm   string
	Service string
	Scope   string
}

// ParseChallenge 解析 Bearer Challenge；无法解析时返回零值。
func ParseChallenge(h string) Challenge {
	var c Challenge
	h = strings.TrimSpace(h)
	if h == "" {
		return c
	}
	// 只处理 Bearer；Basic 之类对 Registry 拉取没有意义。
	if i := strings.IndexByte(h, ' '); i > 0 {
		if !strings.EqualFold(h[:i], "Bearer") {
			return Challenge{}
		}
		h = h[i+1:]
	}
	for _, part := range splitParams(h) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		switch k {
		case "realm":
			c.Realm = v
		case "service":
			c.Service = v
		case "scope":
			c.Scope = v
		}
	}
	return c
}

// splitParams 按逗号切分，但忽略引号内的逗号。
func splitParams(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// Authenticator 持有 HTTP 客户端（必须带 safehttp.Guard 的拨号器）和
// Host → Realm 白名单。
type Authenticator struct {
	client *http.Client
	realms map[string][]string
	ua     string
}

// New 构造认证器。realms 的 key 是 repo_host（如 quay.io），value 是允许的
// Realm 前缀，形如 "https://quay.io" 或 "https://*.example.com"。
func New(client *http.Client, realms map[string][]string) *Authenticator {
	if client == nil {
		client = http.DefaultClient
	}
	return &Authenticator{client: client, realms: realms, ua: "mirror-lens/1.0 (+https://github.com/zggnh12345/mirror-lens)"}
}

// DefaultRealms 是内置白名单，只覆盖已知 Registry 的官方认证端点。
func DefaultRealms() map[string][]string {
	return map[string][]string{
		"quay.io":              {"https://quay.io"},
		"gcr.io":               {"https://gcr.io", "https://storage.googleapis.com"},
		"registry.k8s.io":      {"https://registry.k8s.io"},
		"lscr.io":              {"https://lscr.io"},
		"docker.io":            {"https://auth.docker.io"},
		"registry-1.docker.io": {"https://auth.docker.io"},
	}
}

// RealmAllowed 校验 Challenge 的 Realm 是否属于该 Host 的白名单条目。
//
// 只比对 scheme + host（含端口），路径由调用方按 Challenge 原样使用；
// 带 userinfo 的 Realm 一律拒绝。
func (a *Authenticator) RealmAllowed(host string, realm *url.URL) error {
	if realm == nil || realm.Scheme == "" || realm.Host == "" {
		return ErrNoRealm
	}
	if realm.User != nil {
		return fmt.Errorf("%w: Realm 带 userinfo", ErrRealmDenied)
	}
	got := strings.ToLower(realm.Scheme + "://" + realm.Host)
	for _, allow := range a.realms[host] {
		want := strings.ToLower(strings.TrimSuffix(allow, "/"))
		if got == want {
			return nil
		}
		// 支持 https://*.example.com 形式，只允许一级或多级子域。
		if i := strings.Index(want, "://*."); i > 0 {
			prefix := want[:i+3]
			suffix := want[i+5:]
			if strings.HasPrefix(got, prefix) && strings.HasSuffix(got, "."+suffix) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %s 的 Realm %s", ErrRealmDenied, host, got)
}

// Do 发出请求；遇到 401 时按 Challenge 换 Token 后重试一次。
//
// scope 用于 Challenge 没带 scope 的情况（例如 /v2/ 探测）。
// 返回的 Response 由调用方负责关闭。
func (a *Authenticator) Do(ctx context.Context, host string, req *http.Request, snap CredentialSnapshot, scope string) (*http.Response, error) {
	first, err := a.send(ctx, req, "")
	if err != nil {
		return nil, err
	}
	if first.StatusCode != http.StatusUnauthorized {
		return first, nil
	}
	ch := ParseChallenge(first.Header.Get("Www-Authenticate"))
	drainClose(first)
	if ch.Realm == "" {
		return nil, ErrNoRealm
	}
	ru, err := url.Parse(ch.Realm)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoRealm, err)
	}
	if err := a.RealmAllowed(host, ru); err != nil {
		return nil, err
	}
	if ch.Scope == "" {
		ch.Scope = scope
	}
	tok, err := a.token(ctx, ch, snap.Credentials)
	if err != nil {
		return nil, err
	}
	return a.send(ctx, req, tok)
}

func (a *Authenticator) send(ctx context.Context, req *http.Request, token string) (*http.Response, error) {
	r := req.Clone(ctx)
	if req.Body != nil {
		if req.GetBody == nil {
			return nil, errors.New("带 body 的请求必须提供 GetBody 才能重试")
		}
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		r.Body = body
	}
	r.Header.Set("User-Agent", a.ua)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return a.client.Do(r)
}

// tokenAuthMode 落实 token_method/token_auth 契约。
func (c Credentials) tokenAuthMode() (bool, error) {
	switch strings.ToUpper(strings.TrimSpace(c.TokenMethod)) {
	case "", "GET":
	case "POST":
		return false, fmt.Errorf("%w: token_method=POST 尚未实现，不按 GET 降级", ErrTokenMethodUnsupported)
	default:
		return false, fmt.Errorf("%w: token_method=%q", ErrTokenMethodUnsupported, c.TokenMethod)
	}
	switch strings.ToLower(strings.TrimSpace(c.TokenAuth)) {
	case "":
		return !c.Empty(), nil
	case "none":
		return false, nil
	case "basic":
		if c.Empty() {
			return false, ErrCredentialsMissing
		}
		return true, nil
	default:
		return false, fmt.Errorf("%w: token_auth=%q", ErrTokenAuthUnsupported, c.TokenAuth)
	}
}

func (a *Authenticator) token(ctx context.Context, ch Challenge, creds Credentials) (string, error) {
	ru, err := url.Parse(ch.Realm)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoRealm, err)
	}
	q := ru.Query()
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	if ch.Scope != "" {
		q.Set("scope", ch.Scope)
	}
	ru.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", a.ua)
	useBasic, err := creds.tokenAuthMode()
	if err != nil {
		return "", err
	}
	if useBasic {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 Token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("读取 Token 响应: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Token 端点返回 %d", resp.StatusCode)
	}
	var v struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("Token 响应不是 JSON: %w", err)
	}
	tok := v.Token
	if tok == "" {
		tok = v.AccessToken
	}
	if tok == "" {
		return "", errors.New("Token 响应里没有 token")
	}
	return tok, nil
}

func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}
