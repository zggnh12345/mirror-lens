package v2auth

import (
	"context"
	"fmt"
)

// LegacyHostPolicy 复刻 3.5.10 的 Host→Realm 行为，仅存在于测试代码。
//
// 生产装配只允许 StorePolicy。保留这个类型是为了让 11a 的边界断言
// （userinfo 拒绝、AllowSameHost、默认 Realm）继续有覆盖。
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
	return PolicySnapshot{
		SourceID:      sourceID,
		LogicalHost:   host,
		Official:      sourceID == 0,
		AllowSameHost: p.AllowSameHost,
		Realms:        append([]string(nil), allows...),
	}, nil
}
