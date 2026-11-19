package gateway

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// B15 的进程内对照：只测「跨站候选发现」这一步（B 关闭/空勾选时唯一多出来的
// 热路径成本）。它不代表端到端时延，也不能当成真实外部 Registry 的性能证据。
func benchReplicaGateway(b *testing.B, replicas int, selectAll bool) *Gateway {
	b.Helper()
	dir := b.TempDir()
	st, err := store.Open(filepath.Join(dir, "b.db"))
	if err != nil {
		b.Fatalf("打开库: %v", err)
	}
	b.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		b.Fatalf("缓存: %v", err)
	}
	gw := New(st, Options{Cache: cs, MaxBytes: 8 << 20, ReplicaMode: "race"})
	gw.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))
	ids := make([]int64, 0, replicas)
	for i, h := range []string{"quay.io", "gcr.io", "public.ecr.aws", "mcr.microsoft.com", "cgr.dev"} {
		if i >= replicas {
			break
		}
		id, err := st.AddSource(store.Source{BaseURL: "https://" + h, Name: "r-" + h, Status: "usable", Enabled: true})
		if err != nil {
			b.Fatalf("加源: %v", err)
		}
		if err := st.SetRepoHost(id, h); err != nil {
			b.Fatalf("repo_host: %v", err)
		}
		starred := true
		if err := st.UpdateSource(id, nil, nil, nil, &starred); err != nil {
			b.Fatalf("加星标: %v", err)
		}
		gen, dig, err := st.HostGenState(h)
		if err != nil {
			b.Fatalf("gen: %v", err)
		}
		if res, err := st.CommitHostProbe(store.HostProbeCommit{
			SourceID: id, Host: h, Cap: store.RegistryCapYes, PathMode: "native",
			At: time.Now().Unix(), Gen: gen, CredDigest: dig, HasCredential: dig != "",
		}); err != nil || res != store.HostCapsCommitted {
			b.Fatalf("CommitHostProbe: %v %v", res, err)
		}
		if _, err := st.PutRegistryReplica(store.RegistryReplica{
			AuthorityRegistry: "registry.k8s.io", AuthorityRepository: "kube-apiserver",
			ReplicaRegistry: h, ReplicaRepository: "mirror/kube-apiserver",
			Priority: 10 + i, Enabled: true,
		}, "bench", true); err != nil {
			b.Fatalf("副本映射: %v", err)
		}
		if _, err := st.PutTrustedMapping(store.TrustedMapping{
			RegistryHost: h, SourceID: id, Priority: 10 + i, Enabled: true,
		}, "bench", true); err != nil {
			b.Fatalf("Source 映射: %v", err)
		}
		ids = append(ids, id)
	}
	if selectAll {
		if err := st.SetCrossSelectedSources(ids, "bench", ""); err != nil {
			b.Fatalf("勾选: %v", err)
		}
	}
	gw.now = time.Now
	return gw
}

func benchReplicaPlan() *adapter.Plan {
	return &adapter.Plan{
		Kind: "registry", ResourceKind: adapter.ResourceManifest,
		SchedKey: "registry:registry.k8s.io", LogicalHost: "registry.k8s.io",
		Repo: "kube-apiserver", Ref: "v1.30.0", Registry: true,
		Upstream: &url.URL{Scheme: "https", Host: "registry.k8s.io",
			Path: "/v2/kube-apiserver/manifests/v1.30.0"},
		CacheKey:  "registry|manifest|registry.k8s.io/kube-apiserver:v1.30.0|accept=x",
		Cacheable: true,
	}
}

// B 关闭（空勾选）时只多一次设置读取。
func BenchmarkReplicaTargetsNoSelection(b *testing.B) {
	gw := benchReplicaGateway(b, 4, false)
	plan := benchReplicaPlan()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ts, _, _ := gw.replicaTargets(plan, time.Now()); len(ts) != 0 {
			b.Fatalf("空勾选不应产生候选: %d", len(ts))
		}
	}
}

// M=4 全配：4 副本 + 1 权威。
func BenchmarkReplicaTargetsFourReplicas(b *testing.B) {
	gw := benchReplicaGateway(b, 4, true)
	plan := benchReplicaPlan()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts, _, err := gw.replicaTargets(plan, time.Now())
		if err != nil || len(ts) != 5 {
			b.Fatalf("候选=%d err=%v", len(ts), err)
		}
	}
}
