package v2auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/endpoint"
)

// ErrPolicyMissing 表示 Registry 请求没有可用的认证策略。
//
// 这是 fail closed 的错误：调用方必须返回 503，不能伪装成 401。
var ErrPolicyMissing = errors.New("认证策略不存在")

// PolicySnapshot 是一次请求使用的认证策略快照。
//
// 3.5.11a 只有 LegacyHostPolicy 会生成它；3.5.11b 由 StorePolicy 生成。
// 快照必须随请求 context 走，不能在下游重新按 Host 查表。
type PolicySnapshot struct {
	SourceID    int64
	LogicalHost string
	PolicyGen   int64
	CredGen     int64
	// CredGenKnown 表示 CredGen 来自真实读取的 registry_host_gen。
	// 缺行为 false：凭据认证必须 fail closed，不能默认 0 继续。
	CredGenKnown  bool
	Official      bool
	AllowSameHost bool
	Scheme        string
	Realms        []string

	// AllowPlainHTTP 是 3.5.24 的运行时字段（不是 DB 字段），默认 false。
	// 仅由对应 Source 的 sources.base_url Scheme=http 派生。
	AllowPlainHTTP bool
	// SourceOrigin 是该 Source 的实际连接 Origin（sources.base_url 的
	// Scheme + CanonicalHost + EffectivePort），用于 HTTP Realm 窄例外。
	// 零值表示未知，此时 HTTP Realm 一律拒绝。
	SourceOrigin endpoint.Endpoint
}

// PolicyResolver 把 (source_id, host) 解析成策略快照。
//
// source_id=0 表示官方直连；source_id>0 表示某个候选源。
// 11a 的 LegacyHostPolicy 会忽略 source_id，11b 的 StorePolicy 必须使用它。
type PolicyResolver interface {
	Resolve(ctx context.Context, sourceID int64, host string) (PolicySnapshot, error)
}

// OfficialPolicy 是 sourceID=0 的官方直连策略。
//
// 官方直连保留 3.5.10 的同 Host、同 scheme 快捷放行；这不是第三方
// Source 的 fallback。第三方 Source 只认 StorePolicy 的显式条目。
type OfficialPolicy struct{}

func (OfficialPolicy) Resolve(_ context.Context, sourceID int64, host string) (PolicySnapshot, error) {
	if sourceID != 0 {
		return PolicySnapshot{}, fmt.Errorf("%w: OfficialPolicy 只接受 source_id=0，实际 %d", ErrPolicyMissing, sourceID)
	}
	// 官方直连的 Host 来自已解析的 plan，可能是测试/内网地址（含端口或 IP）。
	// 这里只做基本收敛，不做 registryhost.Normalize——后者的白名单式校验
	// 会把测试和合法内网官方上游一并拒掉。
	logical := strings.ToLower(strings.TrimSpace(host))
	if logical == "" {
		return PolicySnapshot{}, fmt.Errorf("%w: 官方 Host 为空", ErrPolicyMissing)
	}
	return PolicySnapshot{
		SourceID:      sourceID,
		LogicalHost:   logical,
		Official:      true,
		AllowSameHost: true,
		Scheme:        "https",
		Realms:        append([]string(nil), DefaultRealmsFor(logical)...),
	}, nil
}

// 11a 的 Host→Realm 过渡实现已从生产代码移除；仅测试辅助包
// internal/v2auth/v2authtest 保留同等语义，供注入式测试使用。

// Allows 校验 Challenge 的 Realm 是否属于本策略。
//
// 3.5.24 起按 Origin 解析后比较：
//   - HTTPS 同 Host：按 CanonicalHost（去端口）+ Realm Scheme 比较；
//   - HTTP 同 Host：窄例外，必须 AllowPlainHTTP 且 Realm 与 SourceOrigin
//     的完整 Origin（Scheme/CanonicalHost/EffectivePort）完全一致；
//   - 显式白名单：HTTPS 按 CanonicalHost（去端口）+ Scheme 比较，
//     保留跨 Host 兼容能力；HTTP 白名单仍按完整 Origin 比较。
func (p PolicySnapshot) Allows(realm *url.URL) error {
	if realm == nil || realm.Scheme == "" || realm.Host == "" {
		return ErrNoRealm
	}
	if realm.User != nil {
		return fmt.Errorf("%w: Realm 带 userinfo", ErrRealmDenied)
	}
	re, err := endpoint.FromURL(realm)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRealmDenied, err)
	}
	logical, haveLogical := canonicalPolicyHost(p.LogicalHost)
	if p.AllowSameHost && haveLogical && strings.EqualFold(re.Host(), logical) {
		if re.Scheme() == endpoint.HTTP {
			// HTTP Realm 窄例外：只对该 Source 的完整 Origin 生效。
			if !p.AllowPlainHTTP || p.SourceOrigin.IsZero() || !re.Equal(p.SourceOrigin) {
				return fmt.Errorf("%w: HTTP Realm 未被 SourceEndpoint 完整 Origin 授权", ErrRealmDenied)
			}
			return nil
		}
		scheme := p.Scheme
		if scheme == "" {
			scheme = "https"
		}
		if strings.EqualFold(string(re.Scheme()), scheme) {
			return nil
		}
	}
	for _, allow := range p.Realms {
		ae, err := endpoint.Parse(allow)
		if err != nil {
			continue
		}
		if re.Scheme() == endpoint.HTTP || ae.Scheme() == endpoint.HTTP {
			if re.Equal(ae) {
				return nil
			}
			continue
		}
		// HTTPS Realm 身份按 CanonicalHost 比较，端口忽略（冻结裁定 ②）。
		if strings.EqualFold(re.Host(), ae.Host()) && strings.EqualFold(string(re.Scheme()), string(ae.Scheme())) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s 的 Realm %s", ErrRealmDenied, p.LogicalHost, re.Origin())
}

// canonicalPolicyHost 把策略里的 LogicalHost 收敛成 CanonicalHost。
// 官方测试/内网 Host 可能带端口，先按 https://authority 解析。
func canonicalPolicyHost(raw string) (string, bool) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", false
	}
	if ep, err := endpoint.ParseOrigin("https://" + h); err == nil {
		return ep.Host(), true
	}
	return strings.ToLower(h), false
}

type policyCtxKey struct{}
type registryCtxKey struct{}

// WithRegistry 标记这是 Registry 请求；Git/Private/Raw 不能带这个标记。
func WithRegistry(ctx context.Context) context.Context {
	return context.WithValue(ctx, registryCtxKey{}, true)
}

// IsRegistryRequest 判断当前请求是否应走 Registry 认证闸口。
func IsRegistryRequest(ctx context.Context) bool {
	v, _ := ctx.Value(registryCtxKey{}).(bool)
	return v
}

// WithPolicy 把策略快照写入请求 context。
func WithPolicy(ctx context.Context, p PolicySnapshot) context.Context {
	return context.WithValue(WithRegistry(ctx), policyCtxKey{}, p)
}

// PolicyFromContext 读取策略快照；缺失时返回 false。
func PolicyFromContext(ctx context.Context) (PolicySnapshot, bool) {
	p, ok := ctx.Value(policyCtxKey{}).(PolicySnapshot)
	return p, ok
}
