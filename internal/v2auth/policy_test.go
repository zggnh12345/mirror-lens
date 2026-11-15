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
	if err := p.Allows(mustURL(t, "https://auth.example/token")); err != nil {
		t.Fatalf("通配子域应放行: %v", err)
	}
	if err := p.Allows(mustURL(t, "https://example/token")); err == nil {
		t.Fatal("通配不应放行根域")
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
