package runner

// 本文件验证「Host 探测任务级凭据一致性」的四条验收场景：
//
//  1. 只用 A、不更新           → 全部认证请求 A，提交 Committed
//  2. A 更新为 B 后启动任务     → 全部认证请求 B，提交 Committed
//  3. A 任务执行中更新为 B      → 旧任务继续用 A，提交 StaleSnapshot；
//                              诊断/probe_at/能力不被旧任务覆盖，不产生健康样本
//  4. A 任务执行中删除凭据      → 旧任务用已有快照 A 走完（提交 stale）；
//                              新任务不再取得 A
//
// 一致性单位是「一次 Host 探测任务」，不是单个 HTTP 请求，更不是 Docker Pull
// 事务：网关拉取路径目前不读 secret.Store，没有凭据维度，本文件不作该承诺。
//
// 时序全部靠闸门同步：token 端点校验完 Basic 并签发 token 后挂起，测试侧完成
// 凭据变更再放行，没有 sleep 猜测。身份记录取自 token 签发时的凭据标签，所以
// 「同一个任务的请求混用 A/B」会直接暴露在日志里。
//
// 本文件禁止 t.Parallel()：newCredFixture 会改 probe.Configure 这个包级全局。
// 所有闸门等待都经 awaitGate 限时，超时打印场景、闸门序号和当前请求日志，
// 不能让整个包挂到 -timeout 才失败；arm 同时注册 Cleanup 兜底放行，
// 测试中途 Fatal 也不会把 httptest 的 handler 永久留在 <-release 上。
//
// 默认用本地伪造的 OCI manifest；设置 ML_CREDTEST_UPSTREAM=http://native:5000
// 后可把 manifest 请求代理到真实 registry:3（需在 ml-e2e 网络里运行测试二进制）。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/secret"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth/v2authtest"
)

const credTestHost = "quay.io"

// credReq 是上游看到的一次请求。Identity 来自 token 签发时的凭据标签，
// 因此 Bearer 请求也能追溯它是用哪一代凭据换来的 token。
type credReq struct {
	Job      string // 任务标签：把请求归属到某次 runOne
	Kind     string // challenge / root / manifest / token
	Path     string
	Identity string // A / B / anon / unknown
	Status   int
	Scope    string
}

// credRegistry 是一个最小 registry + Bearer 认证端点。
// 它同时接受 A、B 两套凭据：这样「中途换读」会成功而不是被拒绝，
// 混用才会暴露出来，而不是被 401 掩盖。
type credRegistry struct {
	srv      *httptest.Server
	upstream string // 非空：manifest 请求代理到真实 registry

	// allowAnon 让匿名请求也能拿到 manifest（用于「探测期间变更凭据」，
	// 此时上游本身是公开可读的；仍然不允许携带任何凭据）。
	allowAnon bool

	mu     sync.Mutex
	creds  map[string]string // "user:pass" -> 标签
	tokens map[string]string // token -> 标签
	log    []credReq
	seq    int

	jobTag     string // 当前任务标签，仅用于日志归属
	holdAt     int    // 0 = 不设闸门；否则在第 N 次 token 请求上挂起
	tokenCalls int
	fired      bool

	// manifest 闸门：挂住第一个 manifest 请求，供「探测期间变更凭据」用。
	holdManifestAt int
	manifestCalls  int
	mArmed         chan struct{}
	mRelease       chan struct{}
	mOnce          sync.Once
	armed          chan struct{}
	release        chan struct{}
	relOnce        sync.Once
}

// gateTimeout 是一次闸门等待的上限：超时说明装配和被测时序脱节
// （例如探测顺序变了），必须马上失败并给出诊断，而不是挂到 -timeout。
const gateTimeout = 10 * time.Second

// awaitGate 限时等待闸门。snapshot 为 nil 时只报闸门名。
func awaitGate(t *testing.T, scene, gate string, ch <-chan struct{}, snapshot func() string) {
	t.Helper()
	select {
	case <-ch:
		return
	case <-time.After(gateTimeout):
		state := ""
		if snapshot != nil {
			state = "；" + snapshot()
		}
		t.Fatalf("%s 等待 %s 超时（%s）%s", scene, gate, gateTimeout, state)
	}
}

func newCredRegistry(t *testing.T) *credRegistry {
	t.Helper()
	c := &credRegistry{
		upstream: os.Getenv("ML_CREDTEST_UPSTREAM"),
		creds:    map[string]string{"userA:passA": "A", "userB:passB": "B"},
		tokens:   map[string]string{},
	}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	return c
}

// arm 让第 at 次 token 请求在签发后挂起；返回的 armed 通道在挂起时关闭。
// 放行一律走 releaseGate：测试侧和 t.Cleanup 都调它，sync.Once 保证只关一次，
// 中途 Fatal 也不会把 handler 永久留在 <-release 上。
func (c *credRegistry) arm(t *testing.T, at int) (armed chan struct{}) {
	t.Helper()
	c.mu.Lock()
	c.holdAt = at
	c.tokenCalls = 0
	c.fired = false
	c.armed = make(chan struct{})
	c.release = make(chan struct{})
	armed = c.armed
	c.mu.Unlock()
	t.Cleanup(c.releaseGate)
	return armed
}

// holdManifest 让第 1 个 manifest 请求挂起；返回 armed（已挂起）与 release。
func (c *credRegistry) holdManifest() (armed, release chan struct{}) {
	c.mu.Lock()
	c.holdManifestAt = 1
	c.manifestCalls = 0
	c.mArmed = make(chan struct{})
	c.mRelease = make(chan struct{})
	armed, release = c.mArmed, c.mRelease
	c.mu.Unlock()
	return armed, release
}

func (c *credRegistry) releaseGate() {
	c.mu.Lock()
	rel := c.release
	c.mu.Unlock()
	if rel == nil {
		return
	}
	c.relOnce.Do(func() { close(rel) })
}

// state 供超时诊断：闸门到哪一步、最近三条上游请求。
func (c *credRegistry) state() string {
	c.mu.Lock()
	fired, calls, hold := c.fired, c.tokenCalls, c.holdAt
	c.mu.Unlock()
	tail := c.since(0)
	if len(tail) > 3 {
		tail = tail[len(tail)-3:]
	}
	return fmt.Sprintf("闸门 fired=%v holdAt=%d tokenCalls=%d；最近请求=%v", fired, hold, calls, tail)
}

func (c *credRegistry) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token":
		c.serveToken(w, r)
	case strings.HasPrefix(r.URL.Path, "/v2/"):
		c.serveRegistry(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (c *credRegistry) serveToken(w http.ResponseWriter, r *http.Request) {
	user, pass, hasBasic := r.BasicAuth()
	ident := "anon"
	if hasBasic {
		ident = "unknown"
		if label, ok := c.creds[user+":"+pass]; ok {
			ident = label
		}
	}

	c.mu.Lock()
	c.seq++
	tok := fmt.Sprintf("t%d-%s", c.seq, ident)
	c.tokens[tok] = ident
	hold := false
	if c.holdAt > 0 {
		c.tokenCalls++
		if c.tokenCalls == c.holdAt && !c.fired {
			c.fired = true
			hold = true
		}
	}
	armed, release := c.armed, c.release
	c.mu.Unlock()

	if hold {
		close(armed)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
	}

	status := http.StatusOK
	if ident == "anon" || ident == "unknown" {
		status = http.StatusUnauthorized
	}
	c.record(credReq{Kind: "token", Path: r.URL.Path, Identity: ident, Status: status, Scope: r.URL.Query().Get("scope")})

	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
}

func (c *credRegistry) serveRegistry(w http.ResponseWriter, r *http.Request) {
	kind := "root"
	if strings.Contains(r.URL.Path, "/manifests/") {
		kind = "manifest"
	}
	c.mu.Lock()
	hold := c.holdManifestAt > 0 && kind == "manifest"
	var armed, release chan struct{}
	if hold {
		c.manifestCalls++
		if c.manifestCalls == c.holdManifestAt {
			armed, release = c.mArmed, c.mRelease
		}
	}
	c.mu.Unlock()
	if armed != nil {
		close(armed)
		<-release
	}

	h := r.Header.Get("Authorization")
	if c.allowAnon && h == "" {
		if kind == "root" {
			c.record(credReq{Kind: "root", Path: r.URL.Path, Identity: "anon", Status: http.StatusOK})
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			_, _ = w.Write([]byte("{}"))
			return
		}
		// 落到下面统一的 manifest 分支（allowAnon 只跳过认证挑战）。
		body, ctype, status := c.manifest(r)
		c.record(credReq{Kind: kind, Path: r.URL.Path, Identity: "anon", Status: status})
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Docker-Content-Digest", "sha256:"+sumHex(body))
		_, _ = w.Write(body)
		return
	}
	if !strings.HasPrefix(h, "Bearer ") {
		c.record(credReq{Kind: "challenge", Path: r.URL.Path, Identity: "-", Status: http.StatusUnauthorized})
		w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="e2e"`, c.srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	c.mu.Lock()
	ident, ok := c.tokens[tok]
	c.mu.Unlock()
	if !ok {
		c.record(credReq{Kind: kind, Path: r.URL.Path, Identity: "bogus", Status: http.StatusUnauthorized})
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, ctype, status := c.manifest(r)
	c.record(credReq{Kind: kind, Path: r.URL.Path, Identity: ident, Status: status})
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Docker-Content-Digest", "sha256:"+sumHex(body))
	_, _ = w.Write(body)
}

// manifest 返回 manifest 正文：优先代理真实 registry，否则给一份自洽的 OCI manifest。
func (c *credRegistry) manifest(r *http.Request) ([]byte, string, int) {
	if c.upstream != "" {
		path := r.URL.Path
		// host_prefix 路径要把 "/v2/<host>/" 还原成 "/v2/"。
		path = strings.Replace(path, "/v2/"+credTestHost+"/", "/v2/", 1)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(c.upstream, "/")+path, nil)
		if err != nil {
			return nil, "", http.StatusBadGateway
		}
		req.Header.Set("Accept", r.Header.Get("Accept"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, "", http.StatusBadGateway
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		ctype := resp.Header.Get("Content-Type")
		if ctype == "" {
			ctype = "application/vnd.oci.image.manifest.v1+json"
		}
		return b, ctype, resp.StatusCode
	}
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json",` +
		`"digest":"sha256:` + strings.Repeat("0", 64) + `","size":2},"layers":[]}`)
	return body, "application/vnd.oci.image.manifest.v1+json", http.StatusOK
}

func (c *credRegistry) setJob(tag string) {
	c.mu.Lock()
	c.jobTag = tag
	c.mu.Unlock()
}

func (c *credRegistry) record(r credReq) {
	c.mu.Lock()
	r.Job = c.jobTag
	c.log = append(c.log, r)
	c.mu.Unlock()
}

// since 返回从第 n 条之后的新记录，用来按任务切片。
func (c *credRegistry) since(n int) []credReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]credReq, len(c.log)-n)
	copy(out, c.log[n:])
	return out
}

func (c *credRegistry) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.log)
}

// identities 统计「真正带着凭据标签的请求」用了哪些身份。
// challenge（未认证的 401）与 anon 不计入，但要单独断言存在。
func identities(recs []credReq) map[string]int {
	out := map[string]int{}
	for _, r := range recs {
		if r.Kind == "challenge" || r.Identity == "-" || r.Identity == "anon" || r.Identity == "bogus" {
			continue
		}
		out[r.Identity]++
	}
	return out
}

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---- 测试装配 ----

type credFixture struct {
	st    *store.Store
	creds *secret.Store
	db    *sql.DB
	srcID int64
	job   HostProbeJob
	reg   *credRegistry
	now   int64
}

func newCredFixture(t *testing.T, reg *credRegistry) *credFixture {
	t.Helper()
	dir := t.TempDir()
	probe.Configure(true, "v4") // 测试端点在本机回环，生产默认仍拒绝内网
	t.Cleanup(func() { probe.Configure(false) })

	st, err := store.Open(filepath.Join(dir, "cred.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	id, err := st.AddSource(store.Source{Name: "mirror", BaseURL: reg.srv.URL, Status: "usable", RepoHost: credTestHost})
	if err != nil {
		t.Fatal(err)
	}
	key, err := secret.LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := secret.NewCredStore(filepath.Join(dir, "registry_creds.enc"), key, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "cred.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	return &credFixture{
		st: st, creds: cs, db: raw, srcID: id, reg: reg, now: 1000,
		job: HostProbeJob{SourceID: id, Host: credTestHost, BaseURL: reg.srv.URL, Repo: "prometheus/node-exporter", Tag: "v3"},
	}
}

func (f *credFixture) runner() *HostProbeRunner {
	return NewHostProbeRunner(HostProbeOptions{
		Store:       f.st,
		Creds:       f.creds,
		Repos:       map[string]string{credTestHost: "prometheus/node-exporter"},
		Concurrency: 1,
		Timeout:     5 * time.Second,
		Policies:    v2authtest.New(map[string][]string{credTestHost: {f.reg.srv.URL}}, false),
		Now:         func() time.Time { return time.Unix(f.now, 0) },
	})
}

// run 在后台跑一次真实 runOne（快照 → Probe → CAS 提交），返回完成通道。
func (f *credFixture) run(r *HostProbeRunner) <-chan struct{} {
	return f.runCtx(context.Background(), r)
}

func (f *credFixture) runCtx(ctx context.Context, r *HostProbeRunner) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runOne(ctx, f.job)
	}()
	return done
}

// wait 限时等任务退出：超时说明 runOne 不响应取消，或闸门没放行。
func (f *credFixture) wait(t *testing.T, scene string, done <-chan struct{}) {
	t.Helper()
	awaitGate(t, scene, "任务退出(done)", done, f.reg.state)
}

type sourceRow struct {
	cap     int
	path    string
	probeAt int64
	errText string
}

func (f *credFixture) source() sourceRow {
	var s sourceRow
	var status string
	var mode sql.NullString
	err := f.db.QueryRow(`SELECT effective_status, registry_path_mode FROM source_host_caps
		WHERE source_id=? AND host=?`, f.srcID, credTestHost).Scan(&status, &mode)
	if err != nil {
		panic(err)
	}
	s.cap = store.CapForStatusForTest(status)
	s.path = mode.String
	err = f.db.QueryRow(`SELECT COALESCE(r.last_probe_at,0), COALESCE(c.diagnostics,'')
		FROM source_host_caps c LEFT JOIN registry_host_recovery r
		  ON r.source_id=c.source_id AND r.family='registry' AND r.scope=c.host
		WHERE c.source_id=? AND c.host=?`, f.srcID, credTestHost).
		Scan(&s.probeAt, &s.errText)
	if err != nil {
		panic(err)
	}
	return s
}

func (f *credFixture) healthRows() int {
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM source_health WHERE source_id=?`, f.srcID).Scan(&n); err != nil {
		panic(err)
	}
	return n
}

// seedDiagnostics 造一条「上一次有效探测」留下的诊断，供 stale 断言使用。
func (f *credFixture) seedDiagnostics(t *testing.T, at int64, note string) {
	t.Helper()
	gen, digest, err := f.st.HostGenState(credTestHost)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.st.SetHostCaps(f.srcID, credTestHost, store.RegistryCapUnknown, "", at, note, gen, digest)
	if err != nil || res != store.HostCapsCommitted {
		t.Fatalf("预置诊断失败: %v %v", res, err)
	}
	if _, err := f.db.Exec(`UPDATE source_host_caps SET diagnostics=? WHERE source_id=? AND host=?`,
		note, f.srcID, credTestHost); err != nil {
		t.Fatalf("预置诊断失败: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE registry_host_recovery SET last_probe_at=?
		WHERE source_id=? AND family='registry' AND scope=?`, at, f.srcID, credTestHost); err != nil {
		t.Fatalf("预置 recovery 诊断失败: %v", err)
	}
}

func mustOneIdentity(t *testing.T, recs []credReq, want string) {
	t.Helper()
	got := identities(recs)
	if len(got) != 1 || got[want] == 0 {
		t.Fatalf("期望全部认证请求使用 %s，实际 %v（记录 %+v）", want, got, recs)
	}
}

// ---- 3.5.15-C.2 新契约：候选探测完全匿名 ----

// 候选 Source 探测不得读取或传递官方凭据：上游只能看到无认证的
// challenge，探测结果按 auth_error/Inconclusive 记账，不标 Unsupported。
func TestProbeCandidateNeverUsesOfficialCredentials(t *testing.T) {
	reg := newCredRegistry(t)
	f := newCredFixture(t, reg)
	if _, err := f.creds.PutCredential(credTestHost,
		secret.Credential{Mode: string(secret.ModeBearerTokenBasic), Username: "userA", Password: "passA"}, nil); err != nil {
		t.Fatal(err)
	}
	mark := reg.mark()
	f.wait(t, "匿名", f.run(f.runner()))

	recs := reg.since(mark)
	if got := identities(recs); len(got) != 0 {
		t.Fatalf("候选探测不得携带任何凭据，实际 %v（记录 %+v）", got, recs)
	}
	// 401 归为 auth_error / Inconclusive，不是 Unsupported。
	src := f.source()
	if src.cap != store.RegistryCapUnknown {
		t.Fatalf("匿名 401 应记 Unknown/Inconclusive，实际 cap=%d", src.cap)
	}
	if f.healthRows() != 0 {
		t.Fatal("认证失败不得产生健康样本")
	}
}

// 凭据在探测期间变更：提交按旧 gen CAS 失败，诊断/探测时间不被覆盖。
func TestProbeCandidateCredentialChangeMidFlight(t *testing.T) {
	reg := newCredRegistry(t)
	f := newCredFixture(t, reg)
	if _, err := f.creds.PutCredential(credTestHost,
		secret.Credential{Mode: string(secret.ModeBearerTokenBasic), Username: "userA", Password: "passA"}, nil); err != nil {
		t.Fatal(err)
	}
	f.seedDiagnostics(t, 900, "prev-probe")

	reg.allowAnon = true
	armed, release := reg.holdManifest()
	done := f.run(f.runner())
	awaitGate(t, "变更中", "manifest 挂起", armed, reg.state)
	if _, err := f.creds.PutCredential(credTestHost,
		secret.Credential{Mode: string(secret.ModeBearerTokenBasic), Username: "userB", Password: "passB"}, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	f.wait(t, "变更中", done)

	src := f.source()
	if src.probeAt != 900 {
		t.Fatalf("旧 gen 的探测不得覆盖 last_probe_at，实际 %d", src.probeAt)
	}
	if src.errText != "prev-probe" {
		t.Fatalf("旧 gen 的探测不得覆盖诊断，实际 %q", src.errText)
	}
}

// 凭据在探测期间被删除：同样按 stale 丢弃，不写回任何结论。
func TestProbeCandidateCredentialDeleteMidFlight(t *testing.T) {
	reg := newCredRegistry(t)
	f := newCredFixture(t, reg)
	if _, err := f.creds.PutCredential(credTestHost,
		secret.Credential{Mode: string(secret.ModeBearerTokenBasic), Username: "userA", Password: "passA"}, nil); err != nil {
		t.Fatal(err)
	}
	f.seedDiagnostics(t, 901, "prev-delete")

	reg.allowAnon = true
	armed, release := reg.holdManifest()
	done := f.run(f.runner())
	awaitGate(t, "删除中", "manifest 挂起", armed, reg.state)
	if _, err := f.creds.DeleteCredential(credTestHost, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	f.wait(t, "删除中", done)

	src := f.source()
	if src.probeAt != 901 || src.errText != "prev-delete" {
		t.Fatalf("删除期间的结果不得写回: probeAt=%d err=%q", src.probeAt, src.errText)
	}
}

// 任务取消必须及时退出（不因为认证/负缓存挂住）。
func TestProbeCredentialCancelStopsTask(t *testing.T) {
	reg := newCredRegistry(t)
	f := newCredFixture(t, reg)
	reg.allowAnon = true
	armed, release := reg.holdManifest()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := f.runCtx(ctx, f.runner())
	awaitGate(t, "取消", "manifest 挂起", armed, reg.state)
	cancel()
	f.wait(t, "取消", done)
}
