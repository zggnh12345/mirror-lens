package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
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
	gw           *Gateway
	st           *store.Store
	plan         *adapter.Plan
	official     atomic.Int32
	replica      atomic.Int32
	sc           *unified.Scope
	srcID        int64
	mu           sync.Mutex
	officialPath string
	replicaPath  string
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
		f.mu.Lock()
		f.officialPath = r.URL.Path
		f.mu.Unlock()
		w.Header().Set("Content-Type", manifestsAccept)
		w.Write(officialBody)
	}))
	t.Cleanup(official.Close)
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.replica.Add(1)
		f.mu.Lock()
		f.replicaPath = r.URL.Path
		f.mu.Unlock()
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
	f.gw, f.st, f.plan, f.sc, f.srcID = gw, st, plan, sc, id
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
	if got := f.replica.Load(); got != 1 {
		t.Fatalf("每个候选只允许下载一次，副本实际 %d", got)
	}
	if got := f.official.Load(); got != 1 {
		t.Fatalf("权威候选只允许下载一次，实际 %d", got)
	}
	if _, err := f.gw.cache.Open(e.SHA256); err != nil {
		t.Fatalf("缓存文件应可读: %v", err)
	}
	f.mu.Lock()
	offPath, repPath := f.officialPath, f.replicaPath
	f.mu.Unlock()
	if offPath != "/v2/kube-apiserver/manifests/v1.30.0" {
		t.Fatalf("权威对上游路径=%s", offPath)
	}
	if repPath != "/v2/mirror/kube-apiserver/manifests/v1.30.0" {
		t.Fatalf("副本路径=%s（replica_repository 未被使用？）", repPath)
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
	// 正向对照：把映射加回同一个（已有能力与 recovery 的）源，同一夹具
	// 必须立刻能竞速，排除「因为别的原因没跑」。
	if _, err := f.st.PutTrustedMapping(store.TrustedMapping{
		RegistryHost: "quay.io", SourceID: f.srcID, Priority: 10, Enabled: true,
	}, "admin", true); err != nil {
		t.Fatalf("重新加映射: %v", err)
	}
	if _, handled, _ := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil); !handled {
		t.Fatal("有 Source 映射时应能形成竞速")
	}
}

// 内容寻址请求：副本正文必须逐字节等于请求 Digest，否则不能落盘/交付。
func TestReplicaSequentialRejectsWrongDigest(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	bad := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:evil"}]}`)
	f := newReplicaFixture(t, good, bad)
	f.gw.replica = "sequential"
	// 客户端按 Digest 请求：身份与内容都必须等于该 Digest。
	sum := sha256.Sum256(good)
	f.plan.Ref = "sha256:" + hex.EncodeToString(sum[:])
	f.plan.ExpectSHA = hex.EncodeToString(sum[:])
	f.plan.Immutable = true
	f.plan.Upstream.Path = "/v2/kube-apiserver/manifests/" + f.plan.Ref

	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil || !handled || e == nil {
		t.Fatalf("错内容副本应被跳过、由权威交付: handled=%v err=%v", handled, err)
	}
	if e.SHA256 != shaOf(good) {
		t.Fatalf("必须交付与请求 Digest 一致的正文，实得 %s", e.SHA256)
	}
	if e.SHA256 == shaOf(bad) {
		t.Fatal("错 Digest 的副本内容被当成权威内容提交")
	}
}

// 并发同 Key：只跑一轮竞速，回源次数不随并发放大。
func TestReplicaRaceMergesConcurrentRequests(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			e, notMod, err := f.gw.fetchWithReplicas(f.ctx(), f.plan, nil)
			if err != nil || notMod || e == nil || e.SHA256 != shaOf(body) {
				errs <- fmt.Errorf("结果不一致: notMod=%v err=%v", notMod, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := f.official.Load(); got != 1 {
		t.Fatalf("并发请求必须共享一轮竞速，权威实际 %d 次", got)
	}
	if got := f.replica.Load(); got != 1 {
		t.Fatalf("并发请求必须共享一轮竞速，副本实际 %d 次", got)
	}
}
