// Package registryhost 统一 Registry Host 的规范化与白名单。
//
// 为什么单独一个包：3.5.x 里「源归属哪个 Registry」同时被 store（校验写入）、
// probe（探测目标）、adapter（/v2/<host>/ 前缀路由）用到。这三处各写一份
// 规范化逻辑，迟早出现「api 接受 GHCR.IO、调度只认 ghcr.io」的分裂。
package registryhost

import (
	"net"
	"strings"
)

// DefaultHosts 是允许按 /v2/<host>/... 前缀回源的仓库站。
// dpanel / 1Panel 这类工具会把完整镜像地址塞进路径（ghcr.io/owner/repo），
// 不设白名单就等于让 URL 决定回源地址——那是 SSRF。
var defaultHosts = []string{
	"docker.io", "registry-1.docker.io", "ghcr.io", "quay.io",
	"gcr.io", "registry.k8s.io", "mcr.microsoft.com", "lscr.io",
	"registry.gitlab.com", "public.ecr.aws", "cgr.dev",
}

// Hosts 返回白名单副本，供 adapter 复用，避免两处维护同一份列表。
func Hosts() []string {
	out := make([]string, len(defaultHosts))
	copy(out, defaultHosts)
	return out
}

// Normalize 把用户/上游给的 Host 收敛成存储用的规范形式。
//
// Canonical Schema 的约定：
//   - "" 表示没有 Registry 归属，不再是 Docker Hub 的隐式写法；
//   - Docker Hub 必须显式存 "docker.io"；
//   - GHCR 存 "ghcr.io"。
//
// 注意：本函数只做标识规范化，不是 URL 安全校验器——不解析路径、不解析凭据。
func Normalize(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	h = strings.TrimSuffix(h, ".")

	switch h {
	case "":
		return "", nil
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return "docker.io", nil
	}

	if err := checkHost(h); err != nil {
		return "", err
	}
	return h, nil
}

func checkHost(h string) error {
	if strings.ContainsAny(h, "/\\@?# \t\r\n") {
		return &InvalidError{Host: h, Reason: "包含协议、路径或非法字符"}
	}
	if strings.Contains(h, ":") {
		return &InvalidError{Host: h, Reason: "不允许带端口"}
	}
	if len(h) > 253 {
		return &InvalidError{Host: h, Reason: "过长"}
	}
	// IP 字面量不能作为 Registry Host：它绕过域名绑定，也让证书校验失去意义。
	if net.ParseIP(h) != nil {
		return &InvalidError{Host: h, Reason: "不允许使用 IP 地址"}
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return &InvalidError{Host: h, Reason: "域名标签非法"}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return &InvalidError{Host: h, Reason: "域名标签不能以 - 开头或结尾"}
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return &InvalidError{Host: h, Reason: "域名含非法字符"}
			}
		}
	}
	return nil
}

// InvalidError 表示 Host 格式不合法。
type InvalidError struct {
	Host   string
	Reason string
}

func (e *InvalidError) Error() string {
	return "Registry Host 不合法（" + e.Host + "）：" + e.Reason
}

// Allowed 判断规范化后的 Host 是否在可回源白名单内。
// 空串（Docker Hub）始终允许。
func Allowed(host string) bool {
	if host == "" {
		return true
	}
	for _, x := range defaultHosts {
		if x == host {
			return true
		}
	}
	return false
}

// Check 一步完成规范化 + 白名单校验，写入路径都应该走它。
func Check(raw string) (string, error) {
	h, err := Normalize(raw)
	if err != nil {
		return "", err
	}
	if !Allowed(h) {
		return "", &UnsupportedError{Host: h}
	}
	return h, nil
}

// UnsupportedError 表示格式合法但当前版本还不支持回源到该 Registry。
type UnsupportedError struct{ Host string }

func (e *UnsupportedError) Error() string {
	return "暂不支持该 Registry Host（" + e.Host + "），需先加入回源白名单"
}
