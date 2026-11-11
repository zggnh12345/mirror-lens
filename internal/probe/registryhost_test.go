package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

const testManifest = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"digest":"sha256:aaa"},"layers":[]}`

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeHost 模拟一个「按路径格式区分行为」的镜像源。
type fakeHost struct {
	native    string // ok / 404 / unsupported / 500 / html
	prefix    string
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
		switch mode {
		case "ok":
			if f.declared != "" {
				w.Header().Set("Docker-Content-Digest", f.declared)
			} else {
				w.Header().Set("Docker-Content-Digest", digestOf(testManifest))
			}
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Write([]byte(testManifest))
		case "html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>hi</html>"))
		case "404":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`))
		case "unsupported":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED"}]}`))
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
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
	out := runProbe(t, f, digestOf(testManifest), v2auth.Credentials{})
	if out.Cap != capYes || out.Verdict != VerdictYes {
		t.Fatalf("应判 cap=1，得到 cap=%d verdict=%q err=%q", out.Cap, out.Verdict, out.Native.Error)
	}
	if out.Native.Mode != modeNative || out.Native.DigestOK != 1 {
		t.Fatalf("native 结论不对: %+v", out.Native)
	}
	if out.Native.Digest != digestOf(testManifest) {
		t.Fatalf("摘要应自己算: %s", out.Native.Digest)
	}
	if out.HostPrefix.Cap != capUnknown {
		t.Fatalf("prefix 404 只能算未知，不能变成 0: %+v", out.HostPrefix)
	}
}

// 响应头说的摘要与正文不符：必须自己算出来并判 2。
func TestRegistryHostDeclaredDigestLies(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "404", declared: "sha256:" + strings.Repeat("0", 64)}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.Cap != capMismatch || !out.Native.Tampered {
		t.Fatalf("头与正文不符应判 2，得到 cap=%d tampered=%v err=%q", out.Cap, out.Native.Tampered, out.Native.Error)
	}
}

// 内容与官方基准不同：判 2。
func TestRegistryHostMismatchAgainstOfficial(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "404"}
	out := runProbe(t, f, "sha256:"+strings.Repeat("1", 64), v2auth.Credentials{})
	if out.Cap != capMismatch || out.Native.DigestOK != 0 {
		t.Fatalf("与官方不一致应判 2，得到 cap=%d digest_ok=%d", out.Cap, out.Native.DigestOK)
	}
}

// native 不支持、prefix 可用：cap=1，且记录命中 prefix。
func TestRegistryHostPrefixOnly(t *testing.T) {
	f := &fakeHost{native: "404", prefix: "ok"}
	out := runProbe(t, f, digestOf(testManifest), v2auth.Credentials{})
	if out.Cap != capYes || out.HostPrefix.Cap != capYes {
		t.Fatalf("prefix 可用应判 1，得到 %+v", out)
	}
	if out.HostPrefix.Path != "/v2/quay.io/library/alpine/manifests/latest" {
		t.Fatalf("prefix 路径不对: %s", out.HostPrefix.Path)
	}
}

// 双 404 只是「没这个仓库」，绝不能写 0，也不能写 1。
func TestRegistryHostDouble404IsUnknown(t *testing.T) {
	f := &fakeHost{native: "404", prefix: "404"}
	out := runProbe(t, f, "", v2auth.Credentials{})
	if out.Cap != capUnknown {
		t.Fatalf("双 404 应保持 -1，得到 cap=%d", out.Cap)
	}
	if out.Cap == capNo {
		t.Fatal("双 404 不得写成 cap=0")
	}
	if len(out.Warnings) == 0 {
		t.Fatal("应记录「双 404 只作负面证据」的告警")
	}
}

// 只有明确的「不支持该路径格式」才写 0。
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
	out := runProbe(t, f, digestOf(testManifest), v2auth.Credentials{Username: "robot", Password: "pw"})
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
