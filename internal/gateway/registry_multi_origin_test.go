package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	// officialStatus/officialHdr 让 HEAD 用例控制官方源的状态与挑战头；
	// 默认 0 表示照旧返回 200 清单。
	officialStatus int
	officialHdr    http.Header
	// native / hostPrefix 分别记录各自命中的路径；official 记录官方回源次数。
	nativeHits   atomic.Int32
	prefixHits   atomic.Int32
	officialHits atomic.Int32
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
	f := &moFixture{}
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
		w.Write([]byte(officialManifest))
	}))
	t.Cleanup(official.Close)

	host := strings.TrimPrefix(official.URL, "http://")
	ad := registryadapter.New(host, 4<<30, 5*time.Minute)
	ad.Scheme = "http"
	ad.MultiOrigin = true

	f.gw = New(st, Options{
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
		if strings.EqualFold(r.URL.Host, "quay.io") {
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
	if err := f.st.SetRepoHost(id, "quay.io"); err != nil {
		t.Fatalf("repo_host: %v", err)
	}
	gen, dig, err := f.st.HostGenState("quay.io")
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if res, err := f.st.SetHostCaps(id, "quay.io", 1, mode, time.Now().Unix(), "", gen, dig); err != nil || res != store.HostCapsCommitted {
		t.Fatalf("SetHostCaps: %v %v", res, err)
	}
	return id
}

func (f *moFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://gw"+path, nil)
	r.SetBasicAuth("x", f.token)
	w := httptest.NewRecorder()
	f.gw.ServeRegistry(w, r)
	return w
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

func mustHealth(t *testing.T, st *store.Store, id int64) map[string]int {
	t.Helper()
	h, err := st.HealthOf(id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
