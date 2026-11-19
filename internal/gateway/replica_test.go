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
	officialWait time.Duration
	replicaWait  time.Duration
	replicaCode  int
	mu           sync.Mutex
	officialPath string
	replicaPath  string
	// replicaHits 按 Registry 记录副本命中数（多副本夹具用）。
	replicaHits map[string]*atomic.Int32
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
		wait := f.officialWait
		f.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}
		w.Header().Set("Content-Type", manifestsAccept)
		w.Write(officialBody)
	}))
	t.Cleanup(official.Close)
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.replica.Add(1)
		f.mu.Lock()
		f.replicaPath = r.URL.Path
		wait, code := f.replicaWait, f.replicaCode
		f.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}
		if code != 0 {
			http.Error(w, "boom", code)
			return
		}
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
	// B20：跨站候选 = 可信映射 ∩ 调度器选中 ∩ 星标 ∩ 参选勾选。夹具必须
	// 把后两道门也置上，否则默认空勾选会（按冻结语义）退化为顺序回退。
	starred := true
	if err := st.UpdateSource(id, nil, nil, nil, &starred); err != nil {
		t.Fatalf("加星标: %v", err)
	}
	if err := st.SetCrossSelectedSources([]int64{id}, "admin", ""); err != nil {
		t.Fatalf("跨站参选勾选: %v", err)
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

// 首轮两路都慢于窗口：窗口从首票起算，慢候选只允许拖 200ms，
// 绝不能「窗口先关 → 零票 → 无结论 → 顺序回退被 dispatched Claim 抑制 → 502」。
func TestReplicaRaceSlowCandidatesStillConfirm(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	f.mu.Lock()
	f.officialWait = 300 * time.Millisecond
	f.replicaWait = 300 * time.Millisecond
	f.mu.Unlock()

	start := time.Now()
	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	elapsed := time.Since(start)
	if err != nil || !handled || e == nil {
		t.Fatalf("两路同内容即使都慢也必须确认: handled=%v err=%v", handled, err)
	}
	if e.SHA256 != shaOf(body) {
		t.Fatalf("内容不对: %s", e.SHA256)
	}
	if elapsed > time.Second {
		t.Fatalf("竞速尾延迟过高: %v", elapsed)
	}
	if f.official.Load() != 1 || f.replica.Load() != 1 {
		t.Fatalf("每候选只允许一次：official=%d replica=%d", f.official.Load(), f.replica.Load())
	}
}

// 竞速没有结论时，顺序回退必须落在同一条 B 链上，不能被 F19 的 dispatched
// Claim 抑制（那会把「副本不可用」变成 502）。
func TestReplicaNoConclusionFallsBackOnSameChain(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	f.mu.Lock()
	f.replicaCode = http.StatusInternalServerError
	f.mu.Unlock()

	e, notMod, err := f.gw.fetchWithReplicas(f.ctx(), f.plan, nil)
	if err != nil || notMod || e == nil {
		t.Fatalf("副本失败应回退权威而不是报错: notMod=%v err=%v", notMod, err)
	}
	if e.SHA256 != shaOf(body) {
		t.Fatalf("回退内容不对: %s", e.SHA256)
	}
	if f.official.Load() == 0 {
		t.Fatal("回退必须真的请求权威")
	}
}

// ---- 第四轮：权威站与 C/M 的边界、FlightKey、Claim 断言 ----

// addReplica 在夹具里追加第二个（或更多）可信副本 Registry：真实 Source、
// 真实 Host 能力提交、真实映射、星标与参选勾选。返回该副本的 Source ID。
func (f *replicaFixtureT) addReplica(t *testing.T, regHost, repo string, body []byte) int64 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", manifestsAccept)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	id, err := f.st.AddSource(store.Source{BaseURL: srv.URL, Name: "replica-" + regHost, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源 %s: %v", regHost, err)
	}
	if err := f.st.SetRepoHost(id, regHost); err != nil {
		t.Fatalf("repo_host %s: %v", regHost, err)
	}
	starred := true
	if err := f.st.UpdateSource(id, nil, nil, nil, &starred); err != nil {
		t.Fatalf("加星标 %s: %v", regHost, err)
	}
	gen, dig, err := f.st.HostGenState(regHost)
	if err != nil {
		t.Fatalf("gen %s: %v", regHost, err)
	}
	if res, err := f.st.CommitHostProbe(store.HostProbeCommit{
		SourceID: id, Host: regHost, Cap: store.RegistryCapYes, PathMode: "native",
		At: time.Now().Unix(), Gen: gen, CredDigest: dig, HasCredential: dig != "",
	}); err != nil || res != store.HostCapsCommitted {
		t.Fatalf("CommitHostProbe %s: %v %v", regHost, res, err)
	}
	if _, err := f.st.PutRegistryReplica(store.RegistryReplica{
		AuthorityRegistry: "registry.k8s.io", AuthorityRepository: "kube-apiserver",
		ReplicaRegistry: regHost, ReplicaRepository: repo,
		Priority: 20, Enabled: true,
	}, "admin", true); err != nil {
		t.Fatalf("副本映射 %s: %v", regHost, err)
	}
	if _, err := f.st.PutTrustedMapping(store.TrustedMapping{
		RegistryHost: regHost, SourceID: id, Priority: 20, Enabled: true,
	}, "admin", true); err != nil {
		t.Fatalf("Source 映射 %s: %v", regHost, err)
	}
	sel := append(f.st.CrossSelectedSources(), id)
	if err := f.st.SetCrossSelectedSources(sel, "admin", ""); err != nil {
		t.Fatalf("参选勾选 %s: %v", regHost, err)
	}
	f.mu.Lock()
	if f.replicaHits == nil {
		f.replicaHits = map[string]*atomic.Int32{}
	}
	f.replicaHits[regHost] = &hits
	f.mu.Unlock()
	return id
}

// 权威站不进入首轮 C：两个副本一致时必须直接确认，权威零派发。
// 若权威被错误地放进首轮（旧实现），official 至少被请求一次。
func TestReplicaAuthorityNotInFirstRound(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	f.plan.ExpectSHA = shaOf(body) // 内容寻址请求：副本正文自身即可证明身份
	f.plan.Immutable = true

	f.addReplica(t, "gcr.io", "mirror/kube-apiserver", body)

	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil || !handled || e == nil {
		t.Fatalf("两副本一致应确认: handled=%v err=%v", handled, err)
	}
	if e.SHA256 != shaOf(body) {
		t.Fatalf("内容不对: %s", e.SHA256)
	}
	if got := f.official.Load(); got != 0 {
		t.Fatalf("权威站在首轮 C 里被派发了 %d 次（应只由 Authority 按需派发，且两副本一致时不需要）", got)
	}
}

// 权威站不占 M 名额：5 条副本映射仍然只有 4 个副本候选（+1 个权威）。
func TestReplicaAuthorityNotCountedInM(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	for i, h := range []string{"gcr.io", "public.ecr.aws", "mcr.microsoft.com", "cgr.dev"} {
		f.addReplica(t, h, fmt.Sprintf("mirror/kube-apiserver-%d", i), body)
	}
	ts, _, err := f.gw.replicaTargets(f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	if len(ts) != crossReplicaM+1 {
		t.Fatalf("候选总数=%d，期望 4 个副本 + 1 个权威 = %d", len(ts), crossReplicaM+1)
	}
	if ts[0].sourceID != 0 {
		t.Fatalf("下标 0 必须是权威站，实际 sourceID=%d", ts[0].sourceID)
	}
	replicas := 0
	for _, c := range ts[1:] {
		if c.sourceID > 0 {
			replicas++
		}
	}
	if replicas != crossReplicaM {
		t.Fatalf("副本候选=%d，期望 M=%d（权威不占名额）", replicas, crossReplicaM)
	}
}

// 副本候选不产生额外 Claim：同一请求只有一条 Claim Dispatched。
func TestReplicaCandidatesShareSingleClaim(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	f.addReplica(t, "gcr.io", "mirror/kube-apiserver", body)
	f.plan.ExpectSHA = shaOf(body)
	f.plan.Immutable = true

	if _, _, err := f.gw.fetchWithReplicas(f.ctx(), f.plan, nil); err != nil {
		t.Fatalf("竞速: %v", err)
	}
	claims := f.sc.Claims()
	if len(claims) != 1 {
		t.Fatalf("同一请求只允许一条 Claim，实际 %d 条: %v", len(claims), claims)
	}
	for id, st := range claims {
		if id.Registry != "registry.k8s.io" {
			t.Fatalf("Claim 身份必须是权威逻辑 Registry，实际 %q", id.Registry)
		}
		if st != unified.ClaimDispatched {
			t.Fatalf("Claim 状态=%v，期望 Dispatched", st)
		}
	}
	if f.sc.DispatchedClaims() != 1 {
		t.Fatalf("DispatchedClaims=%d，期望 1", f.sc.DispatchedClaims())
	}
}

// FlightKey 必须按执行策略分命名空间：A 的 registry-race 与 B 的两档互不复用。
func TestCrossFlightKeyNamespaced(t *testing.T) {
	plan := &adapter.Plan{CacheKey: "registry|manifest|x/y:z|accept=a"}
	race := crossFlightKey("race", plan)
	seq := crossFlightKey("sequential", plan)
	if race == seq {
		t.Fatalf("两档模式不得共用 Flight: %q", race)
	}
	for _, k := range []string{race, seq} {
		if k == plan.CacheKey {
			t.Fatalf("副本 Flight 不能退化成裸 CacheKey: %q", k)
		}
		if strings.HasPrefix(k, flightPolicyRace+"|") == false && !strings.Contains(k, plan.CacheKey) {
			t.Fatalf("Flight 键必须包含 CacheKey: %q", k)
		}
	}
	// A 的竞速 Flight 与 B 的跨站 Flight 也必须互不相干。
	aRace := plan.CacheKey + "\x00policy=" + flightPolicyRace
	if aRace == race || aRace == seq {
		t.Fatalf("A 竞速与 B 跨站共用 Flight: a=%q b=%q", aRace, race)
	}
	if plan.CacheKey != "registry|manifest|x/y:z|accept=a" {
		t.Fatal("Flight 键不得改动 CacheKey 本身")
	}
}

// 可变 Tag：两个副本一致也不得直接写权威 Tag 缓存键，必须取得权威版本一致性。
func TestReplicaTagRequiresAuthorityAgreement(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	stale := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:stale"}]}`)
	f := newReplicaFixture(t, good, stale)
	f.addReplica(t, "gcr.io", "mirror/kube-apiserver", stale)
	if !mutableTagRef(f.plan) {
		t.Fatal("夹具必须是可变 Tag 场景")
	}
	// 两个副本都返回过期内容并互相一致：竞速会确认，但权威不同 → 不得提交。
	if _, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil); handled || err != nil {
		t.Fatalf("可变 Tag 与权威不一致时不得由副本交付: handled=%v err=%v", handled, err)
	}
	if e, _ := f.st.CacheGet(f.plan.CacheKey); e != nil && e.SHA256 == shaOf(stale) {
		t.Fatalf("过期副本内容写进了权威 Tag 缓存键: %s", e.SHA256)
	}
	if f.official.Load() != 1 {
		t.Fatalf("版本一致性必须由唯一一次 Authority 确认，实际权威派发 %d 次", f.official.Load())
	}
}

// 内容寻址请求不需要权威软比对：Digest 本身就是身份证明。
func TestReplicaDigestSkipsAuthority(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	f.addReplica(t, "gcr.io", "mirror/kube-apiserver", body)
	sum := sha256.Sum256(body)
	f.plan.Ref = "sha256:" + hex.EncodeToString(sum[:])
	f.plan.ExpectSHA = hex.EncodeToString(sum[:])
	f.plan.Immutable = true
	f.plan.Upstream.Path = "/v2/kube-apiserver/manifests/" + f.plan.Ref

	if _, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil); !handled || err != nil {
		t.Fatalf("Digest 请求两副本一致应确认: handled=%v err=%v", handled, err)
	}
	if f.official.Load() != 0 {
		t.Fatalf("Digest 请求无需权威，实际派发 %d 次", f.official.Load())
	}
}

// B20：空勾选 = 没有跨站候选（默认值即退化顺序回退）。
func TestReplicaEmptySelectionDegrades(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	if err := f.st.SetCrossSelectedSources(nil, "admin", ""); err != nil {
		t.Fatalf("清空勾选: %v", err)
	}
	ts, _, err := f.gw.replicaTargets(f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	if len(ts) != 0 {
		t.Fatalf("空勾选不得产生跨站候选，实际 %d 个", len(ts))
	}
	if _, handled, _ := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil); handled {
		t.Fatal("空勾选必须退化为原有顺序路径")
	}
	if f.replica.Load() != 0 {
		t.Fatalf("空勾选不得请求副本，实际 %d", f.replica.Load())
	}
}

// B20：未勾选的星标节点不进跨站候选。
func TestReplicaRequiresSelection(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	other, err := f.st.AddSource(store.Source{BaseURL: "https://example.invalid", Name: "other", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	starred := true
	if err := f.st.UpdateSource(other, nil, nil, nil, &starred); err != nil {
		t.Fatalf("加星标: %v", err)
	}
	// 勾选集合里只有另一个星标源：本副本未勾选 → 无候选。
	if err := f.st.SetCrossSelectedSources([]int64{other}, "admin", ""); err != nil {
		t.Fatalf("勾选: %v", err)
	}
	ts, _, err := f.gw.replicaTargets(f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	for _, c := range ts {
		if c.sourceID == f.srcID {
			t.Fatal("未勾选的星标节点不得进入跨站候选")
		}
	}
}

// B20：撤星后陈旧勾选条目读时过滤，不留幽灵资格。
func TestCrossSelectedStaleEntryFiltered(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	if got := f.st.EffectiveCrossSelected(); !got[f.srcID] {
		t.Fatalf("星标 + 勾选应生效: %v", got)
	}
	off := false
	if err := f.st.UpdateSource(f.srcID, nil, nil, nil, &off); err != nil {
		t.Fatalf("撤星: %v", err)
	}
	if got := f.st.EffectiveCrossSelected(); got[f.srcID] {
		t.Fatal("撤星后陈旧勾选条目必须失效")
	}
	ts, _, err := f.gw.replicaTargets(f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	if len(ts) != 0 {
		t.Fatalf("撤星后不得有跨站候选，实际 %d", len(ts))
	}
}

// B19：私有认证 Host 不进跨站候选。
func TestReplicaExcludesPrivateAuthority(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	f.gw.privAuth = PrivateAuthOptions{
		Enabled:      true,
		RequireToken: true,
		Hosts:        map[string]bool{"registry.k8s.io": true},
		Creds:        &fakeCreds{host: "registry.k8s.io"},
	}
	ts, _, err := f.gw.replicaTargets(f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	if len(ts) != 0 {
		t.Fatalf("私有权威站不得产生跨站候选，实际 %d", len(ts))
	}
}

// B17：两档模式，默认顺序回退；UI 设置优先于部署期配置，切换即时生效。
func TestReplicaModeDefaultsAndLiveSwitch(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)

	// 部署期零值 = 默认顺序回退（有候选才生效；默认空勾选仍是零候选）。
	f.gw.replica = ""
	if got := f.gw.replicaMode(); got != "sequential" {
		t.Fatalf("默认模式必须是 sequential，实际 %q", got)
	}
	// 部署期 off = 显式关闭。
	f.gw.replica = "off"
	if got := f.gw.replicaMode(); got != "" {
		t.Fatalf("off 应关闭副本路径，实际 %q", got)
	}
	// UI 设置优先：切到 race 立即生效（不靠 10s TTL）。
	if err := f.st.SetCrossRegistryMode("race", "admin", ""); err != nil {
		t.Fatalf("写模式: %v", err)
	}
	f.gw.InvalidateReplicaMode()
	if got := f.gw.replicaMode(); got != "race" {
		t.Fatalf("UI 切换应立即生效为 race，实际 %q", got)
	}
	// UI 的 off 覆盖部署期 sequential。
	if err := f.st.SetCrossRegistryMode("off", "admin", ""); err != nil {
		t.Fatalf("写模式: %v", err)
	}
	f.gw.InvalidateReplicaMode()
	if got := f.gw.replicaMode(); got != "" {
		t.Fatalf("UI off 应关闭副本路径，实际 %q", got)
	}
	// 切回顺序：之前的 race 结果不得复用（Flight 命名空间隔离）。
	if err := f.st.SetCrossRegistryMode("sequential", "admin", ""); err != nil {
		t.Fatalf("写模式: %v", err)
	}
	f.gw.InvalidateReplicaMode()
	if got := f.gw.replicaMode(); got != "sequential" {
		t.Fatalf("应读回 sequential，实际 %q", got)
	}
}
