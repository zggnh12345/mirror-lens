package v2auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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
	SourceID      int64
	LogicalHost   string
	PolicyGen     int64
	CredGen       int64
	Official      bool
	AllowSameHost bool
	Scheme        string
	Realms        []string
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
func (p PolicySnapshot) Allows(realm *url.URL) error {
	if realm == nil || realm.Scheme == "" || realm.Host == "" {
		return ErrNoRealm
	}
	if realm.User != nil {
		return fmt.Errorf("%w: Realm 带 userinfo", ErrRealmDenied)
	}
	got := strings.ToLower(realm.Scheme + "://" + realm.Host)
	if p.AllowSameHost && p.LogicalHost != "" {
		scheme := p.Scheme
		if scheme == "" {
			scheme = "https"
		}
		if strings.EqualFold(realm.Host, p.LogicalHost) && strings.EqualFold(realm.Scheme, scheme) {
			return nil
		}
	}
	for _, allow := range p.Realms {
		want := strings.ToLower(strings.TrimSuffix(allow, "/"))
		if got == want {
			return nil
		}
	}
	return fmt.Errorf("%w: %s 的 Realm %s", ErrRealmDenied, p.LogicalHost, got)
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
