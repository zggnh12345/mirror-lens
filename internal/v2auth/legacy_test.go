package v2auth

import (
	"context"
	"fmt"

	"github.com/zggnh12345/mirror-lens/internal/endpoint"
)

// LegacyHostPolicy 复刻 3.5.10 的 Host→Realm 行为，仅存在于测试代码。
//
// 生产装配只允许 StorePolicy。保留这个类型是为了让 11a 的边界断言
// （userinfo 拒绝、AllowSameHost、默认 Realm）继续有覆盖。
//
// 3.5.24：Resolve 额外复刻生产 gateway 的运行时派生——若白名单里登记了
// HTTP Realm，说明被测 Source 的回源地址是 HTTP，其 token Realm 位于
// 同源。这正是「HTTP Source + 同 Origin HTTP Realm」的合法形态，避免用
// 缺 SourceOrigin 的人工快照把合法 Token 机制测试全部拒掉。P1 安全边界
// 本身由 allows_production_test.go 用显式字段覆盖。
type LegacyHostPolicy struct {
	Realms        map[string][]string
	AllowSameHost bool
}

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
	snap := PolicySnapshot{
		SourceID:      sourceID,
		LogicalHost:   host,
		Official:      sourceID == 0,
		AllowSameHost: p.AllowSameHost,
		Realms:        append([]string(nil), allows...),
	}
	for _, a := range allows {
		ep, err := endpoint.ParseOrigin(a)
		if err != nil || ep.Scheme() != endpoint.HTTP {
			continue
		}
		snap.AllowPlainHTTP = true
		snap.SourceOrigin = ep
		break
	}
	return snap, nil
}
