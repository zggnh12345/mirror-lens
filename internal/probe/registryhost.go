package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// 独立 Registry Host 探测（3.5.0）。
//
// 它和现有的 Docker()/Ghcr() 是两套东西：那些是业务回源用的镜像源探测，
// 这里只回答「某个 repo_host 在该源上、按某种路径格式，能不能真的取到
// 内容且内容和官方一致」。不写库、不改源状态、不参与调度。

// RegistryHostProbe 是一次 Host 探测的全部输入。
type RegistryHostProbe struct {
	BaseURL string // 源根地址，如 https://quay.mirror.example
	Host    string // repo_host，如 quay.io
	Repo    string // 探测用仓库，如 library/alpine
	Tag     string // 默认 latest
	// ExpectedDigest 是该 tag 的官方 manifest 摘要（sha256:...）。为空时只验
	// 内容自洽，不宣称与官方一致。
	ExpectedDigest string
	// Snapshot 在探测开始前一次性取好，运行期间不重新读凭据。
	// 它的 Generation/Digest 与 store 的一致性由调用方（Runner）负责，
	// 本层只保证「一次探测只用这一份快照」。
	Snapshot v2auth.CredentialSnapshot
	// Realms 是 Host → Realm 白名单；为空时用 v2auth.DefaultRealms()。
	Realms  map[string][]string
	Timeout time.Duration
}

// PathOutcome 是一条路径格式的独立结论。
type PathOutcome struct {
	Mode      string `json:"mode"` // native / host_prefix
	Path      string `json:"path"`
	Verdict   string `json:"verdict,omitempty"`
	Cap       int    `json:"cap"`
	HTTPCode  int    `json:"http_code,omitempty"`
	Digest    string `json:"digest,omitempty"`
	DigestOK  int    `json:"digest_ok"` // -1 未比对 / 0 与官方不同 / 1 一致
	Tampered  bool   `json:"tampered,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// RegistryHostOutcome 是一个 Host 的综合结论。
type RegistryHostOutcome struct {
	Host       string      `json:"host"`
	Native     PathOutcome `json:"native"`
	HostPrefix PathOutcome `json:"host_prefix"`
	Verdict    string      `json:"verdict,omitempty"`
	Cap        int         `json:"cap"`
	Warnings   []string    `json:"warnings,omitempty"`
}

const defaultManifestCap = 8 << 20

// 与 store.RegistryCap* 同一套取值；这里不 import store，避免探测层被
// 数据库依赖拖进去。测试里有一条断言锁死两边的一致性。
const (
	modeNative     = "native"
	modeHostPrefix = "host_prefix"
	capUnknown     = -1
	capNo          = 0
	capYes         = 1
	capMismatch    = 2
)

// ProbeRegistryHost 独立验证 native 与 host_prefix 两种路径。
//
// 结论映射（与 registry_cap 一致）：
//
//	1 = 取到内容，且摘要与自身声明/官方基准一致
//	2 = 取到内容但摘要不一致（硬排除）
//	0 = 有明确证据表明该路径格式不被支持（或根本不是 registry）
//
// -1 = 其余一切：超时、5xx、401、双 404……都说明不了能力，不写 0
func ProbeRegistryHost(ctx context.Context, p RegistryHostProbe) RegistryHostOutcome {
	out := RegistryHostOutcome{Host: p.Host, Cap: capUnknown}
	host, err := registryhost.Check(p.Host)
	if err != nil {
		out.Native.Error, out.HostPrefix.Error = err.Error(), err.Error()
		return out
	}
	if host == "" || host == "ghcr.io" {
		err := fmt.Errorf("%s 不进入 Host 探测（Hub/ghcr 有单独的探测与调度）", host)
		out.Native.Error, out.HostPrefix.Error = err.Error(), err.Error()
		return out
	}
	if p.Snapshot.Host != "" && p.Snapshot.Host != host {
		err := fmt.Errorf("凭据快照属于 %s，与本次探测的 %s 不一致", p.Snapshot.Host, host)
		out.Native.Error, out.HostPrefix.Error = err.Error(), err.Error()
		return out
	}
	if p.Repo == "" {
		p.Repo = "library/alpine"
	}
	if p.Tag == "" {
		p.Tag = "latest"
	}
	if p.Timeout <= 0 {
		p.Timeout = 25 * time.Second
	}
	realms := p.Realms
	if realms == nil {
		realms = v2auth.DefaultRealms()
	}
	auth := v2auth.New(newClient(p.Timeout), realms)

	out.Native = probePath(ctx, auth, p, false)
	out.HostPrefix = probePath(ctx, auth, p, true)
	out.Verdict, out.Cap = mergeOutcome(out.Native, out.HostPrefix)

	// 双 404 是很常见的「源不认这个仓库」，不是「不支持该路径格式」。
	if out.Native.HTTPCode == http.StatusNotFound && out.HostPrefix.HTTPCode == http.StatusNotFound {
		out.Warnings = append(out.Warnings, "两条路径均 404：只作负面证据，不写 cap=0")
	}
	return out
}

func probePath(ctx context.Context, auth *v2auth.Authenticator, p RegistryHostProbe, prefix bool) PathOutcome {
	mode := modeNative
	if prefix {
		mode = modeHostPrefix
	}
	o := PathOutcome{Mode: mode, Cap: capUnknown, DigestOK: -1}
	base := trimBase(p.BaseURL)
	if base == "" {
		o.Error = "源地址为空"
		return o
	}
	// 每条路径独立做一次 /v2/ 探测：它决定「这个地址是不是 registry」，
	// 也顺带触发 Challenge，让认证在真实请求上被验证。
	root, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/", nil)
	if err != nil {
		o.Error = errStr(err)
		return o
	}
	rr, err := auth.Do(ctx, p.Host, root, p.Snapshot, "")
	if err != nil {
		o.Error = errStr(err)
		return o
	}
	rootBody, _ := readCapped(rr.Body, 2048)
	rr.Body.Close()
	o.HTTPCode = rr.StatusCode
	if rr.StatusCode == http.StatusOK && strings.Contains(rr.Header.Get("Content-Type"), "text/html") {
		o.Cap = capNo
		o.Verdict = VerdictNo
		o.Error = "返回 HTML 页面，不是 registry"
		o.Detail = string(rootBody[:min(len(rootBody), 80)])
		return o
	}

	rel := "/v2/" + p.Repo + "/manifests/" + p.Tag
	if prefix {
		rel = "/v2/" + p.Host + "/" + p.Repo + "/manifests/" + p.Tag
	}
	o.Path = rel
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+rel, nil)
	if err != nil {
		o.Error = errStr(err)
		return o
	}
	req.Header.Set("Accept", manifestAccept)
	t0 := time.Now()
	resp, err := auth.Do(ctx, p.Host, req, p.Snapshot, "repository:"+p.Repo+":pull")
	if err != nil {
		o.Error = errStr(err)
		o.LatencyMS = time.Since(t0).Milliseconds()
		return o
	}
	defer resp.Body.Close()
	raw, err := readCapped(resp.Body, defaultManifestCap)
	o.LatencyMS = time.Since(t0).Milliseconds()
	o.HTTPCode = resp.StatusCode
	if err != nil {
		o.Error = "读取 manifest: " + errStr(err)
		return o
	}
	if resp.StatusCode != http.StatusOK {
		if explicitlyUnsupported(resp.StatusCode, raw) {
			o.Cap = capNo
			o.Verdict = VerdictNo
			o.Error = fmt.Sprintf("%d 明确表示不支持该路径格式", resp.StatusCode)
			o.Detail = clip(string(raw), 120)
			return o
		}
		o.Error = fmt.Sprintf("manifest 返回 %d", resp.StatusCode)
		o.Detail = clip(string(raw), 120)
		return o
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		o.Error = "manifest 位置返回 HTML"
		o.Detail = clip(string(raw), 120)
		return o
	}

	// 摘要必须自己算。Docker-Content-Digest 只是对方的说法，正文才是证据。
	sum := sha256.Sum256(raw)
	o.Digest = "sha256:" + hex.EncodeToString(sum[:])
	if declared := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); declared != "" && declared != o.Digest {
		o.Tampered = true
		o.DigestOK = 0
		o.Cap = capMismatch
		o.Verdict = VerdictNo
		o.Error = "Docker-Content-Digest 与实际内容不符"
		return o
	}
	if p.ExpectedDigest != "" {
		if strings.EqualFold(strings.TrimSpace(p.ExpectedDigest), o.Digest) {
			o.DigestOK = 1
		} else {
			o.DigestOK = 0
			o.Cap = capMismatch
			o.Verdict = VerdictNo
			o.Error = "内容与官方基准不一致"
			return o
		}
	}
	o.Cap = capYes
	o.Verdict = VerdictYes
	if o.DigestOK == -1 {
		o.Detail = "内容自洽，未与官方基准比对"
	} else {
		o.Detail = "摘要与官方一致"
	}
	return o
}

// explicitlyUnsupported 只在响应明确表示「这个路径/仓库名不被接受」时成立。
// 404 通常只意味着「我这里没这个仓库」，不能据此写 cap=0。
func explicitlyUnsupported(code int, body []byte) bool {
	if code != http.StatusBadRequest && code != http.StatusNotFound {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "unsupported") || strings.Contains(s, "name_invalid") ||
		strings.Contains(s, "invalid repository name")
}

// mergeOutcome：yes > mismatch > （两条都明确不支持）> unknown。
// 只要有一条路径是 unknown，就不能靠另一条的 0 给整个 Host 定 0。
func mergeOutcome(native, prefix PathOutcome) (string, int) {
	switch {
	case native.Cap == capYes || prefix.Cap == capYes:
		return VerdictYes, capYes
	case native.Cap == capMismatch || prefix.Cap == capMismatch:
		return VerdictNo, capMismatch
	case native.Cap == capNo && prefix.Cap == capNo:
		return VerdictNo, capNo
	default:
		return VerdictUnknown, capUnknown
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
