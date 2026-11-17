package v2auth

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

func officialCtx(t *testing.T, host string, gen int64, known bool) context.Context {
	t.Helper()
	id, err := NewOfficialIdentity(host)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithIdentity(context.Background(), id)
	return WithPolicy(ctx, PolicySnapshot{
		SourceID: 0, LogicalHost: host, Official: true,
		CredGen: gen, CredGenKnown: known,
	})
}

func snap(host string, gen int64, c Credentials) CredentialSnapshot {
	return CredentialSnapshot{Host: host, Generation: gen, HasCredential: true, Credentials: c}
}

// S01：只有 Official + SourceID=0 + Host/gen 一致才允许使用凭据。
func TestAuthorizeCredentialRequiresOfficial(t *testing.T) {
	cred := Credentials{Username: "u", Password: "p", Mode: "basic"}
	ctx := officialCtx(t, "quay.io", 3, true)
	if err := AuthorizeCredential(ctx, snap("quay.io", 3, cred)); err != nil {
		t.Fatalf("合法官方请求应通过: %v", err)
	}

	// S15：零值身份（Unknown）一律拒绝。
	if err := AuthorizeCredential(context.Background(), snap("quay.io", 3, cred)); !errors.Is(err, ErrIdentityUnknown) {
		t.Fatalf("零值身份应拒绝，得到 %v", err)
	}
	// 候选身份不得使用凭据（S02/S04）。
	cid, err := NewCandidateIdentity(7, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	cctx := WithPolicy(WithIdentity(context.Background(), cid), PolicySnapshot{
		SourceID: 7, LogicalHost: "quay.io", Official: false, CredGen: 3, CredGenKnown: true,
	})
	if err := AuthorizeCredential(cctx, snap("quay.io", 3, cred)); !errors.Is(err, ErrIdentityNotOfficial) {
		t.Fatalf("候选身份应拒绝，得到 %v", err)
	}
	// Host 不一致。
	if err := AuthorizeCredential(ctx, snap("gcr.io", 3, cred)); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("Host 不一致应拒绝，得到 %v", err)
	}
	// gen 不一致（S11）。
	if err := AuthorizeCredential(ctx, snap("quay.io", 2, cred)); !errors.Is(err, ErrCredentialStale) {
		t.Fatalf("gen 不一致应拒绝，得到 %v", err)
	}
	// gen 未知（读不到 registry_host_gen）不得默认 0。
	if err := AuthorizeCredential(officialCtx(t, "quay.io", 0, false), snap("quay.io", 0, cred)); !errors.Is(err, ErrCredentialStale) {
		t.Fatalf("gen 未知应拒绝，得到 %v", err)
	}
	// 没有凭据。
	empty := CredentialSnapshot{Host: "quay.io", Generation: 3}
	if err := AuthorizeCredential(ctx, empty); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("无凭据应拒绝，得到 %v", err)
	}
	// 模式与字段不一致。
	if err := AuthorizeCredential(ctx, snap("quay.io", 3, Credentials{Mode: "basic"})); !errors.Is(err, ErrCredentialModeInvalid) {
		t.Fatalf("basic 缺密码应拒绝，得到 %v", err)
	}
	if err := AuthorizeCredential(ctx, snap("quay.io", 3, Credentials{Mode: "bearer_token_none"})); !errors.Is(err, ErrCredentialModeInvalid) {
		t.Fatalf("bearer_token_none 未标记应拒绝，得到 %v", err)
	}
}

// S16：普通调用方无法字面量构造 Official。
func TestIdentityConstructors(t *testing.T) {
	var zero Identity
	if zero.Kind() != IdentityUnknown {
		t.Fatalf("零值必须是 Unknown，实际 %v", zero.Kind())
	}
	if _, err := NewOfficialIdentity(""); err == nil {
		t.Fatal("空 Host 不能构造官方身份")
	}
	if _, err := NewCandidateIdentity(0, "quay.io"); err == nil {
		t.Fatal("候选身份必须带 source_id>0")
	}
	if _, err := NewCandidateIdentity(-1, "quay.io"); err == nil {
		t.Fatal("负 source_id 必须拒绝")
	}
	id, err := NewOfficialIdentity("QUAY.IO")
	if err != nil || id.Host() != "quay.io" || id.SourceID() != 0 || !id.Official() {
		t.Fatalf("官方身份规范化不对: %+v err=%v", id, err)
	}
}

func u(t *testing.T, s string) *url.URL {
	t.Helper()
	x, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// S07/S18：Registry 跨 Host 清 Authorization；HTTPS 降级拒绝。
func TestRedirectRegistryRules(t *testing.T) {
	from := u(t, "https://quay.io/v2/a/manifests/1")
	same := u(t, "https://quay.io/v2/a/manifests/2")
	cross := u(t, "https://cdn.example/x")
	d, err := RedirectPolicy(RedirectRegistry, "GET", from, same, 1)
	if err != nil || d.StripAuth || !d.RecheckPolicy {
		t.Fatalf("同 Host 应保留认证并复检策略: %+v err=%v", d, err)
	}
	d, err = RedirectPolicy(RedirectRegistry, "GET", from, cross, 1)
	if err != nil || !d.StripAuth {
		t.Fatalf("跨 Host 必须清 Authorization: %+v err=%v", d, err)
	}
	if _, err := RedirectPolicy(RedirectRegistry, "GET", from, u(t, "http://quay.io/x"), 1); err == nil {
		t.Fatal("HTTPS→HTTP 必须拒绝")
	}
	if _, err := RedirectPolicy(RedirectRegistry, "GET", from, same, 6); err == nil {
		t.Fatal("Registry 最多 5 跳")
	}
}

// S17：Token 端点只允许同源，跨源直接拒绝。
func TestRedirectTokenRules(t *testing.T) {
	from := u(t, "https://quay.io/token")
	if _, err := RedirectPolicy(RedirectToken, "GET", from, u(t, "https://evil.example/token"), 1); err == nil {
		t.Fatal("Token 跨源必须拒绝")
	}
	if _, err := RedirectPolicy(RedirectToken, "GET", from, u(t, "https://quay.io/token2"), 4); err == nil {
		t.Fatal("Token 最多 3 跳")
	}
	if _, err := RedirectPolicy(RedirectToken, "GET", from, u(t, "http://quay.io/token"), 1); err == nil {
		t.Fatal("Token 端点必须 HTTPS")
	}
}

// S19：CDN 只允许 GET/HEAD，且必须清认证。
func TestRedirectCDNRules(t *testing.T) {
	from := u(t, "https://quay.io/v2/a/blobs/sha256:1")
	to := u(t, "https://cdn.example/blob?X-Amz-Signature=x")
	d, err := RedirectPolicy(RedirectCDN, "GET", from, to, 2)
	if err != nil || !d.StripAuth {
		t.Fatalf("CDN 重定向应清认证: %+v err=%v", d, err)
	}
	if _, err := RedirectPolicy(RedirectCDN, "POST", from, to, 1); err == nil {
		t.Fatal("CDN 只允许 GET/HEAD")
	}
	if _, err := RedirectPolicy(RedirectCDN, "GET", from, u(t, "http://cdn.example/x"), 1); err == nil {
		t.Fatal("CDN 必须 HTTPS")
	}
	if _, err := RedirectPolicy(RedirectCDN, "GET", from, to, 6); err == nil {
		t.Fatal("CDN 最多 5 跳")
	}
}
