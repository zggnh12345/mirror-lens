package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// candidateMissingPolicy 复现 11b 的真实装配：官方直连（sourceID=0）
// 走 OfficialPolicy 不查表，只有候选源（sourceID>0）缺策略。
// 用来验证 ErrPolicyMissing 不会被官方兜底的状态码盖掉（v1.6 §B/§E）。
type candidateMissingPolicy struct{}

func (candidateMissingPolicy) Resolve(ctx context.Context, sourceID int64, host string) (v2auth.PolicySnapshot, error) {
	if sourceID > 0 {
		return v2auth.PolicySnapshot{}, fmt.Errorf("%w: 候选源 %d 无策略", v2auth.ErrPolicyMissing, sourceID)
	}
	return v2auth.OfficialPolicy{}.Resolve(ctx, 0, host)
}

// 候选缺策略 + 官方兜底失败：必须 503 + X-ML-Error，不能被官方 401 伪装。
func TestCandidatePolicyMissingBeatsOfficialError(t *testing.T) {
	f := newMOFixture(t)
	f.officialStatus = http.StatusUnauthorized
	m := &moMirror{path: "/v2/library/alpine/manifests/latest", body: mirrorManifest, hits: &f.nativeHits}
	srv := httptest.NewServer(m)
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")
	f.gw.policies = candidateMissingPolicy{}

	w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%q headers=%v", w.Code, w.Body.String(), w.Header())
	}
	if got := w.Header().Get("X-ML-Error"); got != "auth_policy_missing" {
		t.Fatalf("X-ML-Error=%q headers=%v", got, w.Header())
	}
}

// 官方成功时降级语义不变：候选缺策略不能把 200 变成 503。
func TestOfficialSuccessStillWinsOverCandidatePolicyMissing(t *testing.T) {
	f := newMOFixture(t)
	m := &moMirror{path: "/v2/library/alpine/manifests/latest", body: mirrorManifest, hits: &f.nativeHits}
	srv := httptest.NewServer(m)
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")
	f.gw.policies = candidateMissingPolicy{}

	w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q headers=%v", w.Code, w.Body.String(), w.Header())
	}
	if got := w.Header().Get("X-ML-Error"); got != "" {
		t.Fatalf("官方成功不应标 X-ML-Error: %q", got)
	}
}
