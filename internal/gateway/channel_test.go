package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
)

func regReq(g *Gateway, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	rec := httptest.NewRecorder()
	g.ServeRegistry(rec, req)
	return rec
}

// 三个通道开关要各自独立：关 GHCR 不能把 Docker Hub 一起关掉，
// 反之亦然；ping 永远放行，否则 docker 客户端连握手都过不去。
func TestRegistryChannelGating(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.requireToken = false
	g.regAd = registryadapter.New("registry-1.docker.io", 1<<30, time.Minute)

	g.ghcrOff = true
	rr := regReq(g, "http://gw/v2/ghcr.io/zggnh12345/mirror-lens/manifests/3.1.0")
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), "GHCR") {
		t.Fatalf("关 GHCR 后应 404 GHCR，得到 %d %q", rr.Code, rr.Body.String())
	}
	// Docker Hub 通道仍开着：这条不该被 GHCR 的开关带下去。
	rr = regReq(g, "http://gw/v2/library/alpine/manifests/latest")
	if strings.Contains(rr.Body.String(), "GHCR") {
		t.Fatalf("关 GHCR 误伤了 Docker 通道: %q", rr.Body.String())
	}
	// ping 不受通道开关影响。
	if rr = regReq(g, "http://gw/v2/"); rr.Code != 200 {
		t.Fatalf("ping 应 200，得到 %d", rr.Code)
	}

	g.ghcrOff = false
	g.dockerOff = true
	rr = regReq(g, "http://gw/v2/library/alpine/manifests/latest")
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), "Docker") {
		t.Fatalf("关 Docker 后应 404 Docker，得到 %d %q", rr.Code, rr.Body.String())
	}
	rr = regReq(g, "http://gw/v2/ghcr.io/zggnh12345/mirror-lens/manifests/3.1.0")
	if strings.Contains(rr.Body.String(), "Docker 加速未开启") {
		t.Fatalf("关 Docker 误伤了 GHCR 通道: %q", rr.Body.String())
	}
}

func TestGitHubSwitch(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	g.requireToken = false
	g.githubOff = true
	for _, p := range []string{"http://gw/raw/o/r/main/a.txt", "http://gw/gh/o/r/releases/download/v1/a.bin"} {
		rr := do(g, "", p, nil)
		if rr.Code != 404 || !strings.Contains(rr.Body.String(), "GitHub 加速未开启") {
			t.Fatalf("%s 应 404 GitHub 加速未开启，得到 %d %q", p, rr.Code, rr.Body.String())
		}
	}
}
