package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 多源 E2E 夹具：一个假官方（同时当 Hub 上游）+ 若干假镜像源。
type moFixture struct {
	gw     *Gateway
	st     *store.Store
	ad     *registryadapter.Adapter
	token  string
	dbPath string
	host   string // 逻辑 Host，默认 quay.io
	// officialStatus/officialHdr 让 HEAD 用例控制官方源的状态与挑战头；
	// 默认 0 表示照旧返回 200 清单。officialBody 用于 GET blob 时提供
	// 与请求 digest 一致的真实字节。
	officialStatus int
	officialHdr    http.Header
	officialBody   []byte
	// native / hostPrefix 分别记录各自命中的路径；official 记录官方回源次数。
	nativeHits   atomic.Int32
	prefixHits   atomic.Int32
	officialHits atomic.Int32
	// officialTargets 记录官方请求实际使用的逻辑 Host，用于 F7 证明
	// “没有候选命中”时确实回到对应 Host，而不是笼统的 Docker Hub。
	officialMu      sync.Mutex
	officialTargets []string
}

type moMirror struct {
	path   string // 期望收到的路径（含 /v2/ 前缀）
	status int
	body   string
	hdr    http.Header // 额外响应头（429 的 Retry-After、200 的 digest 等）
	hits   *atomic.Int32
	// abort 为 true 时先发一半 body 再断开，模拟已提交响应后上游失败。
	abort bool
}

func (m *moMirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.hits.Add(1)
	for k, vs := range m.hdr {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if r.URL.Path != m.path {
		http.NotFound(w, r)
		return
	}
	if m.status != 0 && m.status != 200 {
		w.WriteHeader(m.status)
		return
	}
	if m.abort {
		w.Header().Set("Content-Type", manifestsAccept)
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(200)
		w.Write([]byte(`{"schemaVersion":2,"half":`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Content-Type", manifestsAccept)
	// tag 清单带缓存头会更贴近真实上游；这里保持最小。
	w.Write([]byte(m.body))
}

const (
	officialManifest = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","official":true}`
	mirrorManifest   = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","mirror":"native"}`
	prefixManifest   = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","mirror":"prefix"}`
)

func newMOFixture(t *testing.T) *moFixture {
	t.Helper()
	f := &moFixture{host: "quay.io"}
	dir := t.TempDir()
	f.dbPath = filepath.Join(dir, "t.db")
	st, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}

	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.officialHits.Add(1)
		for k, vs := range f.officialHdr {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if f.officialStatus != 0 {
			w.WriteHeader(f.officialStatus)
			return
		}
		w.Header().Set("Content-Type", manifestsAccept)
		if len(f.officialBody) > 0 {
			w.Write(f.officialBody)
			return
		}
		w.Write([]byte(officialManifest))
	}))
	t.Cleanup(official.Close)

	host := strings.TrimPrefix(official.URL, "http://")
	ad := registryadapter.New(host, 4<<30, 5*time.Minute)
	ad.Scheme = "http"
	ad.MultiOriginHosts = []string{f.host}

	f.gw = New(st, Options{
		Policies: testGatewayPolicy,
		Cache:    cs,
		MaxBytes: 256 << 20,
		Registry: ad,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	f.gw.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))
	// 官方上游按逻辑 Host 拼 URL（这里就是 quay.io），测试里把它导到假官方，
	// 否则会打真实外网，回一个 401 挑战。
	off := official.URL
	f.gw.cli = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.EqualFold(r.URL.Host, f.host) {
			f.officialMu.Lock()
			f.officialTargets = append(f.officialTargets, r.URL.Host)
			f.officialMu.Unlock()
			u := *r.URL
			u.Scheme, u.Host = "http", strings.TrimPrefix(off, "http://")
			r2 := r.Clone(r.Context())
			r2.URL = &u
			r2.Host = u.Host
			return http.DefaultTransport.RoundTrip(r2)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	f.st, f.ad = st, ad
	plain, _, err := st.CreateToken("docker")
	if err != nil {
		t.Fatalf("建 Token: %v", err)
	}
	f.token = plain
	return f
}

// addRegistrySource 建一个 quay.io 的镜像源，并把它标成已验证的 registry 能力。
func (f *moFixture) addRegistrySource(t *testing.T, srv *httptest.Server, mode string) int64 {
	t.Helper()
	id, err := f.st.AddSource(store.Source{BaseURL: srv.URL, Name: srv.URL, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	if err := f.st.SetRepoHost(id, f.host); err != nil {
		t.Fatalf("repo_host: %v", err)
	}
	gen, dig, err := f.st.HostGenState(f.host)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	// C5 起候选准入要求 recovery 证据：走真实提交路径同时写 cap、path
	// 与 recovery=ok，而不是只改 sources 的展示列。
	if res, err := f.st.CommitHostProbe(store.HostProbeCommit{
		SourceID: id, Host: f.host, Cap: store.RegistryCapYes, PathMode: mode,
		At: time.Now().Unix(), Gen: gen, CredDigest: dig, HasCredential: dig != "",
	}); err != nil || res != store.HostCapsCommitted {
		t.Fatalf("CommitHostProbe: %v %v", res, err)
	}
	return id
}

func (f *moFixture) officialTargetLog() []string {
	f.officialMu.Lock()
	defer f.officialMu.Unlock()
	out := make([]string, len(f.officialTargets))
	copy(out, f.officialTargets)
	return out
}

func (f *moFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://gw"+path, nil)
	r.SetBasicAuth("x", f.token)
	w := httptest.NewRecorder()
	f.gw.ServeRegistry(w, r)
	return w
}

// 策略解析失败必须走完整 Gateway 路径 fail closed：503 + 可识别的 X-ML-Error。
func TestGatewayPolicyMissingHeader(t *testing.T) {
	f := newMOFixture(t)
	f.officialStatus = http.StatusNotFound // 迫使官方失败，进入候选源路径
	m := &moMirror{
		path: "/v2/library/alpine/manifests/latest",
		body: mirrorManifest, hits: &f.nativeHits,
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")
	f.gw.policies = failingPolicy{}

	w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ML-Error"); got != "auth_policy_missing" {
		t.Fatalf("X-ML-Error=%q headers=%v", got, w.Header())
	}
}

// native 源命中：请求路径原样带到源，官方零回源。
func TestMultiOriginNativeHit(t *testing.T) {
	f := newMOFixture(t)
	m := &moMirror{path: "/v2/library/alpine/manifests/latest", body: mirrorManifest, hits: &f.nativeHits}
	srv := httptest.NewServer(m)
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != mirrorManifest {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if f.nativeHits.Load() != 1 || f.officialHits.Load() != 0 {
		t.Fatalf("命中 native=%d official=%d", f.nativeHits.Load(), f.officialHits.Load())
	}
}

// 候选 Blob 的 Digest 和缓存键：第二次同 Digest 请求应命中缓存，
// 不再打候选源，内容仍与 Digest 一致。
func TestMultiOriginCandidateBlobDigestAndCache(t *testing.T) {
	f := newMOFixture(t)
	body := []byte("candidate-blob-content-for-digest")
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	path := "/v2/library/alpine/blobs/" + digest
	srv := httptest.NewServer(&moMirror{
		path: path, body: string(body), hits: &f.nativeHits,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/quay.io/library/alpine/blobs/"+digest)
	if w.Code != http.StatusOK || w.Body.String() != string(body) {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ML-Source"); got == "" {
		t.Fatal("候选 Blob 命中时必须带 X-ML-Source")
	}
	if f.nativeHits.Load() != 1 || f.officialHits.Load() != 0 {
		t.Fatalf("native=%d official=%d，首次应只命中候选", f.nativeHits.Load(), f.officialHits.Load())
	}

	w2 := f.get(t, "/v2/quay.io/library/alpine/blobs/"+digest)
	if w2.Code != http.StatusOK || w2.Body.String() != string(body) {
		t.Fatalf("缓存命中 status=%d body=%q", w2.Code, w2.Body.String())
	}
	if f.nativeHits.Load() != 1 {
		t.Fatalf("第二次同 Digest 应命中缓存，候选请求次数=%d", f.nativeHits.Load())
	}
}

// 第一批八个 Host 都要能实际命中候选源，不能只验证 quay.io。
func TestMultiOriginFirstBatchCandidateHit(t *testing.T) {
	for _, host := range []string{
		"gcr.io", "registry.k8s.io", "us-docker.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	} {
		t.Run(host, func(t *testing.T) {
			f := newMOFixture(t)
			f.host = host
			f.ad.MultiOriginHosts = []string{host}
			srv := httptest.NewServer(&moMirror{
				path: "/v2/foo/bar/manifests/latest",
				body: mirrorManifest, hits: &f.nativeHits,
			})
			defer srv.Close()
			f.addRegistrySource(t, srv, "native")

			w := f.get(t, "/v2/"+host+"/foo/bar/manifests/latest")
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != mirrorManifest {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-ML-Source"); got == "" {
				t.Fatal("命中候选源时必须带 X-ML-Source")
			}
			if f.nativeHits.Load() != 1 || f.officialHits.Load() != 0 {
				t.Fatalf("native=%d official=%d，期望命中候选且不回官方", f.nativeHits.Load(), f.officialHits.Load())
			}
		})
	}
}

// 第一批八个 Host 的 host_prefix 路径模式也要逐一命中，不能只验证 quay.io。
func TestMultiOriginFirstBatchHostPrefixHit(t *testing.T) {
	for _, host := range []string{
		"gcr.io", "registry.k8s.io", "us-docker.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	} {
		t.Run(host, func(t *testing.T) {
			f := newMOFixture(t)
			f.host = host
			f.ad.MultiOriginHosts = []string{host}
			path := "/v2/" + host + "/foo/bar/manifests/latest"
			srv := httptest.NewServer(&moMirror{
				path: path, body: prefixManifest, hits: &f.prefixHits,
			})
			defer srv.Close()
			f.addRegistrySource(t, srv, "host_prefix")

			w := f.get(t, path)
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != prefixManifest {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-ML-Source"); got == "" {
				t.Fatal("host_prefix 命中时必须带 X-ML-Source")
			}
			if f.prefixHits.Load() != 1 || f.officialHits.Load() != 0 {
				t.Fatalf("prefix=%d official=%d，期望命中候选且不回官方", f.prefixHits.Load(), f.officialHits.Load())
			}
		})
	}
}

// 每个 Host 的候选 401 都必须回官方；401 是中性事件，不进冷却。
func TestMultiOriginFirstBatchCandidate401FallsBackOfficial(t *testing.T) {
	for _, host := range []string{
		"gcr.io", "registry.k8s.io", "us-docker.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	} {
		t.Run(host, func(t *testing.T) {
			f := newMOFixture(t)
			f.host = host
			f.ad.MultiOriginHosts = []string{host}
			srv := httptest.NewServer(&moMirror{
				path: "/v2/foo/bar/manifests/latest", status: http.StatusUnauthorized,
				hits: &f.nativeHits,
			})
			defer srv.Close()
			f.addRegistrySource(t, srv, "native")

			w := f.get(t, "/v2/"+host+"/foo/bar/manifests/latest")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"official":true`) {
				t.Fatalf("status=%d body=%q，候选 401 后应回官方", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-ML-Source"); got != "" {
				t.Fatalf("回官方时 X-ML-Source 应为空，得到 %q", got)
			}
			if f.nativeHits.Load() != 1 || f.officialHits.Load() != 1 {
				t.Fatalf("native=%d official=%d，期望各一次", f.nativeHits.Load(), f.officialHits.Load())
			}
		})
	}
}

// 每个 Host 的候选网络失败都必须回官方；不能把连接失败当成最终结果。
func TestMultiOriginFirstBatchCandidateNetworkFailureFallsBackOfficial(t *testing.T) {
	for _, host := range []string{
		"gcr.io", "registry.k8s.io", "us-docker.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	} {
		t.Run(host, func(t *testing.T) {
			f := newMOFixture(t)
			f.host = host
			f.ad.MultiOriginHosts = []string{host}
			srv := httptest.NewServer(&moMirror{
				path: "/v2/foo/bar/manifests/latest", hits: &f.nativeHits,
			})
			f.addRegistrySource(t, srv, "native")
			srv.Close() // 候选连接直接失败

			w := f.get(t, "/v2/"+host+"/foo/bar/manifests/latest")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"official":true`) {
				t.Fatalf("status=%d body=%q，候选网络失败后应回官方", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-ML-Source"); got != "" {
				t.Fatalf("回官方时 X-ML-Source 应为空，得到 %q", got)
			}
			if f.nativeHits.Load() != 0 || f.officialHits.Load() != 1 {
				t.Fatalf("native=%d official=%d，期望候选未命中、官方一次", f.nativeHits.Load(), f.officialHits.Load())
			}
		})
	}
}

// 候选和官方都失败：最终必须是官方失败，不能伪装成 200。
func TestMultiOriginCandidateAndOfficialFailureIsNotSuccess(t *testing.T) {
	f := newMOFixture(t)
	f.officialStatus = http.StatusInternalServerError
	srv := httptest.NewServer(&moMirror{
		path: "/v2/foo/bar/manifests/latest", status: http.StatusInternalServerError,
		hits: &f.nativeHits,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/quay.io/foo/bar/manifests/latest")
	// 候选和官方都失败时网关统一包装成 502；关键是绝不能返回 200
	// 或官方成功正文。
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d，候选和官方都失败时应返回 502", w.Code)
	}
	if strings.Contains(w.Body.String(), `"official":true`) {
		t.Fatalf("失败响应不能伪装成官方成功: %q", w.Body.String())
	}
	if f.nativeHits.Load() != 1 || f.officialHits.Load() != 1 {
		t.Fatalf("native=%d official=%d，期望各一次", f.nativeHits.Load(), f.officialHits.Load())
	}
}

// 空范围时八个 Host 都必须记录到对应官方 Host，而不是笼统的 Docker Hub。
func TestMultiOriginNoSourceOfficialTargets(t *testing.T) {
	hosts := []string{
		"gcr.io", "registry.k8s.io", "us-docker.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	}
	f := newMOFixture(t)
	f.ad.MultiOriginHosts = hosts
	for _, host := range hosts {
		f.host = host
		w := f.get(t, "/v2/"+host+"/foo/bar/manifests/latest")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"official":true`) {
			t.Fatalf("%s 空范围应回官方: status=%d body=%q", host, w.Code, w.Body.String())
		}
	}
	targets := f.officialTargetLog()
	if len(targets) != len(hosts) {
		t.Fatalf("官方请求 %d 次，期望 %d 次: %v", len(targets), len(hosts), targets)
	}
	seen := make(map[string]bool, len(hosts))
	for _, h := range targets {
		seen[h] = true
	}
	for _, host := range hosts {
		if !seen[host] {
			t.Errorf("没有记录到官方目标 %s，实际 %v", host, targets)
		}
	}
	if f.nativeHits.Load() != 0 || f.prefixHits.Load() != 0 {
		t.Fatalf("空范围不应命中候选: native=%d prefix=%d", f.nativeHits.Load(), f.prefixHits.Load())
	}
}

// host_prefix 源命中：路径必须插入 Host 段。
func TestMultiOriginHostPrefixHit(t *testing.T) {
	f := newMOFixture(t)
	m := &moMirror{path: "/v2/quay.io/library/alpine/manifests/latest", body: prefixManifest, hits: &f.prefixHits}
	srv := httptest.NewServer(m)
	defer srv.Close()
	f.addRegistrySource(t, srv, "host_prefix")

	w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != prefixManifest {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if f.prefixHits.Load() != 1 || f.officialHits.Load() != 0 {
		t.Fatalf("命中 prefix=%d official=%d", f.prefixHits.Load(), f.officialHits.Load())
	}
}

// 两条已验证源都失败：回官方，且每个源只按自己那条路径试一次。
func TestMultiOriginAllFailFallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	n := &moMirror{path: "/v2/library/alpine/manifests/latest", status: 500, hits: &f.nativeHits}
	nsrv := httptest.NewServer(n)
	defer nsrv.Close()
	p := &moMirror{path: "/v2/quay.io/library/alpine/manifests/latest", status: 404, hits: &f.prefixHits}
	psrv := httptest.NewServer(p)
	defer psrv.Close()
	f.addRegistrySource(t, nsrv, "native")
	f.addRegistrySource(t, psrv, "host_prefix")

	w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"official":true`) {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if f.nativeHits.Load() != 1 || f.prefixHits.Load() != 1 || f.officialHits.Load() != 1 {
		t.Fatalf("native=%d prefix=%d official=%d，每个源应只试一次、官方一次",
			f.nativeHits.Load(), f.prefixHits.Load(), f.officialHits.Load())
	}
}

// Registry 失败不得写进 Docker 健康维度，也不得影响 docker 候选池。
func TestMultiOriginFailureKeepsDockerHealthClean(t *testing.T) {
	f := newMOFixture(t)
	n := &moMirror{path: "/v2/library/alpine/manifests/latest", status: 500, hits: &f.nativeHits}
	srv := httptest.NewServer(n)
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")
	// 另有一台真正的 docker 源，验证它不被牵连。
	dockerID, err := f.st.AddSource(store.Source{BaseURL: "https://hub.example", Name: "hub", DockerCap: 1, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	if w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest"); w.Code != 200 {
		t.Fatalf("回官方应 200，得到 %d", w.Code)
	}

	var dockerFails int
	for _, v := range mustHealth(t, f.st, dockerID) {
		dockerFails += v
	}
	if dockerFails != 0 {
		t.Errorf("docker 源被 registry 失败污染: %d", dockerFails)
	}
	for fam, v := range mustHealth(t, f.st, regID) {
		if fam == "docker" && v > 0 {
			t.Errorf("registry 源在 docker 族留下失败记录: %d", v)
		}
	}
	if n := len(f.gw.sched.Candidates("docker")); n != 1 {
		t.Errorf("docker 候选池数量=%d，应为 1", n)
	}
	// 失败后该源进入熔断层（按 kind+源 隔离），下一次请求直接走官方；
	// 这是调度器的预期行为，不是健康度被污染。
	if n := len(f.gw.sched.Candidates("registry:quay.io")); n != 0 {
		t.Errorf("失败后应进入冷却，registry 候选池=%d", n)
	}
}

// Phase 1 目标 Host：候选失败仍要能回官方，且不会被当成 Docker 通道。
func TestMultiOriginGcrFallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	f.host = "gcr.io"
	f.ad.MultiOriginHosts = []string{"gcr.io"}

	n := &moMirror{path: "/v2/foo/bar/manifests/latest", status: 500, hits: &f.nativeHits}
	srv := httptest.NewServer(n)
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/gcr.io/foo/bar/manifests/latest")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"official":true`) {
		t.Fatalf("候选失败应回官方: status=%d body=%q", w.Code, w.Body.String())
	}
	if f.nativeHits.Load() != 1 || f.officialHits.Load() != 1 {
		t.Fatalf("native=%d official=%d，期望各一次", f.nativeHits.Load(), f.officialHits.Load())
	}
}

// 没有 Source 的 Host 不能因为开启多源而改变回源结果：仍应直接回官方。
func TestMultiOriginNoSourceFallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	f.ad.MultiOriginHosts = []string{
		"gcr.io", "registry.k8s.io", "*.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	}
	for _, host := range []string{
		"gcr.io", "registry.k8s.io", "us-docker.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	} {
		f.host = host
		w := f.get(t, "/v2/foo/bar/manifests/latest")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"official":true`) {
			t.Fatalf("%s 无 Source 时应回官方: status=%d body=%q", host, w.Code, w.Body.String())
		}
	}
	if n := f.nativeHits.Load() + f.prefixHits.Load(); n != 0 {
		t.Fatalf("没有 Source 时不应命中候选，实际 %d", n)
	}
}

// 第一批每个 Host 都要能完成“候选失败 → 官方成功”，不能只验证 gcr。
func TestMultiOriginFirstBatchCandidateFailureFallsBackOfficial(t *testing.T) {
	for _, host := range []string{
		"gcr.io", "registry.k8s.io", "us-docker.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	} {
		t.Run(host, func(t *testing.T) {
			f := newMOFixture(t)
			f.host = host
			f.ad.MultiOriginHosts = []string{host}
			n := &moMirror{path: "/v2/foo/bar/manifests/latest", status: 500, hits: &f.nativeHits}
			srv := httptest.NewServer(n)
			defer srv.Close()
			f.addRegistrySource(t, srv, "native")

			w := f.get(t, "/v2/"+host+"/foo/bar/manifests/latest")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"official":true`) {
				t.Fatalf("候选失败应回官方: status=%d body=%q", w.Code, w.Body.String())
			}
			if f.nativeHits.Load() != 1 || f.officialHits.Load() != 1 {
				t.Fatalf("native=%d official=%d，期望各一次", f.nativeHits.Load(), f.officialHits.Load())
			}
		})
	}
}

// 每个 Host 的失败只进入自己的 registry:<host> 冷却，不污染 Docker 族。
func TestMultiOriginFirstBatchFailureIsolation(t *testing.T) {
	for _, host := range []string{
		"gcr.io", "registry.k8s.io", "us-docker.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev",
	} {
		t.Run(host, func(t *testing.T) {
			f := newMOFixture(t)
			f.host = host
			f.ad.MultiOriginHosts = []string{host}
			n := &moMirror{path: "/v2/foo/bar/manifests/latest", status: 500, hits: &f.nativeHits}
			srv := httptest.NewServer(n)
			defer srv.Close()
			id := f.addRegistrySource(t, srv, "native")

			if w := f.get(t, "/v2/"+host+"/foo/bar/manifests/latest"); w.Code != http.StatusOK {
				t.Fatalf("应回官方 200，得到 %d", w.Code)
			}
			for fam, v := range mustHealth(t, f.st, id) {
				if fam == "docker" && v > 0 {
					t.Errorf("registry 失败污染 docker 健康: %d", v)
				}
			}
			if n := len(f.gw.sched.Candidates("registry:" + host)); n != 0 {
				t.Errorf("失败源应进入 %s 冷却，候选=%d", host, n)
			}
		})
	}
}

func mustHealth(t *testing.T, st *store.Store, id int64) map[string]int {
	t.Helper()
	h, err := st.HealthOf(id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
