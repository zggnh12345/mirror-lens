package v2auth

import (
	"context"
	"net/url"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLegacyHostPolicySameHostOnlyWithFlag(t *testing.T) {
	host := "registry.example"
	p, err := NewLegacyHostPolicy(nil, true).Resolve(context.Background(), 7, host)
	if err != nil {
		t.Fatal(err)
	}
	p.Scheme = "https"
	if err := p.Allows(mustURL(t, "https://registry.example/token")); err != nil {
		t.Fatalf("同 Host/同 scheme 应放行: %v", err)
	}
	if err := p.Allows(mustURL(t, "http://registry.example/token")); err == nil {
		t.Fatal("同 Host 不同 scheme 不应放行")
	}

	noShortcut, err := NewLegacyHostPolicy(nil, false).Resolve(context.Background(), 7, host)
	if err != nil {
		t.Fatal(err)
	}
	noShortcut.Scheme = "https"
	if err := noShortcut.Allows(mustURL(t, "https://registry.example/token")); err == nil {
		t.Fatal("未启用同 Host 快捷放行时不应放行")
	}
}

func TestLegacyHostPolicyRejectsUserinfo(t *testing.T) {
	p, err := NewLegacyHostPolicy(map[string][]string{"registry.example": {"https://auth.example"}}, false).Resolve(context.Background(), 1, "registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Allows(mustURL(t, "https://user:pass@auth.example/token")); err == nil {
		t.Fatal("带 userinfo 的 Realm 必须拒绝")
	}
}

func TestLegacyHostPolicyWildcard(t *testing.T) {
	p, err := NewLegacyHostPolicy(map[string][]string{"registry.example": {"https://*.example"}}, false).Resolve(context.Background(), 1, "registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Allows(mustURL(t, "https://auth.example/token")); err == nil {
		t.Fatal("策略禁止通配 Realm：auth.example 必须拒绝")
	}
	if err := p.Allows(mustURL(t, "https://example/token")); err == nil {
		t.Fatal("策略禁止通配 Realm：根域也必须拒绝")
	}
}

// 上游 challenge 的 Realm 天生带 /token 等 path（Docker Hub / GHCR 都是）。
// Allows 必须按 scheme+host 归一化比较：策略条目侧由 store.normalizeRealm 拒绝带
// path/query/fragment 的输入；运行时若把 path 当拒绝条件，会让所有官方拉取 fail-closed。
func TestAllowsNormalizesChallengeRealm(t *testing.T) {
	p := PolicySnapshot{Realms: []string{"https://auth.docker.io"}}
	for _, raw := range []string{
		"https://auth.docker.io/token",
		"https://auth.docker.io/token?service=registry.docker.io",
		"https://auth.docker.io/token#frag",
	} {
		if err := p.Allows(mustURL(t, raw)); err != nil {
			t.Fatalf("带 path/query/fragment 的 challenge realm 应归一化放行 %q: %v", raw, err)
		}
	}
	// 归一化不能放宽边界：换 Host 或换 scheme 仍必须拒绝。
	for _, raw := range []string{
		"https://evil.example/token",
		"http://auth.docker.io/token",
	} {
		if err := p.Allows(mustURL(t, raw)); err == nil {
			t.Fatalf("越权 realm 不应放行: %q", raw)
		}
	}
}

func TestPolicyContextRoundTrip(t *testing.T) {
	p := PolicySnapshot{SourceID: 42, LogicalHost: "quay.io", PolicyGen: 3, CredGen: 4}
	ctx := WithPolicy(context.Background(), p)
	got, ok := PolicyFromContext(ctx)
	if !ok || got.SourceID != 42 || got.LogicalHost != "quay.io" || got.PolicyGen != 3 || got.CredGen != 4 {
		t.Fatalf("策略快照未完整往返: %+v ok=%v", got, ok)
	}
	if _, ok := PolicyFromContext(context.Background()); ok {
		t.Fatal("空 context 不应有策略")
	}
}
