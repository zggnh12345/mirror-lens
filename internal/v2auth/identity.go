package v2auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// IdentityKind 是请求身份的唯一权威分类。
//
// 零值必须是 Unknown，一律拒绝凭据：只有显式构造的 Official 才可能通过
// 认证门禁。Candidate 永远拿不到官方凭据或官方私有 Token。
type IdentityKind uint8

const (
	IdentityUnknown IdentityKind = iota
	IdentityOfficial
	IdentityCandidate
)

func (k IdentityKind) String() string {
	switch k {
	case IdentityOfficial:
		return "official"
	case IdentityCandidate:
		return "candidate"
	default:
		return "unknown"
	}
}

// Identity 是不可由普通调用方随意构造的身份对象：字段私有，只能通过
// 受控构造函数创建。仅导出一个枚举字段不够——内部代码仍可字面量写
// Official，那正是 S04/S16 要防的。
type Identity struct {
	kind     IdentityKind
	host     string
	sourceID int64
}

// NewOfficialIdentity 只能由官方直连入口调用：SourceID 恒为 0，
// Host 来自已解析的 plan。
func NewOfficialIdentity(host string) (Identity, error) {
	h := normalizeIdentityHost(host)
	if h == "" {
		return Identity{}, errors.New("官方身份缺少 Host")
	}
	return Identity{kind: IdentityOfficial, host: h, sourceID: 0}, nil
}

// NewCandidateIdentity 只能由候选 Source 入口调用；sourceID 必须 > 0。
func NewCandidateIdentity(sourceID int64, host string) (Identity, error) {
	if sourceID <= 0 {
		return Identity{}, fmt.Errorf("候选身份必须带 source_id>0，实际 %d", sourceID)
	}
	h := normalizeIdentityHost(host)
	if h == "" {
		return Identity{}, errors.New("候选身份缺少 Host")
	}
	return Identity{kind: IdentityCandidate, host: h, sourceID: sourceID}, nil
}

func (i Identity) Kind() IdentityKind { return i.kind }
func (i Identity) Host() string       { return i.host }
func (i Identity) SourceID() int64    { return i.sourceID }
func (i Identity) Official() bool     { return i.kind == IdentityOfficial }

func normalizeIdentityHost(raw string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), "/")
}

type identityCtxKey struct{}

// WithIdentity 把身份写进请求 context。整个请求生命周期只写一次。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext 读取身份；缺失时返回零值（Unknown）。
func IdentityFromContext(ctx context.Context) Identity {
	if v, ok := ctx.Value(identityCtxKey{}).(Identity); ok {
		return v
	}
	return Identity{}
}

// 认证门禁的稳定错误码。API 只返回这些码，不返回上游响应体。
var (
	ErrIdentityUnknown        = errors.New("auth_identity_unknown")
	ErrIdentityNotOfficial    = errors.New("auth_identity_not_official")
	ErrCredentialHostMismatch = errors.New("auth_host_mismatch")
	ErrCredentialStale        = errors.New("auth_stale")
	ErrCredentialUnavailable  = errors.New("auth_unavailable")
	ErrCredentialModeInvalid  = errors.New("auth_mode_invalid")
)

// AuthorizeCredential 是凭据读取处与最终认证执行处的唯一门禁。
//
// 必须同时成立（1.6 15-C.1）：
//   - IdentityKind=Official；
//   - SourceID=0；
//   - PolicySnapshot.Official=true；
//   - snap.Host == policy.LogicalHost（规范化后）；
//   - snap.HasCredential=true；
//   - snap.Generation == policy.CredGen（且 CredGen 来自真实读取）。
func AuthorizeCredential(ctx context.Context, snap CredentialSnapshot) error {
	id := IdentityFromContext(ctx)
	if id.kind == IdentityUnknown {
		return ErrIdentityUnknown
	}
	if !id.Official() || id.sourceID != 0 {
		return ErrIdentityNotOfficial
	}
	policy, ok := PolicyFromContext(ctx)
	if !ok {
		return ErrPolicyMissing
	}
	if !policy.Official || policy.SourceID != 0 {
		return ErrIdentityNotOfficial
	}
	snapHost := normalizeIdentityHost(snap.Host)
	policyHost := normalizeIdentityHost(policy.LogicalHost)
	if snapHost == "" || policyHost == "" || snapHost != policyHost || snapHost != id.host {
		return fmt.Errorf("%w: 凭据 %q / 策略 %q / 身份 %q", ErrCredentialHostMismatch, snap.Host, policy.LogicalHost, id.host)
	}
	if !snap.HasCredential {
		return ErrCredentialUnavailable
	}
	if !policy.CredGenKnown {
		// 读不到 generation 不得默认 0 继续认证。
		return fmt.Errorf("%w: %s 没有 registry_host_gen 记录", ErrCredentialStale, policyHost)
	}
	if snap.Generation != policy.CredGen {
		return fmt.Errorf("%w: 快照 gen=%d，策略 gen=%d", ErrCredentialStale, snap.Generation, policy.CredGen)
	}
	return snap.Credentials.validateMode()
}

// validateMode 校验「凭据状态与认证模式一致」。
func (c Credentials) validateMode() error {
	switch c.AuthMode() {
	case ModeBasic:
		if c.Username == "" || c.Password == "" {
			return fmt.Errorf("%w: basic 缺少用户名/密码", ErrCredentialModeInvalid)
		}
		if c.TokenMethod != "" || c.TokenAuth != "" {
			return fmt.Errorf("%w: basic 不接受 TokenMethod/TokenAuth", ErrCredentialModeInvalid)
		}
	case ModeBearerTokenBasic:
		if c.Username == "" || c.Password == "" {
			return fmt.Errorf("%w: bearer_token_basic 缺少用户名/密码", ErrCredentialModeInvalid)
		}
		if c.TokenAuth == "none" {
			return fmt.Errorf("%w: bearer_token_basic 与 none 矛盾", ErrCredentialModeInvalid)
		}
	case ModeBearerTokenNone:
		if c.TokenAuth != "none" {
			return fmt.Errorf("%w: bearer_token_none 未显式标记", ErrCredentialModeInvalid)
		}
	case ModeInvalid:
		return fmt.Errorf("%w: 未知认证模式", ErrCredentialModeInvalid)
	}
	return nil
}

// AuthMode 是 v2auth 侧的认证模式枚举，与 secret.AuthMode 取值一致。
type AuthMode string

const (
	ModeBasic            AuthMode = "basic"
	ModeBearerTokenBasic AuthMode = "bearer_token_basic"
	ModeBearerTokenNone  AuthMode = "bearer_token_none"
	ModeInvalid          AuthMode = ""
)

// AuthMode 派生凭据模式；历史记录按「有凭据 = Token 端点 Basic」解释。
func (c Credentials) AuthMode() AuthMode {
	if m := strings.TrimSpace(c.Mode); m != "" {
		switch AuthMode(m) {
		case ModeBasic, ModeBearerTokenBasic, ModeBearerTokenNone:
			return AuthMode(m)
		}
		return ModeInvalid
	}
	if c.Username == "" && c.Password == "" {
		if strings.EqualFold(strings.TrimSpace(c.TokenAuth), "none") {
			return ModeBearerTokenNone
		}
		return ModeInvalid
	}
	return ModeBearerTokenBasic
}

// 重定向策略（1.6 15-C.3）。

// RedirectKind 区分三类请求的重定向规则。
type RedirectKind uint8

const (
	RedirectRegistry RedirectKind = iota + 1
	RedirectToken
	RedirectCDN
)

// RedirectDecision 告诉调用方这一跳允许后必须做什么。
type RedirectDecision struct {
	// StripAuth：跨 Host 或跨 Scheme 时必须清除 Authorization。
	StripAuth bool
	// RecheckPolicy：同 Host 也要重新校验策略（Registry）。
	RecheckPolicy bool
}

// RedirectPolicy 是唯一的重定向裁决入口。
//
// 规则：
//   - Registry：最多 5 跳，只允许 HTTPS，Host/Scheme 变化时清 Authorization；
//   - Token：最多 3 跳，只允许同源 HTTPS，跨源直接拒绝；
//   - CDN：最多 5 跳，只允许 GET/HEAD + HTTPS，不携带 Authorization。
func RedirectPolicy(kind RedirectKind, method string, from, to *url.URL, hop int) (RedirectDecision, error) {
	if to == nil || from == nil {
		return RedirectDecision{}, errors.New("重定向目标为空")
	}
	limit := 5
	switch kind {
	case RedirectRegistry:
		limit = 5
	case RedirectToken:
		limit = 3
	case RedirectCDN:
		limit = 5
	default:
		return RedirectDecision{}, errors.New("未知的重定向类型")
	}
	if hop < 1 || hop > limit {
		return RedirectDecision{}, fmt.Errorf("重定向超过 %d 跳", limit)
	}
	if to.User != nil {
		return RedirectDecision{}, errors.New("重定向目标带 userinfo")
	}
	if !strings.EqualFold(to.Scheme, "https") {
		return RedirectDecision{}, fmt.Errorf("重定向目标不是 HTTPS: %s", to.Scheme)
	}
	if !strings.EqualFold(from.Scheme, "https") {
		// 上游本来就不是 HTTPS（测试/内网）时不做降级判断，但也只允许 https 目标。
	}
	if strings.EqualFold(from.Scheme, "https") && !strings.EqualFold(to.Scheme, "https") {
		return RedirectDecision{}, errors.New("拒绝 HTTPS 降级重定向")
	}
	sameOrigin := strings.EqualFold(from.Scheme, to.Scheme) && strings.EqualFold(from.Host, to.Host)
	switch kind {
	case RedirectToken:
		if !sameOrigin {
			return RedirectDecision{}, fmt.Errorf("Token 端点只允许同源重定向: %s -> %s", from.Host, to.Host)
		}
		return RedirectDecision{StripAuth: false, RecheckPolicy: false}, nil
	case RedirectCDN:
		if method != "" && !strings.EqualFold(method, "GET") && !strings.EqualFold(method, "HEAD") {
			return RedirectDecision{}, fmt.Errorf("CDN 重定向只允许 GET/HEAD，实际 %s", method)
		}
		return RedirectDecision{StripAuth: true, RecheckPolicy: false}, nil
	default:
		return RedirectDecision{StripAuth: !sameOrigin, RecheckPolicy: true}, nil
	}
}
