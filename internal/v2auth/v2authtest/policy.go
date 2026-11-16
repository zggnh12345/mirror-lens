// Package v2authtest 提供仅测试使用的 PolicyResolver 实现。
//
// 它复刻 3.5.10 的 Host→Realm 行为，供 gateway / probe / runner 的
// 注入式测试使用。生产代码不得导入本包。
package v2authtest

import (
	"context"
	"fmt"

	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

type Policy struct {
	Realms        map[string][]string
	AllowSameHost bool
}

func New(realms map[string][]string, allowSameHost bool) *Policy {
	return &Policy{Realms: realms, AllowSameHost: allowSameHost}
}

func (p *Policy) Resolve(_ context.Context, sourceID int64, host string) (v2auth.PolicySnapshot, error) {
	if sourceID < 0 {
		return v2auth.PolicySnapshot{}, fmt.Errorf("%w: source_id=%d", v2auth.ErrPolicyMissing, sourceID)
	}
	allows := p.Realms[host]
	if len(allows) == 0 {
		allows = v2auth.DefaultRealmsFor(host)
	}
	return v2auth.PolicySnapshot{
		SourceID:      sourceID,
		LogicalHost:   host,
		Official:      sourceID == 0,
		AllowSameHost: p.AllowSameHost,
		Realms:        append([]string(nil), allows...),
	}, nil
}
