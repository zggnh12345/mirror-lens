package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

// 3.5.16 验收矩阵（T01–T50 / A01–A10）的代码测试入口。
//
// 本文件只证明代码路径；真实进程与公共源证据在验收记录里单列，
// 不能把这里的全绿当成 REAL_PASS。

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type raceFixture struct {
	*moFixture
}

func newRaceFixture(t *testing.T, host string) *raceFixture {
	t.Helper()
	f := newMOFixture(t)
	f.host = host
	f.ad.MultiOriginHosts = []string{host}
	// 竞速启动延迟压到 10ms：测试不该为耐心预算等一秒。
	f.gw.race.After = 10 * time.Millisecond
	return &raceFixture{f}
}

func (f *raceFixture) star(t *testing.T, id int64, on bool) {
	t.Helper()
	v := on
	if err := f.st.UpdateSource(id, nil, nil, nil, &v); err != nil {
		t.Fatalf("星标: %v", err)
	}
	f.gw.sched.Invalidate()
}

func (f *raceFixture) switchHost(t *testing.T, host string, on bool) {
	t.Helper()
	if err := f.st.SetStarRace(host, on); err != nil {
		t.Fatalf("Host 开关: %v", err)
	}
	f.gw.InvalidateStarRace()
}

func (f *raceFixture) mirror(t *testing.T, body string, delay time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	m := &moMirror{path: "/v2/foo/bar/manifests/" + digestOf(body), body: body, hits: &hits, delay: delay}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func (f *raceFixture) getDigest(t *testing.T, digest string) *httptest.ResponseRecorder {
	t.Helper()
	return f.get(t, "/v2/"+f.host+"/foo/bar/manifests/"+digest)
}

func (f *raceFixture) head(t *testing.T, digest string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodHead, "http://gw/v2/"+f.host+"/foo/bar/manifests/"+digest, nil)
	r.SetBasicAuth("x", f.token)
	w := httptest.NewRecorder()
	f.gw.ServeRegistry(w, r)
	return w
}

func (f *raceFixture) getTag(t *testing.T, tag string) *httptest.ResponseRecorder {
	t.Helper()
	return f.get(t, "/v2/"+f.host+"/foo/bar/manifests/"+tag)
}

// cacheKey 用适配器自己算缓存键：测试不另写一套键算法（否则断言的是假键）。
func (f *raceFixture) cacheKey(t *testing.T, digest string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://gw/v2/"+f.host+"/foo/bar/manifests/"+digest, nil)
	plan, err := f.ad.Plan(r)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan.CacheKey
}

// scrapeRaceMetrics 返回 /metrics 全文，用于断言三类并发机制可区分（T45–T47）。
func scrapeRaceMetrics(t *testing.T) string {
	t.Helper()
	h := metrics.Get()
	if h == nil {
		t.Fatal("metrics 未启用")
	}
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

func counterValue(t *testing.T, text, line string) float64 {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, line) {
			continue
		}
		fs := strings.Fields(l)
		if len(fs) != 2 {
			return 0
		}
		var v float64
		for _, c := range fs[1] {
			if c < '0' || c > '9' {
				return -1
			}
			v = v*10 + float64(c-'0')
		}
		return v
	}
	return 0
}

func boolPtr(b bool) *bool { return &b }

// T01/T02：全新配置与旧配置升级都不自动开启竞速、不给任何 Source 加星。
func TestRaceDefaultsOff(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	if hosts := f.gw.StarRaceHosts(); len(hosts) != 0 {
		t.Fatalf("默认应无 Host 开竞速，实得 %v", hosts)
	}
	if f.gw.starRaceOn("quay.io") {
		t.Fatal("默认 star_race 必须关闭")
	}
	srv, _ := f.mirror(t, mirrorManifest, 0)
	id := f.addRegistrySource(t, srv, "native")
	src, err := f.st.GetSource(id)
	if err != nil || src.Preferred {
		t.Fatalf("新建 Source 不应被自动星标: %v %v", src, err)
	}
}

// T03：Host 关闭、Source 已星标 → 不竞速。
func TestRaceHostSwitchOff(t *testing.T) {
	metrics.Enable()
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	srvA, hitsA := f.mirror(t, body, 50*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 50*time.Millisecond)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.officialBody = []byte(body)

	if w := f.getDigest(t, digestOf(body)); w.Code != 200 {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if hitsA.Load()+hitsB.Load() != 1 {
		t.Fatalf("Host 开关关闭时不应并行打两源：A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
	if got := counterValue(t, scrapeRaceMetrics(t), "mirrorlens_registry_race_started_total{"); got != 0 {
		t.Fatalf("Host 关闭不应形成竞速，started=%v", got)
	}
}

// T04：Host 开启但 Source 未星标 → 不竞速。
func TestRaceSourceNotStarred(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	srvA, hitsA := f.mirror(t, body, 50*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 50*time.Millisecond)
	f.addRegistrySource(t, srvA, "native")
	f.addRegistrySource(t, srvB, "native")
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	if w := f.getDigest(t, digestOf(body)); w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if hitsA.Load()+hitsB.Load() != 1 {
		t.Fatalf("未星标不应竞速：A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
}

// T05/T11/A01：Host 开 + 两个星标源在该 Host 能力合格 + 不可变 Digest → 双路竞速。
func TestRaceSameHostDigest(t *testing.T) {
	metrics.Enable()
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	srvA, hitsA := f.mirror(t, body, 200*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	w := f.getDigest(t, digestOf(body))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != body {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-ML-Source") == "" {
		t.Fatal("竞速命中必须带 X-ML-Source")
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("应各打一次：A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
	text := scrapeRaceMetrics(t)
	if got := counterValue(t, text, `mirrorlens_registry_race_started_total{layer="same_host",host="quay.io"}`); got != 1 {
		t.Fatalf("registry_race_started_total=%v want 1\n%s", got, text)
	}
	if got := counterValue(t, text, `mirrorlens_registry_race_wins_total{rank="2"}`); got != 1 {
		t.Fatalf("第二候选应先胜出，wins{rank=2}=%v", got)
	}
	if got := counterValue(t, text, "mirrorlens_hedge_triggered_total"); got != 0 {
		t.Fatalf("竞速不得计入通用 hedge 指标，hedge_triggered=%v", got)
	}
}

// T43：可变 Tag 一律不进竞速。
func TestRaceTagNotRaced(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	delay := 60 * time.Millisecond
	var hitsA atomic.Int32
	srvA := httptest.NewServer(&moMirror{
		path: "/v2/foo/bar/manifests/latest", body: body, hits: &hitsA, delay: delay,
	})
	defer srvA.Close()
	idA := f.addRegistrySource(t, srvA, "native")
	f.star(t, idA, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	if w := f.getTag(t, "latest"); w.Code != 200 {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 {
		t.Fatalf("tag 请求只应顺序访问一次，实得 %d", hitsA.Load())
	}
}

// T12/T13/T14：客户端 HEAD 始终顺序执行，且不形成 Registry 竞速。
func TestRaceHeadSequential(t *testing.T) {
	metrics.Enable()
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 0)
	srvB, hitsB := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	if w := f.head(t, digest); w.Code != 200 {
		t.Fatalf("HEAD status=%d body=%q", w.Code, w.Body.String())
	}
	if hitsA.Load()+hitsB.Load() != 1 {
		t.Fatalf("HEAD 必须顺序执行：A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
	if got := counterValue(t, scrapeRaceMetrics(t), "mirrorlens_registry_race_started_total{"); got != 0 {
		t.Fatalf("HEAD 不得形成竞速，started=%v", got)
	}
}

// T17：启用态 GET 竞速进行中，HEAD 到达 → HEAD 不加入 GET Flight。
func TestRaceHeadNotJoinRaceFlight(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 400*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 400*time.Millisecond)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)
	// HEAD 走顺序路径时只会在第一候选上等到 400ms；把耐心预算拉长，
	// 让 GET 的竞速仍在进行中。
	f.gw.hedge.After = 2 * time.Second

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.getDigest(t, digest)
	}()
	time.Sleep(40 * time.Millisecond)
	start := time.Now()
	if w := f.head(t, digest); w.Code != 200 {
		t.Fatalf("HEAD status=%d", w.Code)
	}
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Fatalf("HEAD 不应等到 GET 竞速结束（耗时 %v）", d)
	}
	wg.Wait()
	if hitsA.Load()+hitsB.Load() < 2 {
		t.Fatalf("HEAD 与 GET 必须各自回源（hits=%d+%d）", hitsA.Load(), hitsB.Load())
	}
}

// T19：两个同策略 GET 合并成一次回源。
func TestRaceSamePolicyCoalesce(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srv, hits := f.mirror(t, body, 40*time.Millisecond)
	id := f.addRegistrySource(t, srv, "native")
	f.star(t, id, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.getDigest(t, digest)
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("同策略 GET 应合并成一次回源，实得 %d", hits.Load())
	}
}

// T23b：只有一个合格镜像源时顺序回源，不与官方源竞速。
func TestRaceNeedsTwoMirrorSources(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srv, hits := f.mirror(t, body, 30*time.Millisecond)
	id := f.addRegistrySource(t, srv, "native")
	f.star(t, id, true)
	f.switchHost(t, "quay.io", true)

	if w := f.getDigest(t, digest); w.Code != 200 {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("只有一个镜像源时只应打它一次，实得 %d", hits.Load())
	}
	if f.officialHits.Load() != 0 {
		t.Fatalf("单镜像源成功时不该回官方，official=%d", f.officialHits.Load())
	}
}

// T23d/T32：三个合格源时最多只竞速两个。
func TestRaceAtMostTwoCandidates(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 120*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 120*time.Millisecond)
	srvC, hitsC := f.mirror(t, body, 120*time.Millisecond)
	ids := []int64{
		f.addRegistrySource(t, srvA, "native"),
		f.addRegistrySource(t, srvB, "native"),
		f.addRegistrySource(t, srvC, "native"),
	}
	for _, id := range ids {
		f.star(t, id, true)
	}
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	if w := f.getDigest(t, digest); w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if total := hitsA.Load() + hitsB.Load() + hitsC.Load(); total > 2 {
		t.Fatalf("实际启动超过两个竞速候选：%d", total)
	}
}

// T23e/T06：第二候选被禁用即失去资格，不用官方源补位。
func TestRaceNoOfficialBackfill(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, _ := f.mirror(t, body, 20*time.Millisecond)
	idA := f.addRegistrySource(t, srvA, "native")
	f.star(t, idA, true)
	srvB, hitsB := f.mirror(t, body, 0)
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idB, true)
	if err := f.st.UpdateSource(idB, boolPtr(false), nil, nil, nil); err != nil {
		t.Fatalf("禁用: %v", err)
	}
	f.gw.sched.Invalidate()
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	if w := f.getDigest(t, digest); w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if hitsB.Load() != 0 {
		t.Fatalf("被禁用的源不应被请求：%d", hitsB.Load())
	}
	if f.officialHits.Load() != 0 {
		t.Fatalf("官方源不应补位参与竞速：official=%d", f.officialHits.Load())
	}
}

// A03：一个 Host 的开关不影响另一个 Host 的资格（Host 级隔离）。
func TestRaceHostIsolation(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	f.switchHost(t, "gcr.io", true)
	if f.gw.starRaceOn("quay.io") {
		t.Fatal("开 gcr.io 不得连带打开 quay.io")
	}
	if !f.gw.starRaceOn("gcr.io") {
		t.Fatal("gcr.io 应已开启")
	}
	if !f.gw.starRaceOn("registry-1.docker.io") == false {
		t.Fatal("docker hub 不应受影响")
	}
}

// T24：第一候选快速失败时第二候选按策略启动。
func TestRaceSecondCandidateStart(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	var hitsFail atomic.Int32
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsFail.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	srvB, hitsB := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, failSrv, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	w := f.getDigest(t, digest)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != body {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if hitsFail.Load() == 0 || hitsB.Load() == 0 {
		t.Fatalf("第一路 500 后第二路必须启动：fail=%d ok=%d", hitsFail.Load(), hitsB.Load())
	}
}

// T26/T27/T28：胜者正文截断 / Digest 不符 / 长度不符都不提交缓存，并顺序回退。
func TestRaceWinnerBodyFailureFallsBack(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		length string
	}{
		{name: "截断", body: `{"schemaVersion":2,"half":`, length: "512"},
		{name: "digest 不符", body: `{"schemaVersion":2,"tampered":true}`},
		{name: "长度不符", body: `{"schemaVersion":2,"short":1}`, length: "9999"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			metrics.Enable()
			f := newRaceFixture(t, "quay.io")
			good := mirrorManifest
			digest := digestOf(good)
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.length != "" {
					w.Header().Set("Content-Length", c.length)
				}
				w.Header().Set("Content-Type", manifestsAccept)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(c.body))
			}))
			defer bad.Close()
			idBad := f.addRegistrySource(t, bad, "native")
			// 第二个候选很慢：坏源先交响应头，正文校验失败后才顺序回退。
			srvSlow, _ := f.mirror(t, good, 400*time.Millisecond)
			idSlow := f.addRegistrySource(t, srvSlow, "native")
			f.star(t, idBad, true)
			f.star(t, idSlow, true)
			f.switchHost(t, "quay.io", true)
			f.officialBody = []byte(good)

			w := f.getDigest(t, digest)
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != good {
				t.Fatalf("应回退到可用源，status=%d body=%q", w.Code, w.Body.String())
			}
			e, _ := f.st.CacheGet(f.cacheKey(t, digest))
			if e == nil {
				t.Fatal("回退成功后应提交缓存")
			}
			if e.SHA256 != strings.TrimPrefix(digest, "sha256:") {
				t.Fatalf("缓存内容不是请求的 Digest：%s", e.SHA256)
			}
			if got := counterValue(t, scrapeRaceMetrics(t), "mirrorlens_registry_race_body_failures_total"); got < 1 {
				t.Fatalf("应记一次正文校验失败，实得 %v", got)
			}
		})
	}
}

// T31：第二候选预算不足时不启动第二路（第一路成功分支）。
//
// 预算是「每源」的：先占满第二候选自己那一份，让它在启动前拿不到名额。
func TestRaceSecondCandidateBudget(t *testing.T) {
	metrics.Enable()
	f := newRaceFixture(t, "quay.io")
	f.gw.sched = scheduler.New(f.st, scheduler.Config{Enabled: true, MaxCandidates: 3, MaxPerSource: 1})
	f.gw.sched.Invalidate()
	body := mirrorManifest
	digest := digestOf(body)
	// 第一路故意慢：否则它在竞速计时器到期前就赢了，第二路根本没机会申请名额。
	srvA, hitsA := f.mirror(t, body, 150*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	// 模拟另一条在途请求占满第二候选的唯一名额。
	held, err := f.gw.sched.TryAcquireLease(context.Background(), idB, scheduler.KindNormal)
	if err != nil || held == nil {
		t.Fatalf("预占名额失败: %v %v", held, err)
	}
	defer held.Release(held.Token())

	w := f.getDigest(t, digest)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if hitsA.Load() != 1 {
		t.Fatalf("第一候选应只请求一次：%d", hitsA.Load())
	}
	if hitsB.Load() != 0 {
		t.Fatalf("预算不足时不应启动第二路：%d", hitsB.Load())
	}
	if got := counterValue(t, scrapeRaceMetrics(t), "mirrorlens_registry_race_budget_skipped_total"); got < 1 {
		t.Fatalf("应记一次预算不足，实得 %v", got)
	}
}

// T33：缓存命中不进入竞速。
func TestRaceCacheHitNoRace(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 0)
	srvB, hitsB := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	f.getDigest(t, digest)
	base := hitsA.Load() + hitsB.Load()
	if w := f.getDigest(t, digest); w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if hitsA.Load()+hitsB.Load() != base {
		t.Fatal("缓存命中不应再回源")
	}
}

// T36：调度器关闭时不竞速（也不命中镜像源）。
func TestRaceSchedulerOff(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	f.gw.sched = scheduler.New(f.st, scheduler.Config{Enabled: false})
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	f.star(t, idA, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	if w := f.getDigest(t, digest); w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if hitsA.Load() != 0 {
		t.Fatalf("调度器关闭时不应命中镜像源：%d", hitsA.Load())
	}
}

// T34：hasPrev（重验证）分支不竞速。
//
// 不可变 Digest 的条目永远算新鲜（plan.Immutable），所以「重验证」只发生在
// 可变 Tag 上；这里直接断言准入判定：hasPrev=true 时不允许竞速。
func TestRaceRevalidationNoRace(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 0)
	srvB, _ := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	r := httptest.NewRequest(http.MethodGet, "http://gw/v2/"+f.host+"/foo/bar/manifests/"+digest, nil)
	plan, err := f.ad.Plan(r)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	f.gw.evaluateRegistryRace(plan, http.MethodGet, false)
	if !plan.RaceReady {
		t.Fatal("前置条件：无既有条目时应具备竞速资格")
	}
	plan2, _ := f.ad.Plan(r)
	f.gw.evaluateRegistryRace(plan2, http.MethodGet, true)
	if plan2.RaceReady || plan2.FlightPolicy != "" {
		t.Fatalf("重验证分支不得竞速：ready=%v policy=%q", plan2.RaceReady, plan2.FlightPolicy)
	}
	if hitsA.Load() != 0 {
		t.Fatalf("准入判定不应产生任何上游请求：%d", hitsA.Load())
	}
}

// A05/A08：竞速请求结束后所有 Lease 都归还，没有残留 attempt。
func TestRaceReleasesLeases(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, _ := f.mirror(t, body, 150*time.Millisecond)
	srvB, _ := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	if w := f.getDigest(t, digest); w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.gw.sched.LeaseHeld(idA) == 0 && f.gw.sched.LeaseHeld(idB) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("竞速结束后名额未归还：A=%d B=%d", f.gw.sched.LeaseHeld(idA), f.gw.sched.LeaseHeld(idB))
}

// A07：主链有 fallback 时仍能竞速；备用链不叠加对冲；总请求数不超过 3。
func TestRaceChainSemantics(t *testing.T) {
	metrics.Enable()
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 200*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	cands := f.gw.sched.CandidatesN("registry:quay.io", 4)
	if len(cands) < 2 {
		t.Fatalf("候选不足：%d", len(cands))
	}
	mk := func(srv *httptest.Server, c *scheduler.Candidate) fetchTarget {
		u, _ := url.Parse(srv.URL + "/v2/foo/bar/manifests/" + digest)
		if c == nil {
			return fetchTarget{u: u}
		}
		cc := *c
		return fetchTarget{u: u, c: &cc}
	}
	main := []fetchTarget{mk(srvA, &cands[0]), mk(srvB, &cands[1])}
	var fbHits atomic.Int32
	fbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fbHits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fbSrv.Close()
	fb := []fetchTarget{mk(fbSrv, nil)}

	offURL, _ := url.Parse("http://" + f.host + "/v2/foo/bar/manifests/" + digest)
	plan := &adapter.Plan{
		Kind: "docker", Registry: true, ResourceKind: adapter.ResourceManifest,
		SchedKey: "registry:quay.io", LogicalHost: "quay.io",
		Upstream: offURL, Method: http.MethodGet, CacheKey: "race-chain-a07",
		Immutable: true, ExpectSHA: strings.TrimPrefix(digest, "sha256:"),
		Fallbacks: []adapter.Fallback{{URL: offURL, Kind: "docker"}},
		RaceReady: true,
	}
	e, _, err := f.gw.fetchRace(context.Background(), plan, main, fb)
	if err != nil || e == nil {
		t.Fatalf("fetchRace: %v", err)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("主链应形成双路竞速：A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
	if fbHits.Load() > 1 {
		t.Fatalf("备用链不得叠加对冲：%d", fbHits.Load())
	}
}

// T20/T21/T22：HEAD 顺序 Flight 与 GET 竞速 Flight 并发写同一 CacheKey 后缓存一致。
func TestRaceConcurrentFlightsSameCacheKey(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, _ := f.mirror(t, body, 0)
	srvB, _ := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				f.getDigest(t, digest)
				return
			}
			f.head(t, digest)
		}(i)
	}
	wg.Wait()

	e, err := f.st.CacheGet(f.cacheKey(t, digest))
	if err != nil || e == nil {
		t.Fatalf("并发提交后应有缓存条目: %v %v", e, err)
	}
	if !f.gw.cache.Has(e.SHA256) {
		t.Fatal("索引指向的 blob 必须存在")
	}
}

// T16：非 Manifest 的 Registry 请求（Blob）不进入 Manifest 竞速。
func TestRaceBlobPathUntouched(t *testing.T) {
	metrics.Enable()
	f := newRaceFixture(t, "quay.io")
	blob := []byte("layer-bytes")
	sum := sha256.Sum256(blob)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var hits atomic.Int32
	srv := httptest.NewServer(&moMirror{
		path: "/v2/foo/bar/blobs/" + digest, body: string(blob), hits: &hits,
	})
	defer srv.Close()
	id := f.addRegistrySource(t, srv, "native")
	f.star(t, id, true)
	f.switchHost(t, "quay.io", true)

	w := f.get(t, "/v2/"+f.host+"/foo/bar/blobs/"+digest)
	if w.Code != 200 || w.Body.String() != string(blob) {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := counterValue(t, scrapeRaceMetrics(t), "mirrorlens_registry_race_started_total{"); got != 0 {
		t.Fatalf("Blob 不得进入 Manifest 竞速，started=%v", got)
	}
}

// T18：HEAD 顺序执行中 GET 到达 → GET 不加入 HEAD 的 Flight，自己按竞速走。
func TestRaceGetNotJoinHeadFlight(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, hitsA := f.mirror(t, body, 300*time.Millisecond)
	srvB, hitsB := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.head(t, digest)
	}()
	// 让 HEAD 先进顺序 Flight（它会打第一个候选，约 300ms）。
	time.Sleep(30 * time.Millisecond)
	start := time.Now()
	if w := f.getDigest(t, digest); w.Code != 200 {
		t.Fatalf("GET status=%d", w.Code)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("GET 不应等 HEAD 结束（耗时 %v）", d)
	}
	wg.Wait()
	if hitsA.Load()+hitsB.Load() < 3 {
		t.Fatalf("HEAD 一次 + GET 竞速两次，期望 ≥3 次回源，实得 %d", hitsA.Load()+hitsB.Load())
	}
}

// T20：两个同策略 HEAD 合并成一次回源。
func TestRaceHeadCoalesce(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srv, hits := f.mirror(t, body, 60*time.Millisecond)
	id := f.addRegistrySource(t, srv, "native")
	f.star(t, id, true)
	f.switchHost(t, "quay.io", true)
	f.officialBody = []byte(body)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.head(t, digest)
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("同策略 HEAD 应合并成一次回源，实得 %d", hits.Load())
	}
}

// T35：OfficialOnly（私有认证等保护路径）不竞速。
func TestRaceOfficialOnlyNoRace(t *testing.T) {
	f := newRaceFixture(t, "quay.io")
	body := mirrorManifest
	digest := digestOf(body)
	srvA, _ := f.mirror(t, body, 0)
	srvB, _ := f.mirror(t, body, 0)
	idA := f.addRegistrySource(t, srvA, "native")
	idB := f.addRegistrySource(t, srvB, "native")
	f.star(t, idA, true)
	f.star(t, idB, true)
	f.switchHost(t, "quay.io", true)

	r := httptest.NewRequest(http.MethodGet, "http://gw/v2/"+f.host+"/foo/bar/manifests/"+digest, nil)
	plan, err := f.ad.Plan(r)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plan.OfficialOnly = true
	f.gw.evaluateRegistryRace(plan, http.MethodGet, false)
	if plan.RaceReady || plan.FlightPolicy != "" {
		t.Fatalf("OfficialOnly 不得竞速：ready=%v policy=%q", plan.RaceReady, plan.FlightPolicy)
	}
}
