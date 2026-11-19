package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/unified"
)

// 3.5.17 B 网关入口的受控端到端夹具：两个真实 HTTP 上游 + 真实 SQLite 映射。
// 这只证明代码路径与内容仲裁（SIMULATED_PASS），不代表任何公共站点。

type replicaFixtureT struct {
	gw       *Gateway
	st       *store.Store
	plan     *adapter.Plan
	official atomic.Int32
	replica  atomic.Int32
	sc       *unified.Scope
}

func newReplicaFixture(t *testing.T, officialBody, replicaBody []byte) *replicaFixtureT {
	t.Helper()
	f := &replicaFixtureT{}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}

	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.official.Add(1)
		w.Header().Set("Content-Type", manifestsAccept)
		w.Write(officialBody)
	}))
	t.Cleanup(official.Close)
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.replica.Add(1)
		w.Header().Set("Content-Type", manifestsAccept)
		w.Write(replicaBody)
	}))
	t.Cleanup(replica.Close)

	gw := New(st, Options{
		Policies:    testGatewayPolicy,
		Cache:       cs,
		MaxBytes:    8 << 20,
		Guard:       safehttp.New(safehttp.Options{AllowLoopback: true}),
		ReplicaMode: "race",
	})
	gw.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))
	off := official.URL
	// 官方逻辑 Host 走权威上游；副本源的 BaseURL 已是回环地址，直接放行。
	gw.cli = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.EqualFold(r.URL.Host, "registry.k8s.io") {
			u := *r.URL
			u.Scheme, u.Host = "http", strings.TrimPrefix(off, "http://")
			r2 := r.Clone(r.Context())
			r2.URL = &u
			return http.DefaultTransport.RoundTrip(r2)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}

	// 副本 Registry 的可信 Source：能力 + recovery 走真实提交路径。
	id, err := st.AddSource(store.Source{BaseURL: replica.URL, Name: "replica-mirror", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	if err := st.SetRepoHost(id, "quay.io"); err != nil {
		t.Fatalf("repo_host: %v", err)
	}
	gen, dig, err := st.HostGenState("quay.io")
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if res, err := st.CommitHostProbe(store.HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: store.RegistryCapYes, PathMode: "native",
		At: time.Now().Unix(), Gen: gen, CredDigest: dig, HasCredential: dig != "",
	}); err != nil || res != store.HostCapsCommitted {
		t.Fatalf("CommitHostProbe: %v %v", res, err)
	}
	// 先 replicas（仓库→仓库），再 Source 映射（Host→Source）。
	if _, err := st.PutRegistryReplica(store.RegistryReplica{
		AuthorityRegistry: "registry.k8s.io", AuthorityRepository: "kube-apiserver",
		ReplicaRegistry: "quay.io", ReplicaRepository: "mirror/kube-apiserver",
		Priority: 10, Enabled: true,
	}, "admin", true); err != nil {
		t.Fatalf("副本映射: %v", err)
	}
	if _, err := st.PutTrustedMapping(store.TrustedMapping{
		RegistryHost: "quay.io", SourceID: id, Priority: 10, Enabled: true,
	}, "admin", true); err != nil {
		t.Fatalf("Source 映射: %v", err)
	}

	sc, err := unified.NewScope(context.Background(), unified.ScopeConfig{}, nil)
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	t.Cleanup(sc.Close)

	plan := &adapter.Plan{
		Kind:         "registry",
		ResourceKind: adapter.ResourceManifest,
		SchedKey:     "registry:registry.k8s.io",
		LogicalHost:  "registry.k8s.io",
		Repo:         "kube-apiserver",
		Ref:          "v1.30.0",
		Registry:     true,
		Upstream: &url.URL{Scheme: "https", Host: "registry.k8s.io",
			Path: "/v2/kube-apiserver/manifests/v1.30.0"},
		Method:    http.MethodGet,
		Header:    http.Header{"Accept": {manifestsAccept}},
		CacheKey:  "registry|manifest|registry.k8s.io/kube-apiserver:v1.30.0|accept=x",
		Cacheable: true,
		TTL:       time.Minute,
	}
	f.gw, f.st, f.plan, f.sc = gw, st, plan, sc
	return f
}

func (f *replicaFixtureT) ctx() context.Context {
	return unified.WithScope(context.Background(), f.sc)
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// 两路一致：直接确认并交付，不需要 Authority；缓存写入的是同一内容。
func TestReplicaRaceConfirmsOnAgreement(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)

	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil || !handled || e == nil {
		t.Fatalf("竞速应确认: handled=%v err=%v", handled, err)
	}
	if e.SHA256 != shaOf(body) {
		t.Fatalf("缓存条目 digest=%s，期望 %s", e.SHA256, shaOf(body))
	}
	if f.replica.Load() == 0 {
		t.Fatal("副本候选必须被真实请求")
	}
	if _, err := f.gw.cache.Open(e.SHA256); err != nil {
		t.Fatalf("缓存文件应可读: %v", err)
	}
}

// 两路不一致：只允许一次 Authority（即官方那一次派发），最终交付权威正文。
func TestReplicaRaceMismatchFallsBackToAuthority(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	bad := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:bad"}]}`)
	f := newReplicaFixture(t, good, bad)

	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil || !handled || e == nil {
		t.Fatalf("不一致时应由权威兜底: handled=%v err=%v", handled, err)
	}
	if e.SHA256 != shaOf(good) {
		t.Fatalf("必须交付权威正文，实得 %s（坏副本 %s）", e.SHA256, shaOf(bad))
	}
	if f.official.Load() != 1 {
		t.Fatalf("权威只允许一次真实派发，实际 %d", f.official.Load())
	}
}

// 没有 trusted_source_mappings 的副本不参与竞速（分层解析，不隐式放行）。
func TestReplicaWithoutSourceMappingSkips(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	f := newReplicaFixture(t, body, body)
	if _, err := f.st.DeleteTrustedMapping("quay.io", 1, "admin"); err == nil {
		// 删掉刚建的映射；id 不一定是 1，忽略错误再扫一遍。
	}
	all, _ := f.st.ListTrustedMappings("quay.io")
	for _, m := range all {
		_, _ = f.st.DeleteTrustedMapping("quay.io", m.SourceID, "admin")
	}
	if _, handled, _ := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil); handled {
		t.Fatal("没有 Source 映射的副本不得形成竞速")
	}
	if f.replica.Load() != 0 {
		t.Fatalf("未映射的副本不应被请求，实际 %d", f.replica.Load())
	}
}
