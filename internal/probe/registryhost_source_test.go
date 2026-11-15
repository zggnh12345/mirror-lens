package probe

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

type probeRecordingPolicy struct {
	base v2auth.PolicyResolver
	mu   sync.Mutex
	ids  []int64
}

func (p *probeRecordingPolicy) Resolve(ctx context.Context, sourceID int64, host string) (v2auth.PolicySnapshot, error) {
	p.mu.Lock()
	p.ids = append(p.ids, sourceID)
	p.mu.Unlock()
	return p.base.Resolve(ctx, sourceID, host)
}

func (p *probeRecordingPolicy) sources() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.ids...)
}

// Probe 的认证也要把候选源 SourceID 带进策略，不能只带 Host。
func TestProbePolicyReceivesSourceID(t *testing.T) {
	f := &fakeHost{native: "ok", prefix: "404"}
	srv := f.start(t)
	rec := &probeRecordingPolicy{base: v2auth.NewLegacyHostPolicy(map[string][]string{"quay.io": {srv.URL}}, false)}

	out := ProbeRegistryHost(context.Background(), RegistryHostProbe{
		BaseURL:        srv.URL,
		SourceID:       77,
		Host:           "quay.io",
		ExpectedDigest: digestOf(dockerManifest),
		Snapshot:       v2auth.CredentialSnapshot{Host: "quay.io", Generation: 1, Digest: "d"},
		Policy:         rec,
		Timeout:        5 * time.Second,
	})
	if out.Cap != capYes {
		t.Fatalf("探测应成功: %+v", out)
	}
	ids := rec.sources()
	if len(ids) == 0 {
		t.Fatal("策略没有被调用")
	}
	for _, id := range ids {
		if id != 77 {
			t.Fatalf("策略收到的 SourceID=%v，应全部为 77", ids)
		}
	}
}
