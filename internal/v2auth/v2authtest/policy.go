// Package v2authtest 提供仅测试使用的 PolicyResolver 实现。
//
// 它复刻 3.5.10 的 Host→Realm 行为，供 gateway / probe / runner 的
// 注入式测试使用。生产代码不得导入本包。
package v2authtest

import (
	"context"
	"fmt"

	"github.com/zggnh12345/mirror-lens/internal/endpoint"
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
	snap := v2auth.PolicySnapshot{
		SourceID:      sourceID,
		LogicalHost:   host,
		Official:      sourceID == 0,
		AllowSameHost: p.AllowSameHost,
		Realms:        append([]string(nil), allows...),
	}
	// 复刻生产 gateway 的运行时派生：白名单里登记 HTTP Realm 说明被测
	// Source 回源地址是 HTTP，其 token Realm 位于同源。让注入式测试与
	// 生产语义一致，避免缺 SourceOrigin 把合法 Token 机制测试拒掉。
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
