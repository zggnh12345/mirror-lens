package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// recordingPolicy 记录真实路由解析时传进来的 SourceID。
type recordingPolicy struct {
	base   v2auth.PolicyResolver
	mu     sync.Mutex
	source []int64
}

func (p *recordingPolicy) Resolve(ctx context.Context, sourceID int64, host string) (v2auth.PolicySnapshot, error) {
	p.mu.Lock()
	p.source = append(p.source, sourceID)
	p.mu.Unlock()
	return p.base.Resolve(ctx, sourceID, host)
}

func (p *recordingPolicy) sources() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.source...)
}

// 候选源的 401 挑战也必须带着该源的 SourceID 走 token 流程；
// 这里用真实 Gateway 路由 + scheduler 候选，而不是直接构造 PolicySnapshot。
func TestCandidateRegistryTokenUsesSourceIdentity(t *testing.T) {
	f := newMOFixture(t)

	var tokenHits atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenHits.Add(1)
		if got := r.URL.Query().Get("service"); got != "registry" {
			t.Errorf("token service=%q", got)
		}
		if got := r.URL.Query().Get("scope"); got != "repository:library/alpine:pull" {
			t.Errorf("token scope=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token":"candidate-token"}`)
	}))
	defer tokenSrv.Close()

	var mirrorHits atomic.Int32
	var mu sync.Mutex
	var auths []string
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits.Add(1)
		if r.URL.Path != "/v2/library/alpine/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry",scope="repository:library/alpine:pull"`, tokenSrv.URL))
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

	id := f.addRegistrySource(t, mirrorSrv, "native")
	// 这个测试的 Realm 是本机 token server，显式挂到 quay.io 的过渡策略里。
	policy := &recordingPolicy{base: v2auth.NewLegacyHostPolicy(map[string][]string{f.host: {tokenSrv.URL}}, false)}
	f.gw.policies = policy
	f.gw.cli = &http.Client{Transport: &bearerTransport{base: http.DefaultTransport, tokens: newTokenCache()}}

	req := httptest.NewRequest(http.MethodGet, "http://gw/v2/quay.io/library/alpine/manifests/latest", nil)
	plan, err := f.ad.Plan(req)
	if err != nil {
		t.Fatal(err)
	}
	targets := f.gw.targets(plan, nil)
	if len(targets) == 0 || targets[0].c == nil || targets[0].c.ID != id {
		t.Fatalf("候选源身份未进入 target: %+v", targets)
	}

	w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest")
	if w.Code != http.StatusOK || w.Body.String() != mirrorManifest {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if tokenHits.Load() != 1 {
		t.Fatalf("tokenHits=%d，候选源应换一次 token", tokenHits.Load())
	}
	if mirrorHits.Load() != 2 {
		t.Fatalf("mirrorHits=%d，应经历 401 + 带 token 重发", mirrorHits.Load())
	}
	if len(auths) != 1 || auths[0] != "Bearer candidate-token" {
		t.Fatalf("候选源收到的 Authorization=%v", auths)
	}
	if f.officialHits.Load() != 0 {
		t.Fatalf("候选源成功后不应回官方，officialHits=%d", f.officialHits.Load())
	}
	if got := policy.sources(); len(got) != 1 || got[0] != id {
		t.Fatalf("策略解析收到的 SourceID=%v，应为候选源 %d", got, id)
	}
}
