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

// LegacyHostPolicy 复刻 3.5.10 的 Host→Realm 行为。
//
// 四段语义：
//  1. 带 userinfo 的 Realm 一律拒绝；
//  2. AllowSameHost 时，同 Host、同 scheme 的 Realm 放行；
//  3. 否则查显式表；显式表为空时查 DefaultRealmsFor；
//  4. 支持 https://*.example.com 通配。
//
// 11a 里 Gateway 用 AllowSameHost=true 复刻 realmAllowedForHost；
// Probe 用 false 复刻 v2auth.Authenticator.RealmAllowed 的旧行为。
// 11b 删除这个类型后，两端都只剩显式策略。
type LegacyHostPolicy struct {
	Realms        map[string][]string
	AllowSameHost bool
}

// NewLegacyHostPolicy 构造 11a 的过渡策略。
func NewLegacyHostPolicy(realms map[string][]string, allowSameHost bool) *LegacyHostPolicy {
	return &LegacyHostPolicy{Realms: realms, AllowSameHost: allowSameHost}
}

func (p *LegacyHostPolicy) Resolve(_ context.Context, sourceID int64, host string) (PolicySnapshot, error) {
	if sourceID < 0 {
		return PolicySnapshot{}, fmt.Errorf("%w: source_id=%d", ErrPolicyMissing, sourceID)
	}
	allows := p.Realms[host]
	if len(allows) == 0 {
		allows = DefaultRealmsFor(host)
	}
	return PolicySnapshot{
		SourceID:      sourceID,
		LogicalHost:   host,
		Official:      sourceID == 0,
		AllowSameHost: p.AllowSameHost,
		Realms:        append([]string(nil), allows...),
	}, nil
}

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
		if i := strings.Index(want, "://*."); i > 0 {
			prefix := want[:i+3]
			suffix := want[i+5:]
			if strings.HasPrefix(got, prefix) && strings.HasSuffix(got, "."+suffix) {
				return nil
			}
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
