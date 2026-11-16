package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth/v2authtest"
)

// 裸 Docker Hub 名 fallback 到 ghcr 时，认证身份必须切到 ghcr 的
// 候选源 SourceID/Host，不能沿用 Docker Hub 的官方身份。
func TestFallbackToGhcrCarriesCandidateIdentity(t *testing.T) {
	f := newMOFixture(t)
	f.officialStatus = http.StatusNotFound // 迫使主上游失败，进入 ghcr fallback
	// 夹具默认把上游指向本机；这里恢复成真实 Docker Hub 身份，
	// fallback 才会按 registry-1.docker.io -> ghcr.io 生成。
	officialHost := f.ad.Upstream
	f.ad.Upstream = "registry-1.docker.io"
	f.ad.Scheme = "http"

	var tokenHits atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenHits.Add(1)
		if got := r.URL.Query().Get("scope"); got != "repository:foo/bar:pull" {
			t.Errorf("token scope=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token":"ghcr-token"}`)
	}))
	defer tokenSrv.Close()

	var mirrorHits atomic.Int32
	var mu sync.Mutex
	var auths []string
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits.Add(1)
		if r.URL.Path != "/v2/foo/bar/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry",scope="repository:foo/bar:pull"`, tokenSrv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", manifestsAccept)
		fmt.Fprint(w, mirrorManifest)
	}))
	defer mirrorSrv.Close()

	id, err := f.st.AddSource(store.Source{
		BaseURL: mirrorSrv.URL, Name: "ghcr-candidate", Status: "usable", Enabled: true,
		RepoHost: "ghcr.io", GhcrCap: store.CapYes, GhcrPrefix: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	policy := &recordingPolicy{base: v2authtest.New(map[string][]string{"ghcr.io": {tokenSrv.URL}}, false)}
	f.gw.policies = policy
	f.gw.cli = &http.Client{Transport: &bearerTransport{
		base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.EqualFold(r.URL.Host, "registry-1.docker.io") {
				u := *r.URL
				u.Host = officialHost
				r2 := r.Clone(r.Context())
				r2.URL = &u
				r2.Host = u.Host
				return http.DefaultTransport.RoundTrip(r2)
			}
			return http.DefaultTransport.RoundTrip(r)
		}),
		tokens: newTokenCache(),
	}}

	req := httptest.NewRequest(http.MethodGet, "http://gw/v2/foo/bar/manifests/latest", nil)
	plan, err := f.ad.Plan(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Fallbacks) == 0 || plan.Fallbacks[0].URL.Host != "ghcr.io" {
		t.Fatalf("计划未生成 ghcr fallback: %+v", plan.Fallbacks)
	}
	var candidateID int64
	for _, target := range f.gw.targets(plan, nil) {
		if target.fbHost == "ghcr.io" && target.c != nil {
			candidateID = target.c.ID
		}
	}
	if candidateID != id {
		t.Fatalf("ghcr fallback 的候选 SourceID=%d，应为 %d", candidateID, id)
	}

	w := f.get(t, "/v2/foo/bar/manifests/latest")
	if w.Code != http.StatusOK || w.Body.String() != mirrorManifest {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if tokenHits.Load() != 1 {
		t.Fatalf("tokenHits=%d，ghcr 候选应换一次 token", tokenHits.Load())
	}
	if mirrorHits.Load() != 2 || len(auths) != 1 || auths[0] != "Bearer ghcr-token" {
		t.Fatalf("mirrorHits=%d auths=%v", mirrorHits.Load(), auths)
	}
	if f.officialHits.Load() != 1 {
		t.Fatalf("主上游应先回源一次并 404，officialHits=%d", f.officialHits.Load())
	}
	found := false
	for _, sourceID := range policy.sources() {
		if sourceID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("fallback 策略未收到候选 SourceID %d，实际 %v", id, policy.sources())
	}
}
