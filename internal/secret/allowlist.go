package secret

import "github.com/zggnh12345/mirror-lens/internal/registryhost"

// HostProfile 是 Host 的凭据能力模型。
//
// 语法规范化（NormalizeHost）和「是否允许凭据」（HostProfileFor）必须分开：
// 前者只回答 Host 是否合法，后者才回答这个 Host 支持哪些认证方式。
// 50 Host 扩展只需要扩 profiles，不要把认证分支写进 Gateway。
type HostProfile struct {
	Host  string
	Modes []AuthMode
}

var hostProfiles = []HostProfile{
	{Host: "gcr.io", Modes: []AuthMode{ModeBasic, ModeBearerTokenBasic, ModeBearerTokenNone}},
	{Host: "quay.io", Modes: []AuthMode{ModeBasic, ModeBearerTokenBasic, ModeBearerTokenNone}},
}

// NormalizeHost 只做语法规范化，不判断 Host 是否在凭据 allowlist 内。
func NormalizeHost(raw string) (string, error) {
	return registryhost.Check(raw)
}

// HostProfiles 返回凭据能力表（深拷贝，调用方不能改内部切片）。
func HostProfiles() []HostProfile {
	out := make([]HostProfile, len(hostProfiles))
	for i, p := range hostProfiles {
		out[i] = p
		out[i].Modes = append([]AuthMode(nil), p.Modes...)
	}
	return out
}

// HostProfileFor 返回 Host 的能力；不在凭据 allowlist 内时 ok=false。
func HostProfileFor(raw string) (HostProfile, bool) {
	h, err := NormalizeHost(raw)
	if err != nil {
		return HostProfile{}, false
	}
	for _, p := range hostProfiles {
		if p.Host == h {
			p.Modes = append([]AuthMode(nil), p.Modes...)
			return p, true
		}
	}
	return HostProfile{}, false
}

// CredentialCapableHosts 返回可配置凭据的 Host（已规范化、排序固定）。
func CredentialCapableHosts() []string {
	out := make([]string, 0, len(hostProfiles))
	for _, p := range hostProfiles {
		out = append(out, p.Host)
	}
	return out
}

// CredentialHostAllowed 报告某 Host 是否在凭据 allowlist 内。
func CredentialHostAllowed(raw string) bool {
	_, ok := HostProfileFor(raw)
	return ok
}
