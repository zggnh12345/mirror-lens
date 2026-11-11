package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// PathOutcome 是一条路径格式的独立结论。两条路径各自保留完整诊断，
// 即使最终只向数据库提交一组聚合结论。
type PathOutcome struct {
	Mode     string `json:"mode"` // native / host_prefix
	Path     string `json:"path"`
	Verdict  string `json:"verdict,omitempty"`
	Cap      int    `json:"cap"`
	HTTPCode int    `json:"http_code,omitempty"`
	// Code 是 Registry 错误体里的 errors[].code（如 NAME_UNKNOWN）。
	Code      string `json:"code,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Digest    string `json:"digest,omitempty"`
	DigestOK  int    `json:"digest_ok"` // -1 未比对 / 0 与官方不同 / 1 一致
	Tampered  bool   `json:"tampered,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	Detail    string `json:"detail,omitempty"`

	// weakEvidence：证据方向上像「路径格式不支持」，但 NAME_INVALID 也可能
	// 只是命名空间/代理层的仓库名限制。只有另一条路径能证明仓库名被正确
	// 识别时才升级为 cap=0，否则保持 -1。
	weakEvidence bool
}

// RegistryHostOutcome 是一个 Host 的综合结论。
type RegistryHostOutcome struct {
	Host       string      `json:"host"`
	Native     PathOutcome `json:"native"`
	HostPrefix PathOutcome `json:"host_prefix"`
	// Selected 是支撑最终 cap=1 的那条路径；cap 不是 1 时为空。
	Selected string   `json:"selected,omitempty"`
	Verdict  string   `json:"verdict,omitempty"`
	Cap      int      `json:"cap"`
	Warnings []string `json:"warnings,omitempty"`
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

// Manifest 媒体类型白名单：摘要相同只证明字节一致，不证明拿到的是
// 预期的 Manifest 类型。schema1、HTML、纯文本都不算能力。
var manifestTypes = map[string]bool{
	"application/vnd.docker.distribution.manifest.v2+json":      true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
	"application/vnd.oci.image.manifest.v1+json":                true,
	"application/vnd.oci.image.index.v1+json":                   true,
}

// ProbeRegistryHost 独立验证 native 与 host_prefix 两种路径，再按固定
// 聚合规则给出一组结论（数据模型仍是一个 Source × Host 一条记录）。
//
// 单路径映射：
//
//		1 = 取到受支持的 Manifest，内容自洽且与官方基准一致
//		2 = 取到内容但摘要不一致（硬证据）
//		0 = 有明确证据表明该路径格式不被支持（需 native 对照）
//	  -1 = 其余一切：超时、5xx、401、HTML、双 404……
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
	applyControlEvidence(&out.Native, &out.HostPrefix)
	out.Verdict, out.Cap, out.Selected = aggregate(out.Native, out.HostPrefix)
	out.Warnings = collectWarnings(out.Native, out.HostPrefix, out.Selected, out.Cap)
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
	if isHTML(rr.Header.Get("Content-Type"), rootBody) {
		// 登录页、代理错误页都会返回 HTML。它既不能证明是 registry，
		// 也不能证明路径格式不支持，只能算未知。
		o.Error = "返回 HTML 页面，无法判定 registry 能力"
		o.Detail = clip(string(rootBody), 80)
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
		o.Code = registryErrorCode(raw)
		o.Detail = clip(string(raw), 120)
		switch {
		case isHTML(resp.Header.Get("Content-Type"), raw):
			o.Error = fmt.Sprintf("manifest 位置返回 HTML（%d），无法判定", resp.StatusCode)
		case o.Code == "UNSUPPORTED":
			o.Cap = capNo
			o.Verdict = VerdictNo
			o.Error = fmt.Sprintf("%d 明确表示不支持该路径格式", resp.StatusCode)
		case o.Code == "NAME_INVALID" || hasInvalidNameText(raw):
			// 可能是命名空间/代理层的名字限制，先记弱证据。
			o.weakEvidence = true
			o.Error = fmt.Sprintf("%d 疑似路径格式不被接受（%s）", resp.StatusCode, codeOr(o.Code))
		default:
			o.Error = fmt.Sprintf("manifest 返回 %d", resp.StatusCode)
		}
		return o
	}

	mt, ok := acceptedManifestType(resp.Header.Get("Content-Type"), raw)
	if !ok {
		o.Error = "Manifest 媒体类型不在支持范围"
		o.Detail = clip(resp.Header.Get("Content-Type")+" "+string(raw), 120)
		return o
	}
	o.MediaType = mt

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

// applyControlEvidence 把 NAME_INVALID 这类弱证据升级为 cap=0，前提是
// 另一条路径能证明「同一仓库、同一认证条件下」仓库名被正确识别：
// 要么对方成功返回 manifest，要么给出标准的 NAME_UNKNOWN/MANIFEST_UNKNOWN。
// 否则只按疑似处理，保持 -1。
func applyControlEvidence(native, prefix *PathOutcome) {
	if prefix.weakEvidence && controlOK(*native) {
		prefix.Cap = capNo
		prefix.Verdict = VerdictNo
		prefix.Error = "native 可识别该仓库，host_prefix 路径格式被拒绝"
	}
	if native.weakEvidence && controlOK(*prefix) {
		native.Cap = capNo
		native.Verdict = VerdictNo
		native.Error = "host_prefix 可识别该仓库，native 路径格式被拒绝"
	}
}

func controlOK(o PathOutcome) bool {
	if o.Cap == capYes {
		return true
	}
	return o.HTTPCode == http.StatusNotFound &&
		(o.Code == "NAME_UNKNOWN" || o.Code == "MANIFEST_UNKNOWN")
}

// aggregate 是冻结的聚合表：已验证路径优先于不一致路径，不一致只在两条
// 路径都不一致时才生效；0 必须两条路径都成立。
func aggregate(native, prefix PathOutcome) (verdict string, cap int, selected string) {
	switch {
	case native.Cap == capYes:
		return VerdictYes, capYes, modeNative
	case prefix.Cap == capYes:
		return VerdictYes, capYes, modeHostPrefix
	case native.Cap == capMismatch && prefix.Cap == capMismatch:
		return VerdictNo, capMismatch, ""
	case native.Cap == capNo && prefix.Cap == capNo:
		return VerdictNo, capNo, ""
	default:
		return VerdictUnknown, capUnknown, ""
	}
}

func collectWarnings(native, prefix PathOutcome, selected string, cap int) []string {
	var w []string
	if native.HTTPCode == http.StatusNotFound && prefix.HTTPCode == http.StatusNotFound {
		w = append(w, "两条路径均 404：只作负面证据，不写 cap=0")
	}
	// 未选中的路径若有不一致证据，必须留下记录，不能静默丢弃。
	if selected == modeNative && prefix.Cap == capMismatch {
		w = append(w, "host_prefix 内容不一致，未污染 native，但证据已保留")
	}
	if selected == modeHostPrefix && native.Cap == capMismatch {
		w = append(w, "native 内容不一致，未污染 host_prefix，但证据已保留")
	}
	if cap == capUnknown && (native.Cap == capMismatch || prefix.Cap == capMismatch) {
		w = append(w, "存在内容不一致证据，但另一条路径未完成验证，不下结论")
	}
	return w
}

func registryErrorCode(body []byte) string {
	var v struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &v) != nil || len(v.Errors) == 0 {
		return ""
	}
	return v.Errors[0].Code
}

func hasInvalidNameText(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "invalid repository name")
}

func codeOr(code string) string {
	if code == "" {
		return "无错误码"
	}
	return code
}

// acceptedManifestType 以 Content-Type 为准。只有对方完全没给类型时，
// 才退回正文自报的 mediaType；给了错误类型（text/plain、schema1）一律不算，
// 否则「字节对但响应类型不对」会被当成能力。
func acceptedManifestType(ct string, body []byte) (string, bool) {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if ct != "" {
		return ct, manifestTypes[ct]
	}
	var v struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(body, &v) == nil && manifestTypes[v.MediaType] {
		return v.MediaType, true
	}
	return "", false
}

func isHTML(ct string, body []byte) bool {
	if strings.Contains(strings.ToLower(ct), "text/html") {
		return true
	}
	s := strings.ToLower(strings.TrimSpace(string(body)))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html")
}

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
