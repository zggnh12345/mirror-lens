package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func storeCacheEntry(key string) store.CacheEntry {
	return store.CacheEntry{
		Key:         key,
		SHA256:      strings.Repeat("c", 64),
		Size:        2,
		ContentType: "application/vnd.docker.distribution.manifest.v2+json",
		CreatedAt:   time.Now().Unix(),
		TTL:         300,
		Verify:      store.VerifyVerified,
	}
}

// 禁用 Host 必须在校验、缓存、referrers 之前生效，且四种访问方式一致。
func TestRegistryDisabledHostAdmission(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.requireToken = false
	g.regAd = registryadapter.New("registry-1.docker.io", 1<<30, time.Minute)
	g.disabledHosts = map[string]bool{"gcr.io": true, "us-docker.pkg.dev": true}

	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []string{
		"http://gw/v2/gcr.io/foo/bar/manifests/latest",
		"http://gw/v2/gcr.io/foo/bar/blobs/" + digest,
		"http://gw/v2/gcr.io/foo/bar/referrers/" + digest,
		"http://gw/v2/gcr.io/foo/bar/blobs/uploads/",
		"http://gw/v2/us-docker.pkg.dev/foo/bar/manifests/latest",
	}
	for _, target := range cases {
		rr := regReq(g, target)
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "加速未开启") {
			t.Errorf("%s 应 404 加速未开启，得到 %d %q", target, rr.Code, rr.Body.String())
		}
	}
	if rr := regReq(g, "http://gw/v2/"); rr.Code != http.StatusOK {
		t.Fatalf("ping 不能被禁用列表影响，得到 %d", rr.Code)
	}
}

// disabledHosts 只拦访问；Docker 通道关闭不能把所有 foreign Host 一起带走。
func TestRegistryForeignHostNotBoundToDockerSwitch(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.dockerOff = true
	g.ghcrOff = true
	for _, h := range []string{"gcr.io", "registry.k8s.io", "us-docker.pkg.dev"} {
		if msg, off := g.registryChannelOff(h); off {
			t.Errorf("%s 不应因 dockerOff 被关掉: %s", h, msg)
		}
	}
	if _, off := g.registryChannelOff("registry-1.docker.io"); !off {
		t.Error("Docker Hub 应仍受 dockerOff 控制")
	}
	if _, off := g.registryChannelOff("ghcr.io"); !off {
		t.Error("GHCR 应仍受 ghcrOff 控制")
	}
}

func TestRegistryDisabledHostBlocksCache(t *testing.T) {
	g, st, _, _, _ := newGW(t, true)
	g.requireToken = false
	g.regAd = registryadapter.New("registry-1.docker.io", 1<<30, time.Minute)
	g.disabledHosts = map[string]bool{"gcr.io": true}

	// 放一条“看起来新鲜”的缓存索引；禁用 Host 必须在读缓存之前拒绝，
	// 不能因为缓存命中就绕过访问开关。
	key := "docker:manifest:gcr.io/foo:latest|accept="
	if err := st.CachePut(storeCacheEntry(key)); err != nil {
		t.Fatal(err)
	}
	rr := regReq(g, "http://gw/v2/gcr.io/foo/manifests/latest")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("禁用 Host 的缓存请求应 404，得到 %d %q", rr.Code, rr.Body.String())
	}
}

func TestRegistryDisabledHostHEAD(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.requireToken = false
	g.regAd = registryadapter.New("registry-1.docker.io", 1<<30, time.Minute)
	g.disabledHosts = map[string]bool{"gcr.io": true}
	req := httptest.NewRequest(http.MethodHead, "http://gw/v2/gcr.io/foo/bar/manifests/latest", nil)
	rr := httptest.NewRecorder()
	g.ServeRegistry(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("HEAD 应 404，得到 %d", rr.Code)
	}
}
