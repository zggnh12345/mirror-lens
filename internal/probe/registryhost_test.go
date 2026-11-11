package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

const (
	dockerManifest = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"digest":"sha256:aaa"},"layers":[]}`
	ociManifest    = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:bbb"},"layers":[]}`
	schema1Body    = `{"schemaVersion":1,"name":"library/alpine","tag":"latest","fsLayers":[]}`
)

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeHost 模拟一个「按路径格式区分行为」的镜像源。
type fakeHost struct {
	native    string // ok / oci / schema1 / plainjson / html / 404 / name_unknown / name_invalid / unsupported / 500
	prefix    string
	rootHTML  bool
	declared  string // 覆盖 Docker-Content-Digest；空则用真实值
	tokenHits int32
	basicUser string
}

func (f *fakeHost) start(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			atomic.AddInt32(&f.tokenHits, 1)
			u, p, ok := r.BasicAuth()
			if f.basicUser != "" && (!ok || u != f.basicUser || p != "pw") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"token":"t"}`))
			return
		}
		if r.URL.Path == "/v2/" {
			if f.rootHTML {
				w.Header().Set("Content-Type", "text/html")
				w.Write([]byte("<!doctype html><html>login</html>"))
				return
			}
			w.Write([]byte(`{}`))
			return
		}
		var mode string
		switch r.URL.Path {
		case "/v2/library/alpine/manifests/latest":
			mode = f.native
		case "/v2/quay.io/library/alpine/manifests/latest":
			mode = f.prefix
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t" {
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="test",scope="repository:library/alpine:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body := testManifestFor(mode)
		switch mode {
		case "ok", "oci":
			if f.declared != "" {
				w.Header().Set("Docker-Content-Digest", f.declared)
			} else {
				w.Header().Set("Docker-Content-Digest", digestOf(body))
			}
			w.Header().Set("Content-Type", contentTypeFor(mode))
			w.Write([]byte(body))
		case "schema1":
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v1+json")
			w.Write([]byte(body))
		case "plainjson":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(body))
		case "html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>hi</html>"))
		case "name_invalid":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"errors":[{"code":"NAME_INVALID","message":"invalid repository name"}]}`))
		case "name_unknown":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errors":[{"code":"NAME_UNKNOWN"}]}`))
		case "unsupported":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED"}]}`))
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testManifestFor(mode string) string {
	switch mode {
	case "oci":
		return ociManifest
	case "schema1":
		return schema1Body
	default:
		return dockerManifest
	}
}

func contentTypeFor(mode string) string {
	if mode == "oci" {
		return "application/vnd.oci.image.manifest.v1+json"
	}
	return "application/vnd.docker.distribution.manifest.v2+json"
}

func runProbe(t *testing.T, f *fakeHost, expected string, creds v2auth.Credentials) RegistryHostOutcome {
	t.Helper()
	srv := f.start(t)
	return ProbeRegistryHost(context.Background(), RegistryHostProbe{
		BaseURL:        srv.URL,
		Host:           "quay.io",
		ExpectedDigest: expected,
		Snapshot:       v2auth.CredentialSnapshot{Host: "quay.io", Generation: 1, Digest: "d", Credentials: creds},
		Realms:         map[string][]string{"quay.io": {srv.URL}},
		Timeout:        5 * time.Second,
	})
}

func TestRegistryHostNativeOK(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "404"}
	out := runProbe(t, f, digestOf(dockerManifest), v2auth.Credentials{})
	if out.Cap != capYes || out.Verdict != VerdictYes || out.Selected != modeNative {
		t.Fatalf("应判 cap=1/native，得到 %+v", out)
	}
	if out.Native.DigestOK != 1 || out.Native.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Fatalf("native 结论不对: %+v", out.Native)
	}
	if out.Native.Digest != digestOf(dockerManifest) {
		t.Fatalf("摘要应自己算: %s", out.Native.Digest)
	}
	if out.HostPrefix.Cap != capUnknown {
		t.Fatalf("prefix 404 只能算未知: %+v", out.HostPrefix)
	}
}

func TestRegistryHostOCIManifestAccepted(t *testing.T) {
	f := &fakeHost{native: "oci", prefix: "404"}
	out := runProbe(t, f, digestOf(ociManifest), v2auth.Credentials{})
	if out.Cap != capYes {
		t.Fatalf("OCI manifest 应判 1，得到 cap=%d err=%q", out.Cap, out.Native.Error)
	}
	if out.Native.MediaType != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("媒体类型未记录: %q", out.Native.MediaType)
	}
}

// 摘要对、类型不对：不算能力。
func TestRegistryHostRejectsUnsupportedMediaType(t *testing.T) {
	for _, mode := range []string{"schema1", "plainjson"} {
		out := runProbe(t, &fakeHost{native: mode, prefix: "404"}, "", v2auth.Credentials{})
		if out.Cap != capUnknown || out.Native.Cap != capUnknown {
			t.Fatalf("%s 应判未知，得到 cap=%d native=%d", mode, out.Cap, out.Native.Cap)
		}
		if !strings.Contains(out.Native.Error, "媒体类型") {
			t.Fatalf("%s 的错误未说明媒体类型: %q", mode, out.Native.Error)
		}
	}
}

// Content-Type 带参数时必须解析媒体类型，不能全等比较误判。
func TestRegistryHostMediaTypeWithParameters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Write([]byte(`{}`))
			return
		}
		if r.URL.Path != "/v2/library/alpine/manifests/latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json; charset=utf-8")
		w.Header().Set("Docker-Content-Digest", digestOf(ociManifest))
		w.Write([]byte(ociManifest))
	}))
	defer srv.Close()

	out := ProbeRegistryHost(context.Background(), RegistryHostProbe{
		BaseURL:  srv.URL,
		Host:     "quay.io",
		Snapshot: v2auth.CredentialSnapshot{Host: "quay.io"},
		Realms:   map[string][]string{"quay.io": {srv.URL}},
		Timeout:  5 * time.Second,
	})
	if out.Native.Cap != capYes {
		t.Fatalf("带参数的 Content-Type 应被接受: %+v", out.Native)
	}
	if out.Native.MediaType != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("媒体类型应去掉参数: %q", out.Native.MediaType)
	}
}

// cap=1 与 selected 必须成对出现：只写 cap 不写路径，等于把「哪条路径可信」
// 交给下游猜。
func TestAggregateInvariant(t *testing.T) {
	caps := []int{capUnknown, capNo, capYes, capMismatch}
	for _, a := range caps {
		for _, b := range caps {
			_, got, sel := aggregate(PathOutcome{Cap: a}, PathOutcome{Cap: b})
			if got == capYes && sel == "" {
				t.Fatalf("(%d,%d) 判了 cap=1 却没给 selected", a, b)
			}
			if got != capYes && sel != "" {
				t.Fatalf("(%d,%d) 非 cap=1 却给了 selected=%q", a, b, sel)
			}
		}
	}
}

// 两条路径都篡改，才是 2。
func TestRegistryHostBothTampered(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "ok", declared: "sha256:" + strings.Repeat("0", 64)}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.Cap != capMismatch || !out.Native.Tampered || !out.HostPrefix.Tampered {
		t.Fatalf("两条路径都篡改应判 2，得到 %+v", out)
	}
}

// 一条不一致、另一条没验证完：不下结论，但保留证据。
func TestRegistryHostTamperedWithUnknownOtherPath(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "500", declared: "sha256:" + strings.Repeat("0", 64)}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.Cap != capUnknown {
		t.Fatalf("2 + 未知应判 -1，得到 cap=%d", out.Cap)
	}
	if len(out.Warnings) == 0 {
		t.Fatal("不一致证据必须保留告警")
	}
}

// 内容与官方基准不同：两条路径都比对，才判 2。
func TestRegistryHostMismatchAgainstOfficial(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "ok"}
	out := runProbe(t, f, "sha256:"+strings.Repeat("1", 64), v2auth.Credentials{})
	if out.Cap != capMismatch || out.Native.DigestOK != 0 {
		t.Fatalf("与官方不一致应判 2，得到 cap=%d digest_ok=%d", out.Cap, out.Native.DigestOK)
	}
}

// 2 + 1：选中的路径通过完整校验，cap=1；但必须保留异常告警。
func TestRegistryHostVerifiedWinsOverMismatch(t *testing.T) {
	verdict, got, sel := aggregate(
		PathOutcome{Cap: capMismatch},
		PathOutcome{Cap: capYes},
	)
	if verdict != VerdictYes || got != capYes || sel != modeHostPrefix {
		t.Fatalf("2+1 应选 host_prefix 并判 1，得到 %s/%d/%s", verdict, got, sel)
	}
	w := collectWarnings(PathOutcome{Cap: capMismatch}, PathOutcome{Cap: capYes}, modeHostPrefix, capYes)
	if len(w) == 0 {
		t.Fatal("未选中路径的不一致必须留下告警")
	}
}

// native 不支持、prefix 可用。
func TestRegistryHostPrefixOnly(t *testing.T) {
	f := &fakeHost{native: "404", prefix: "ok"}
	out := runProbe(t, f, digestOf(dockerManifest), v2auth.Credentials{})
	if out.Cap != capYes || out.Selected != modeHostPrefix {
		t.Fatalf("prefix 可用应选 host_prefix，得到 %+v", out)
	}
	if out.HostPrefix.Path != "/v2/quay.io/library/alpine/manifests/latest" {
		t.Fatalf("prefix 路径不对: %s", out.HostPrefix.Path)
	}
}

// 双 404 只是「没这个仓库」，绝不能写 0。
func TestRegistryHostDouble404IsUnknown(t *testing.T) {
	f := &fakeHost{native: "404", prefix: "404"}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.Cap != capUnknown {
		t.Fatalf("双 404 应保持 -1，得到 cap=%d", out.Cap)
	}
	if len(out.Warnings) == 0 {
		t.Fatal("应记录「双 404 只作负面证据」的告警")
	}
}

// 两条路径都明确 UNSUPPORTED：0。
func TestRegistryHostExplicitUnsupported(t *testing.T) {
	f := &fakeHost{native: "unsupported", prefix: "unsupported"}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.Cap != capNo {
		t.Fatalf("明确不支持应判 0，得到 cap=%d", out.Cap)
	}
}

// 一条明确不支持、另一条只是 404：仍然不能下定论。
func TestRegistryHostPartialEvidenceStaysUnknown(t *testing.T) {
	f := &fakeHost{native: "unsupported", prefix: "404"}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.Cap != capUnknown {
		t.Fatalf("证据不完整时应保持 -1，得到 cap=%d", out.Cap)
	}
}

// NAME_INVALID 有 native 对照（同仓库被正确识别）才升级为 0。
func TestRegistryHostNameInvalidWithNativeControl(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "name_invalid"}
	out := runProbe(t, f, digestOf(dockerManifest), v2auth.Credentials{})
	if out.HostPrefix.Cap != capNo || !strings.Contains(out.HostPrefix.Error, "路径格式被拒绝") {
		t.Fatalf("有 native 对照时 prefix 应判 0: %+v", out.HostPrefix)
	}
	if out.Cap != capYes || out.Selected != modeNative {
		t.Fatalf("聚合仍应是 native 可用: %+v", out)
	}
}

// 没有对照证据时，NAME_INVALID 只是疑似，不能写 0。
func TestRegistryHostNameInvalidWithoutControl(t *testing.T) {
	f := &fakeHost{native: "500", prefix: "name_invalid"}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.HostPrefix.Cap != capUnknown {
		t.Fatalf("缺少对照时 prefix 应保持 -1，得到 %+v", out.HostPrefix)
	}
	if out.Cap != capUnknown {
		t.Fatalf("整体应保持 -1，得到 cap=%d", out.Cap)
	}
}

// native 给标准 NAME_UNKNOWN 也算「仓库名被正确识别」的对照。
func TestRegistryHostNameUnknownActsAsControl(t *testing.T) {
	f := &fakeHost{native: "name_unknown", prefix: "name_invalid"}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.HostPrefix.Cap != capNo {
		t.Fatalf("NAME_UNKNOWN 应作为对照，得到 %+v", out.HostPrefix)
	}
}

// /v2/ 返回 HTML 不能证明「路径格式不支持」，只能算未知。
func TestRegistryHostHTMLIsUnknown(t *testing.T) {
	out := runProbe(t, &fakeHost{native: "ok", prefix: "ok", rootHTML: true}, "", v2auth.Credentials{})
	if out.Cap != capUnknown {
		t.Fatalf("HTML 应判 -1，得到 cap=%d", out.Cap)
	}
}

func TestRegistryHostServerErrorIsUnknown(t *testing.T) {
	f := &fakeHost{native: "500", prefix: "500"}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.Cap != capUnknown {
		t.Fatalf("5xx 应保持 -1，得到 cap=%d", out.Cap)
	}
}

// 凭据只用于换 Token，且每条路径各自换一次。
func TestRegistryHostUsesCredentialSnapshot(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "ok", basicUser: "robot"}
	out := runProbe(t, f, digestOf(dockerManifest), v2auth.Credentials{Username: "robot", Password: "pw"})
	if out.Cap != capYes {
		t.Fatalf("带凭据应成功，得到 cap=%d err=%q", out.Cap, out.Native.Error)
	}
	if got := atomic.LoadInt32(&f.tokenHits); got != 2 {
		t.Fatalf("native/prefix 应各自换一次 Token，实际 %d 次", got)
	}
}

// 快照与探测目标必须同源：拿 A 的凭据去探 B 绝不允许。
func TestRegistryHostRejectsForeignSnapshot(t *testing.T) {
	srv := (&fakeHost{native: "ok", prefix: "ok"}).start(t)
	out := ProbeRegistryHost(context.Background(), RegistryHostProbe{
		BaseURL:  srv.URL,
		Host:     "quay.io",
		Snapshot: v2auth.CredentialSnapshot{Host: "gcr.io", Generation: 9, Digest: "x"},
		Realms:   map[string][]string{"quay.io": {srv.URL}},
	})
	if out.Cap != capUnknown || !strings.Contains(out.Native.Error, "不一致") {
		t.Fatalf("跨 Host 快照应被拒: %+v", out)
	}
}

func TestRegistryHostRejectsHubAndGhcr(t *testing.T) {
	for _, h := range []string{"ghcr.io", "docker.io", ""} {
		out := ProbeRegistryHost(context.Background(), RegistryHostProbe{Host: h})
		if out.Cap != capUnknown || out.Native.Error == "" {
			t.Fatalf("%q 不应进入 Host 探测: %+v", h, out)
		}
	}
}

// 冻结的聚合表；改动任何一行都会让 cap 与 path_mode 对不上。
func TestAggregateMatrix(t *testing.T) {
	cases := []struct {
		native, prefix int
		cap            int
		selected       string
	}{
		{1, 0, 1, modeNative},
		{0, 1, 1, modeHostPrefix},
		{1, -1, 1, modeNative},
		{-1, 1, 1, modeHostPrefix},
		{1, 1, 1, modeNative},
		{2, 1, 1, modeHostPrefix},
		{1, 2, 1, modeNative},
		{2, 2, 2, ""},
		{0, 0, 0, ""},
		{-1, -1, -1, ""},
		{0, -1, -1, ""},
		{2, -1, -1, ""},
	}
	for _, c := range cases {
		_, got, sel := aggregate(PathOutcome{Cap: c.native}, PathOutcome{Cap: c.prefix})
		if got != c.cap || sel != c.selected {
			t.Fatalf("(%d,%d) 应为 cap=%d selected=%q，得到 cap=%d selected=%q",
				c.native, c.prefix, c.cap, c.selected, got, sel)
		}
	}
}

// 探测层的 cap 取值必须与 store 一致，否则面板与调度会错位。
func TestRegistryCapConstantsMatchStore(t *testing.T) {
	if capUnknown != store.RegistryCapUnknown || capNo != store.RegistryCapNo ||
		capYes != store.RegistryCapYes || capMismatch != store.RegistryCapMismatch {
		t.Fatal("probe 与 store 的 registry_cap 取值不一致")
	}
	if modeNative != store.PathNative || modeHostPrefix != store.PathHostPrefix {
		t.Fatal("probe 与 store 的路径格式常量不一致")
	}
}

// 聚合语义只能有一处真相：cap=1 必须指向一条自身 cap=1 的路径。
func TestStoreFieldsInvariant(t *testing.T) {
	ok := RegistryHostOutcome{
		Cap: capYes, Selected: modeHostPrefix,
		Native:     PathOutcome{Mode: modeNative, Cap: capMismatch},
		HostPrefix: PathOutcome{Mode: modeHostPrefix, Cap: capYes},
	}
	capv, mode, note, err := ok.StoreFields()
	if err != nil || capv != capYes || mode != modeHostPrefix || note == "" {
		t.Fatalf("合法结果应通过: cap=%d mode=%q err=%v", capv, mode, err)
	}

	bad := []RegistryHostOutcome{
		{Cap: capYes, Selected: modeNative, Native: PathOutcome{Cap: capUnknown}},
		{Cap: capYes, Selected: ""},
		{Cap: capYes, Selected: "weird", Native: PathOutcome{Cap: capYes}},
		{Cap: capMismatch, Selected: modeNative, Native: PathOutcome{Cap: capYes}},
		{Cap: capNo, Selected: modeHostPrefix},
		{Cap: 7},
	}
	for _, b := range bad {
		if _, _, _, err := b.StoreFields(); err == nil {
			t.Fatalf("应拒绝不一致结果: %+v", b)
		}
	}
}

// 诊断 JSON 必须完整可解析，且不超过 4 KiB；裁剪只发生在文本字段上。
func TestDiagnosticJSONFitsAndParses(t *testing.T) {
	big := strings.Repeat("错", 6000)
	out := RegistryHostOutcome{
		Host:       "quay.io",
		Cap:        capUnknown,
		Native:     PathOutcome{Mode: modeNative, Cap: capUnknown, Error: big, Detail: big},
		HostPrefix: PathOutcome{Mode: modeHostPrefix, Cap: capNo, Error: big, Detail: big},
		Warnings:   []string{big, "native 内容不一致"},
	}
	raw := out.DiagnosticJSON()
	if len(raw) > diagMaxBytes {
		t.Fatalf("诊断超长: %d", len(raw))
	}
	var d Diagnostics
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("诊断必须是完整 JSON: %v", err)
	}
	if d.Version != DiagnosticsVersion || d.Native.Mode != modeNative || d.HostPrefix.Mode != modeHostPrefix {
		t.Fatalf("关键字段丢失: %+v", d)
	}
	if len([]rune(d.Native.Error)) > diagErrMaxRunes {
		t.Fatalf("错误文本未裁剪: %d", len([]rune(d.Native.Error)))
	}
}

// 2+0 / 0+2 各自留下明确告警，聚合仍是 -1。
func TestAggregateMismatchVsUnsupported(t *testing.T) {
	for _, tc := range []struct {
		native, prefix int
		want           string
	}{
		{capMismatch, capNo, "native 内容不一致"},
		{capNo, capMismatch, "host_prefix 内容不一致"},
	} {
		_, capv, sel := aggregate(PathOutcome{Cap: tc.native}, PathOutcome{Cap: tc.prefix})
		if capv != capUnknown || sel != "" {
			t.Fatalf("(%d,%d) 应为 -1/空，得到 %d/%q", tc.native, tc.prefix, capv, sel)
		}
		w := strings.Join(collectWarnings(PathOutcome{Cap: tc.native}, PathOutcome{Cap: tc.prefix}, "", capv), "|")
		if !strings.Contains(w, tc.want) {
			t.Fatalf("缺少告警 %q: %q", tc.want, w)
		}
	}
}
