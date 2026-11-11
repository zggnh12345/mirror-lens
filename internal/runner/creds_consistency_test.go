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

	mu     sync.Mutex
	creds  map[string]string // "user:pass" -> 标签
	tokens map[string]string // token -> 标签
	log    []credReq
	seq    int

	jobTag     string // 当前任务标签，仅用于日志归属
	holdAt     int    // 0 = 不设闸门；否则在第 N 次 token 请求上挂起
	tokenCalls int
	fired      bool
	armed      chan struct{}
	release    chan struct{}
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

// arm 让下一次 token 请求在签发后挂起；返回的 armed 通道在挂起时关闭。
func (c *credRegistry) arm(at int) (armed, release chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holdAt = at
	c.tokenCalls = 0
	c.fired = false
	c.armed = make(chan struct{})
	c.release = make(chan struct{})
	return c.armed, c.release
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
	h := r.Header.Get("Authorization")
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
		Backoff:     time.Second,
		Realms:      map[string][]string{credTestHost: {f.reg.srv.URL}},
		Now:         func() time.Time { return time.Unix(f.now, 0) },
	})
}

// run 在后台跑一次真实 runOne（快照 → Probe → CAS 提交），返回完成通道。
func (f *credFixture) run(r *HostProbeRunner) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runOne(context.Background(), f.job)
	}()
	return done
}

type sourceRow struct {
	cap     int
	path    string
	probeAt int64
	errText string
}

func (f *credFixture) source() sourceRow {
	var s sourceRow
	var mode sql.NullString
	err := f.db.QueryRow(`SELECT registry_cap, registry_path_mode, registry_probe_at, registry_probe_error FROM sources WHERE id=?`, f.srcID).
		Scan(&s.cap, &mode, &s.probeAt, &s.errText)
	if err != nil {
		panic(err)
	}
	s.path = mode.String
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
}

func mustOneIdentity(t *testing.T, recs []credReq, want string) {
	t.Helper()
	got := identities(recs)
	if len(got) != 1 || got[want] == 0 {
		t.Fatalf("期望全部认证请求使用 %s，实际 %v（记录 %+v）", want, got, recs)
	}
}

// ---- 场景 1：只用 A，不更新 ----

func TestProbeCredentialScenario1Stable(t *testing.T) {
	reg := newCredRegistry(t)
	f := newCredFixture(t, reg)
	if _, err := f.creds.Put(credTestHost, "userA", "passA"); err != nil {
		t.Fatal(err)
	}
	f.now = 1000
	n := reg.mark()
	reg.setJob("S1-A")
	<-f.run(f.runner())

	recs := reg.since(n)
	mustOneIdentity(t, recs, "A")
	if row := f.source(); row.cap != store.RegistryCapYes || row.probeAt != 1000 {
		t.Fatalf("应提交 Committed（cap=1, probe_at=1000），实际 %+v", row)
	}
	if hasChallenge(recs) == 0 {
		t.Fatal("应有未认证的 /v2/ challenge 记录，便于区分认证前后")
	}
	dumpLog(t, "S1", recs)
	dumpRow(t, "S1", f.source())
}

// ---- 场景 2：更新为 B 后启动 ----

func TestProbeCredentialScenario2NewSnapshot(t *testing.T) {
	reg := newCredRegistry(t)
	f := newCredFixture(t, reg)
	if _, err := f.creds.Put(credTestHost, "userA", "passA"); err != nil {
		t.Fatal(err)
	}
	f.now = 1000
	reg.setJob("S2-preA")
	<-f.run(f.runner())
	if _, err := f.creds.Put(credTestHost, "userB", "passB"); err != nil {
		t.Fatal(err)
	}

	f.now = 2000
	n := reg.mark()
	reg.setJob("S2-B")
	<-f.run(f.runner())
	recs := reg.since(n)
	mustOneIdentity(t, recs, "B")
	if row := f.source(); row.cap != store.RegistryCapYes || row.probeAt != 2000 {
		t.Fatalf("B 代次应提交 Committed（cap=1, probe_at=2000），实际 %+v", row)
	}
	dumpLog(t, "S2", recs)
	dumpRow(t, "S2", f.source())
}

// ---- 场景 3：任务执行中 A→B ----

func TestProbeCredentialScenario3UpdateMidFlight(t *testing.T) {
	reg := newCredRegistry(t)
	f := newCredFixture(t, reg)
	if _, err := f.creds.Put(credTestHost, "userA", "passA"); err != nil {
		t.Fatal(err)
	}
	f.seedDiagnostics(t, 500, "上一次诊断")

	// 第二个 token 请求 = native 路径的 manifest：此时已用 A 换到 token。
	armed, release := reg.arm(2)
	n := reg.mark()
	f.now = 3000
	reg.setJob("S3-A")
	done := f.run(f.runner())
	<-armed

	if _, err := f.creds.Put(credTestHost, "userB", "passB"); err != nil {
		t.Fatal(err)
	}
	baseline := f.source()    // 凭据变更完成后的状态，才是 stale 的比对基准
	afterUpdate := reg.mark() // 变更完成之后的请求必须仍然来自 A
	close(release)
	<-done

	recs := reg.since(n)
	mustOneIdentity(t, recs, "A") // 任务中途不得改读 B
	trailing := reg.since(afterUpdate)
	if len(trailing) == 0 {
		t.Fatal("变更后旧任务没有继续发请求，无法证明「不换凭据」")
	}
	mustOneIdentity(t, trailing, "A")
	if n := countKind(trailing, "manifest"); n == 0 {
		t.Fatal("变更后应至少完成一次 manifest 请求，否则不能证明任务带着 A 走完")
	}
	if after := f.source(); after != baseline {
		t.Fatalf("stale 提交覆盖了状态: 变更后 %+v，任务结束后 %+v", baseline, after)
	}
	if baseline.cap != store.RegistryCapUnknown || baseline.path != "" || baseline.probeAt != 500 || baseline.errText != "上一次诊断" {
		t.Fatalf("凭据变更应清能力、留诊断，实际 %+v", baseline)
	}
	if got := f.healthRows(); got != 0 {
		t.Fatalf("stale 任务不应写健康样本，实际 %d 条", got)
	}
	dumpLog(t, "S3", recs)
	dumpRow(t, "S3 baseline", baseline)
	dumpRow(t, "S3 after", f.source())
}

// ---- 场景 4：任务执行中删除凭据 ----

func TestProbeCredentialScenario4DeleteMidFlight(t *testing.T) {
	reg := newCredRegistry(t)
	f := newCredFixture(t, reg)
	if _, err := f.creds.Put(credTestHost, "userA", "passA"); err != nil {
		t.Fatal(err)
	}
	f.seedDiagnostics(t, 500, "上一次诊断")

	armed, release := reg.arm(2)
	n := reg.mark()
	f.now = 4000
	reg.setJob("S4-A")
	done := f.run(f.runner())
	<-armed

	if err := f.creds.Delete(credTestHost); err != nil {
		t.Fatal(err)
	}
	baseline := f.source()
	afterUpdate := reg.mark()
	close(release)
	<-done

	recs := reg.since(n)
	mustOneIdentity(t, recs, "A") // 手里已有 A 快照，允许走完，但不能改读别的代次
	if trailing := reg.since(afterUpdate); len(trailing) == 0 {
		t.Fatal("删除后旧任务没有继续发请求，无法证明「不换凭据」")
	} else {
		mustOneIdentity(t, trailing, "A")
	}
	if after := f.source(); after != baseline {
		t.Fatalf("删除后的 stale 提交覆盖了状态: 变更后 %+v，任务结束后 %+v", baseline, after)
	}

	// 新任务不得再取得 A：快照为空，只能匿名，令牌端点拒绝。
	n2 := reg.mark()
	f.now = 5000
	reg.setJob("S4-new")
	<-f.run(f.runner())
	recs2 := reg.since(n2)
	if got := identities(recs2); len(got) != 0 {
		t.Fatalf("删除后新任务不应再使用任何凭据，实际 %v", got)
	}
	if !hasAnonToken(recs2) {
		t.Fatalf("新任务应以匿名身份申请 token 并被拒，实际 %+v", recs2)
	}
	if got := f.healthRows(); got != 0 {
		t.Fatalf("探测任务不应写健康样本，实际 %d 条", got)
	}
	dumpLog(t, "S4", recs)
	dumpLog(t, "S4-new", recs2)
	dumpRow(t, "S4 baseline", baseline)
	dumpRow(t, "S4 after", f.source())
}

// dumpLog 把上游看到的请求逐条打印出来，作为验收证据（-test.v 可见）。
func dumpLog(t *testing.T, title string, recs []credReq) {
	t.Helper()
	for _, r := range recs {
		t.Logf("%s | job=%-6s %-9s id=%-7s %3d %-60s scope=%s", title, r.Job, r.Kind, r.Identity, r.Status, r.Path, r.Scope)
	}
}

func dumpRow(t *testing.T, title string, row sourceRow) {
	t.Helper()
	t.Logf("%s | DB cap=%d path=%q probe_at=%d err=%q", title, row.cap, row.path, row.probeAt, row.errText)
}

func countKind(recs []credReq, kind string) int {
	n := 0
	for _, r := range recs {
		if r.Kind == kind && r.Status == 200 {
			n++
		}
	}
	return n
}

func hasChallenge(recs []credReq) int {
	n := 0
	for _, r := range recs {
		if r.Kind == "challenge" {
			n++
		}
	}
	return n
}

func hasAnonToken(recs []credReq) bool {
	for _, r := range recs {
		if r.Kind == "token" && r.Identity == "anon" {
			return true
		}
	}
	return false
}
