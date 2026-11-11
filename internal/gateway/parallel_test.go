package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
)

// rangeSrv 是一台支持 Range 的假源，可按需制造各种「不对劲」。
type rangeSrv struct {
	body      []byte
	totalBias int64             // 报的总长偏移，模拟不同对象
	corruptAt int64             // 从该偏移起返回垃圾，模拟「长度对、内容错」
	noRange   bool              // 忽略 Range，直接 200
	failFrom  int64             // 该偏移之后的请求返回 500
	delay     time.Duration     // 每个请求先等一会儿，用来观察协程是否真的退了
	status    int               // 非 0 时直接回这个状态码，用来试预检分类
	cr        string            // 非空时用它顶替 Content-Range（"-" 表示干脆不发）
	hdr       map[string]string // 额外响应头，用来造 429 的 Retry-After
	hits      int32
	cur       int32 // 此刻正在写分片正文的请求数
	maxCur    int32 // 上面那个数的历史峰值
	mu        sync.Mutex
	ranges    []string
}

func (s *rangeSrv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&s.hits, 1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-r.Context().Done():
			return
		}
	}
	rh := r.Header.Get("Range")
	s.mu.Lock()
	s.ranges = append(s.ranges, rh)
	s.mu.Unlock()

	for k, v := range s.hdr {
		w.Header().Set(k, v)
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	if s.noRange {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
		w.Write(s.body)
		return
	}
	var from, to int64
	if _, err := fmt.Sscanf(rh, "bytes=%d-%d", &from, &to); err != nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
		w.Write(s.body)
		return
	}
	if s.failFrom > 0 && from >= s.failFrom {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if from >= int64(len(s.body)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if to >= int64(len(s.body)) {
		to = int64(len(s.body)) - 1
	}
	chunk := append([]byte(nil), s.body[from:to+1]...)
	if s.corruptAt > 0 && to >= s.corruptAt {
		for i := int64(0); i < int64(len(chunk)); i++ {
			if from+i >= s.corruptAt {
				chunk[i] = 'X'
			}
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	switch s.cr {
	case "":
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, int64(len(s.body))+s.totalBias))
	case "-":
		// 故意不发
	default:
		w.Header().Set("Content-Range", s.cr)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.WriteHeader(http.StatusPartialContent)
	// 只统计「正文不止 1 字节」的请求：预检那一次 1 字节的探路不算并发。
	if len(chunk) > 1 {
		cur := atomic.AddInt32(&s.cur, 1)
		for {
			max := atomic.LoadInt32(&s.maxCur)
			if cur <= max || atomic.CompareAndSwapInt32(&s.maxCur, max, cur) {
				break
			}
		}
		defer atomic.AddInt32(&s.cur, -1)
		// 正文写入本身太快，不在这里停一下，计数器几乎看不到重叠。
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
	}
	w.Write(chunk)
}

func parFixture(t *testing.T, size int) (*Gateway, *store.Store, *cache.Store, []byte, *rangeSrv, *rangeSrv, *httptest.Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}
	g := New(st, Options{Cache: cs, Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})
	g.par = ParOptions{Enabled: true, MinSize: 1 << 20, ChunkSize: 2 << 20, Workers: 4}

	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*7 + 3)
	}
	origin := &rangeSrv{body: body}
	mirror := &rangeSrv{body: body}
	os := httptest.NewServer(origin)
	ms := httptest.NewServer(mirror)
	t.Cleanup(os.Close)
	t.Cleanup(ms.Close)
	return g, st, cs, body, origin, mirror, os, ms
}

func parPlan(t *testing.T, origin *httptest.Server, body []byte) (*adapter.Plan, *scheduler.Candidate) {
	t.Helper()
	u, err := url.Parse(origin.URL + "/v2/library/x/blobs/sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return &adapter.Plan{
		Kind:      "docker",
		CacheKey:  "docker:blob:abc",
		Upstream:  u,
		Stream:    true,
		Cacheable: true,
		TTL:       time.Hour,
		ExpectSHA: hex.EncodeToString(sum[:]),
	}, &scheduler.Candidate{ID: 1, Name: "镜像A", BaseURL: "http://mirror"}
}

func parRun(g *Gateway, plan *adapter.Plan) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://gw/v2/library/x/blobs/sha256:abc", nil)
	rec := httptest.NewRecorder()
	if !g.tryParallel(rec, req, plan) {
		rec.Code = -1 // 标记「没有接管」
	}
	return rec
}

// failWriter 第一次写就报错，模拟客户端读到一半断开。
type failWriter struct{ h http.Header }

func (f *failWriter) Header() http.Header       { return f.h }
func (f *failWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (f *failWriter) WriteHeader(int)           {}

// 客户端断开时必须等所有分片协程退出再收尾：否则协程还在往 .part 里写，
// 文件已经被删掉或重建，轻则日志刷屏、重则写坏下一次请求的数据。
func TestParallelWaitsForChunksOnClientGone(t *testing.T) {
	g, _, _, body, _, _, os_, ms := parFixture(t, 9<<20)
	originSrv := os_.Config.Handler.(*rangeSrv)
	mirrorSrv := ms.Config.Handler.(*rangeSrv)
	originSrv.delay = 40 * time.Millisecond
	mirrorSrv.delay = 40 * time.Millisecond
	plan, cand := parPlan(t, os_, body)
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}

	w := &failWriter{h: http.Header{}}
	req := httptest.NewRequest(http.MethodGet, "http://gw/v2/library/x/blobs/sha256:abc", nil)
	if !g.tryParallel(w, req, plan) {
		t.Fatal("应接管这次请求")
	}

	total := func() int32 {
		return atomic.LoadInt32(&originSrv.hits) + atomic.LoadInt32(&mirrorSrv.hits)
	}

	// cancel 之前已经发出去的请求，服务端 handler 可能被排到返回之后才跑，
	// 那是「在途」而不是「泄漏」。先等它落地，再看还会不会继续涨。
	prev := total()
	deadline := time.Now().Add(3 * time.Second)
	for {
		time.Sleep(50 * time.Millisecond)
		cur := total()
		if cur == prev {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("返回后仍在不停请求上游：%d → %d", prev, cur)
		}
		prev = cur
	}

	// 稳定之后必须一直稳定：还有协程活着的话，它会继续发下一段。
	time.Sleep(200 * time.Millisecond)
	if after := total(); after != prev {
		t.Fatalf("返回后仍有协程在请求上游：%d → %d", prev, after)
	}
}

func TestParallelAssemblesInOrder(t *testing.T) {
	g, st, cs, body, origin, mirror, os, ms := parFixture(t, 9<<20)
	plan, cand := parPlan(t, os, body)
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	if rec.Code != http.StatusOK {
		t.Fatalf("应接管并返回 200，得到 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("拼出来的内容不对：%d 字节 vs %d", rec.Body.Len(), len(body))
	}
	if rec.Header().Get("X-ML-Parallel") == "" {
		t.Fatalf("应带 X-ML-Parallel 头")
	}
	e, err := st.CacheGet("docker:blob:abc")
	if err != nil || e == nil || e.Verify != store.VerifyVerified {
		t.Fatalf("应落一条 verified 缓存，得到 %+v err=%v", e, err)
	}
	if !cs.Has(plan.ExpectSHA) {
		t.Fatalf("blob 应已发布")
	}
	if _, err := cs.Open(plan.ExpectSHA); err != nil {
		t.Fatalf("blob 应能打开: %v", err)
	}
	if _, ok := cs.ReadPartial("docker:blob:abc"); ok {
		t.Fatalf(".part 应已清理")
	}
	// 镜像源至少下一段；官方只做预检和兜底，不再抢首段（首段卡在
	// 官方直连上会让整条流慢到几十 KB/s）。
	if atomic.LoadInt32(&mirror.hits) == 0 {
		t.Fatalf("镜像没被用上")
	}
	if atomic.LoadInt32(&origin.hits) < 1 {
		t.Fatalf("官方至少应参与预检，实际 %d 次", origin.hits)
	}
	if g.parFills.Load() != 1 {
		t.Fatalf("并行计数应为 1，得到 %d", g.parFills.Load())
	}
}

// 星标源不支持 Range 时不能硬凑并行：宁可单流用星标源。
func TestParallelYieldsToPreferredSource(t *testing.T) {
	g, _, _, body, _, mirror, os, ms := parFixture(t, 9<<20)
	mirror.noRange = true
	plan, cand := parPlan(t, os, body)
	cand.Preferred = true
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	if rec.Code != -1 {
		t.Fatalf("星标源不支持分片时应回落单流，得到 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("不应写出任何字节")
	}
}

// 星标源掉速时，其余源合计明显更快就该改走并行；星标源够快时保持单流。
func TestStarTooSlowAndCooldown(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.AddSource(store.Source{
		Name: "p", BaseURL: "https://p.example", Status: "usable", Enabled: true,
		DockerCap: 1, GithubCap: 1, GitCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{st: st, degradeAt: map[int64]time.Time{}, probeAt: map[int64]time.Time{}}
	plan := &adapter.Plan{Kind: "docker"}
	c := &scheduler.Candidate{ID: id}

	if g.starTooSlow(plan, c) {
		t.Fatal("没有真实样本时不该判慢")
	}
	for i := 0; i < 3; i++ {
		// 100MiB / 50s = 2MB/s
		if err := st.AddProxySpeed(id, "docker", 100<<20, 50000); err != nil {
			t.Fatal(err)
		}
	}
	if !g.starTooSlow(plan, c) {
		t.Fatal("最近 3 次都是 2MB/s，应判慢")
	}
	if !g.allowParallelNow(id) {
		t.Fatal("判慢后应允许并行")
	}
	if !g.allowParallelNow(id) {
		t.Fatal("冷却期内应继续保持并行")
	}
	g.degradeMu.Lock()
	g.degradeAt[id] = time.Now().Add(-40 * time.Minute)
	g.degradeMu.Unlock()
	if g.allowParallelNow(id) {
		t.Fatal("冷却到期应放一次单流试探")
	}
	if !g.allowParallelNow(id) {
		t.Fatal("试探后 30 分钟内应回到并行")
	}
	time.Sleep(1100 * time.Millisecond)                                   // 让新样本的 ts 严格更新
	if err := st.AddProxySpeed(id, "docker", 100<<20, 5000); err != nil { // 20MB/s
		t.Fatal(err)
	}
	if g.starTooSlow(plan, c) {
		t.Fatal("最近一次 20MB/s 就不该再判慢")
	}
}

// 已知不支持 Range 的源在 TTL 内不该再被预检：否则每次 pull 都让它整层传一遍。
func TestProbeSkipsKnownRangeOff(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := &Gateway{par: ParOptions{RangeTTL: time.Hour}, cli: srv.Client()}
	c := &scheduler.Candidate{ID: 1, RangeCap: 0, RangeAt: time.Now().Unix()}
	res := g.probeTarget(context.Background(), &adapter.Plan{}, fetchTarget{u: mustURL(t, srv.URL), c: c})
	if res.ok {
		t.Fatal("不该把已知不支持 Range 的源算作可参与分片")
	}
	if hits != 0 {
		t.Fatalf("TTL 内应跳过预检，实际发了 %d 次请求", hits)
	}

	// 过期后必须重测一次，不能永久钉死。
	g.par.RangeTTL = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	g.probeTarget(context.Background(), &adapter.Plan{}, fetchTarget{u: mustURL(t, srv.URL), c: c})
	if hits != 1 {
		t.Fatalf("过期后应重探一次，实际 %d 次", hits)
	}
}

func TestParallelRequiresDigest(t *testing.T) {
	g, _, _, body, _, _, os, ms := parFixture(t, 4<<20)
	plan, cand := parPlan(t, os, body)
	plan.ExpectSHA = ""
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	if rec.Code != -1 {
		t.Fatalf("没有摘要不应接管，得到 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("不应写出任何字节")
	}
}

func TestParallelFallsBackWithoutRange(t *testing.T) {
	g, st, _, body, origin, mirror, os, ms := parFixture(t, 4<<20)
	origin.noRange = true
	mirror.noRange = true
	plan, cand := parPlan(t, os, body)
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))
	id, err := st.AddSource(store.Source{Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	if rec.Code != -1 {
		t.Fatalf("不支持 Range 应退回单流，得到 %d", rec.Code)
	}
	if g.parFalls.Load() != 1 {
		t.Fatalf("应记一次退回，得到 %d", g.parFalls.Load())
	}
	// 不支持 Range 只是「不能分片」，源本身没坏：不能降权。
	if okN, _, failN, _, _ := st.SourceEvents(id, 10); okN != 0 || failN != 0 {
		t.Fatalf("能力不匹配不该记源故障，实际 ok=%d fail=%d", okN, failN)
	}
}

func TestParallelRejectsWrongContent(t *testing.T) {
	g, st, cs, body, origin, mirror, os, ms := parFixture(t, 9<<20)
	plan, cand := parPlan(t, os, body)
	// 第二段起全错：长度对、内容错。两台源都坏，否则分片落到好源上就不是「拼出错数据」了。
	origin.corruptAt = 3 << 20
	mirror.corruptAt = 3 << 20
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))
	id, err := st.AddSource(store.Source{Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应头已写出，应为 200，得到 %d", rec.Code)
	}
	if e, _ := st.CacheGet("docker:blob:abc"); e != nil {
		t.Fatalf("校验不过的内容绝不能落索引")
	}
	if g.VerifyFailed() != 1 {
		t.Fatalf("应记一次拒收，得到 %d", g.VerifyFailed())
	}
	if cs.Has(plan.ExpectSHA) {
		t.Fatalf("校验不过的内容绝不能发布成 blob")
	}
	if _, ok := cs.ReadPartial("docker:blob:abc"); ok {
		t.Fatalf(".part 应已丢弃")
	}
	// 两台源拼出来的东西是错的，但分不清是谁撒谎：不能把两台都记失败，
	// 否则正常源跟着被熔断。
	if okN, _, failN, _, _ := st.SourceEvents(cand.ID, 10); okN != 0 || failN != 0 {
		t.Fatalf("无法归因时不该动源评分，实际 ok=%d fail=%d", okN, failN)
	}
}

func TestParallelRetriesOriginOnChunkError(t *testing.T) {
	g, st, _, body, origin, mirror, os, ms := parFixture(t, 9<<20)
	plan, cand := parPlan(t, os, body)
	mirror.failFrom = 4 << 20 // 第 3 段开始镜像全 500
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))
	id, err := st.AddSource(store.Source{Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	if rec.Code != http.StatusOK {
		t.Fatalf("应接管，得到 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("回官方重取后内容应正确")
	}
	if e, err := st.CacheGet("docker:blob:abc"); err != nil || e == nil || e.Verify != store.VerifyVerified {
		t.Fatalf("应落 verified 缓存，得到 %+v err=%v", e, err)
	}
	_ = origin
	// 镜像供了第 0 段、后两段全 500 由官方源补上：它必须被记成失败，
	// 不能因为「整份最终成功」就记成功——否则坏源永远熔不掉。
	okN, _, failN, _, err := st.SourceEvents(cand.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if okN != 0 || failN != 1 {
		t.Fatalf("应给镜像记 1 次失败、0 次成功，实际 ok=%d fail=%d", okN, failN)
	}
}

func TestParallelExcludesMismatchedTotal(t *testing.T) {
	g, _, _, body, _, mirror, os, ms := parFixture(t, 4<<20)
	plan, cand := parPlan(t, os, body)
	mirror.totalBias = 1 // 报的总长不一样 = 不是同一个对象
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	if rec.Code != -1 {
		t.Fatalf("只剩一台源时应退回单流，得到 %d", rec.Code)
	}
}

func TestParallelStatsExposed(t *testing.T) {
	g, _, _, body, _, _, os, ms := parFixture(t, 9<<20)
	plan, cand := parPlan(t, os, body)
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	parRun(g, plan)
	s := g.ParallelStats()
	if s["fills"] != 1 || s["bytes"] != int64(len(body)) {
		t.Fatalf("统计不对: %v", s)
	}
	// 面板看到的是同一条链路：任务结束后必须落在「最近完成」里，字节数要对得上。
	snap := g.ActiveTasks()
	if len(snap.Active) != 0 {
		t.Fatalf("结束后不该还在活跃表: %+v", snap.Active)
	}
	if len(snap.Recent) != 1 || snap.Recent[0].Mode != "parallel" || snap.Recent[0].Total != int64(len(body)) {
		t.Fatalf("最近完成不对: %+v", snap.Recent)
	}
	if snap.WindowBytes < int64(len(body)) {
		t.Fatalf("吞吐窗口应记到这次下载的字节，得到 %d", snap.WindowBytes)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 确认流式读取不会被 ResponseRecorder 的写入顺序搞乱：客户端拿到的必须是原文件。
func TestParallelBodyDigestMatches(t *testing.T) {
	g, _, _, body, _, _, os, ms := parFixture(t, 6<<20)
	plan, cand := parPlan(t, os, body)
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	sum := sha256.Sum256(rec.Body.Bytes())
	if hex.EncodeToString(sum[:]) != plan.ExpectSHA {
		t.Fatalf("客户端收到的字节与摘要不符")
	}
	_ = io.Discard
	_ = strings.TrimSpace("")
}

// 分片被「别的分片失败」连带取消时，不能把取消记成这个源故障。
func TestParChunkIgnoresCancellation(t *testing.T) {
	g, st, _, body, _, _, osrv, _ := parFixture(t, 4<<20)
	plan, cand := parPlan(t, osrv, body)
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))
	id, err := st.AddSource(store.Source{Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id

	part, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟 abort() 已经取消

	stats := make([]parStat, 1)
	src := parSource{t: fetchTarget{u: plan.Upstream, c: cand}}
	_ = g.parChunk(ctx, plan, src, []parSource{src}, 0, 0, 1<<20, int64(len(body)), part, stats, nil)
	if stats[0].fail != 0 || stats[0].ok != 0 {
		t.Fatalf("取消不该计入源评分，实际 ok=%d fail=%d", stats[0].ok, stats[0].fail)
	}
}

func TestPreflightStatusIsSourceFailure(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, false},
		{http.StatusPartialContent, false},
		{http.StatusMovedPermanently, false},
		{http.StatusFound, false},
		{http.StatusTemporaryRedirect, false},
		{http.StatusPermanentRedirect, false},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusMethodNotAllowed, false},
		{http.StatusRequestTimeout, true},
		{http.StatusConflict, false},
		{http.StatusGone, false},
		{http.StatusRequestedRangeNotSatisfiable, false},
		{http.StatusMisdirectedRequest, false},
		{http.StatusTooEarly, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusNotImplemented, false},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusInsufficientStorage, true},
		{http.StatusVariantAlsoNegotiates, false},
		{http.StatusLoopDetected, false},
		{http.StatusNotExtended, false},
		{http.StatusHTTPVersionNotSupported, false},
		{http.StatusNetworkAuthenticationRequired, false},
		{499, false}, // 非标准，落不进任何一类
		{600, false}, // 越界
		{0, false},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			if got := preflightStatusIsSourceFailure(tt.status); got != tt.want {
				t.Fatalf("status=%d: want=%v got=%v", tt.status, tt.want, got)
			}
		})
	}
}

// 预检把状态码分成「源故障」和「请求/能力不匹配」，只有前者能进 source_events。
func TestParallelPreflightStatusClassification(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantFail int
	}{
		{"range unsupported", http.StatusOK, 0},
		{"unauthorized", http.StatusUnauthorized, 0},
		{"forbidden", http.StatusForbidden, 0},
		{"not found", http.StatusNotFound, 0},
		{"range unsatisfied", http.StatusRequestedRangeNotSatisfiable, 0},
		{"method not allowed", http.StatusMethodNotAllowed, 0},
		{"request timeout", http.StatusRequestTimeout, 1},
		{"too early", http.StatusTooEarly, 0},
		{"rate limited", http.StatusTooManyRequests, 1},
		{"internal error", http.StatusInternalServerError, 1},
		{"bad gateway", http.StatusBadGateway, 1},
		{"service unavailable", http.StatusServiceUnavailable, 1},
		{"gateway timeout", http.StatusGatewayTimeout, 1},
		{"not implemented", http.StatusNotImplemented, 0},
		{"variant also negotiates", http.StatusVariantAlsoNegotiates, 0},
		{"loop detected", http.StatusLoopDetected, 0},
		{"not extended", http.StatusNotExtended, 0},
		{"http version unsupported", http.StatusHTTPVersionNotSupported, 0},
		{"network auth required", http.StatusNetworkAuthenticationRequired, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, st, _, body, _, mirror, os, ms := parFixture(t, 4<<20)
			plan, cand := parPlan(t, os, body)
			mirror.status = tt.status
			g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))
			id, err := st.AddSource(store.Source{Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1})
			if err != nil {
				t.Fatal(err)
			}
			cand.ID = id
			g.parTargets = func(*adapter.Plan) []fetchTarget {
				return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
			}
			if rec := parRun(g, plan); rec.Code != -1 {
				t.Fatalf("预检没通过时应退回单流，得到 %d", rec.Code)
			}
			okN, _, failN, _, err := st.SourceEvents(id, 10)
			if err != nil {
				t.Fatal(err)
			}
			if okN != 0 || failN != tt.wantFail {
				t.Fatalf("status=%d: want ok=0 fail=%d, got ok=%d fail=%d", tt.status, tt.wantFail, okN, failN)
			}
		})
	}
}

// 206 但 Content-Range 不合法是协议响应异常，算源故障；总长不一致只是版本滞后，不算。
func TestParallelPreflightContentRange(t *testing.T) {
	tests := []struct {
		name     string
		cr       string
		bias     int64
		wantFail int
	}{
		{"missing header", "-", 0, 1},
		{"malformed", "bytes oops", 0, 1},
		{"wrong start", "bytes 5-5/4194304", 0, 1},
		{"zero total", "bytes 0-0/0", 0, 1},
		{"length differs", "", 1024, 0},
		{"valid", "", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, st, _, body, _, mirror, os, ms := parFixture(t, 4<<20)
			plan, cand := parPlan(t, os, body)
			mirror.cr = tt.cr
			mirror.totalBias = tt.bias
			g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))
			id, err := st.AddSource(store.Source{Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1})
			if err != nil {
				t.Fatal(err)
			}
			cand.ID = id
			g.parTargets = func(*adapter.Plan) []fetchTarget {
				return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
			}
			parRun(g, plan)
			okN, _, failN, _, err := st.SourceEvents(id, 10)
			if err != nil {
				t.Fatal(err)
			}
			if failN != tt.wantFail {
				t.Fatalf("%s: want fail=%d, got ok=%d fail=%d", tt.name, tt.wantFail, okN, failN)
			}
		})
	}
}

// 分到的那台源整段失败时，先把这一段换到另一台参与分片的镜像；
// 只有所有镜像都不行才回官方源。否则一台坏源就能把整份拖回慢路。
func TestParChunkFailsOverToOtherMirror(t *testing.T) {
	g, st, _, body, origin, mirror, os_, ms := parFixture(t, 9<<20)
	plan, cand := parPlan(t, os_, body)
	mirror.failFrom = 1 // 这台只肯给第 0 字节，正文全 500

	other := &rangeSrv{body: body, delay: 2 * time.Millisecond}
	os2 := httptest.NewServer(other)
	t.Cleanup(os2.Close)

	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))
	id, err := st.AddSource(store.Source{Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id
	id2, err := st.AddSource(store.Source{Name: "镜像B", BaseURL: os2.URL, Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	cand2 := &scheduler.Candidate{ID: id2, Name: "镜像B", BaseURL: os2.URL}
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{
			{u: mustURL(t, ms.URL), c: cand},
			{u: mustURL(t, os2.URL), c: cand2},
			{u: plan.Upstream},
		}
	}
	rec := parRun(g, plan)
	if rec.Code != http.StatusOK {
		t.Fatalf("应接管，得到 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("换源补分片后内容应正确")
	}
	// 预检只打镜像，官方源一次都不该被 Range 请求。
	// 镜像A 只肯给第 0 字节，凡是它接到的正经分片都会 500。动态派活之后
	// 具体分到哪几段不固定，所以按「A 实际请求过的段」来对账：这些段
	// 必须由镜像B 补上，且一段都不许落到官方源。
	mirror.mu.Lock()
	var failedRanges []string
	for _, rg := range mirror.ranges {
		var from int64
		if _, err := fmt.Sscanf(rg, "bytes=%d-", &from); err == nil && from >= 1 {
			failedRanges = append(failedRanges, rg)
		}
	}
	mirror.mu.Unlock()
	if len(failedRanges) == 0 {
		t.Fatal("镜像A 一段正经分片都没分到，测试没测到东西")
	}
	other.mu.Lock()
	got := map[string]bool{}
	for _, rg := range other.ranges {
		got[rg] = true
	}
	other.mu.Unlock()
	origin.mu.Lock()
	ogot := map[string]bool{}
	for _, rg := range origin.ranges {
		ogot[rg] = true
	}
	origin.mu.Unlock()
	covered := 0
	for _, rg := range failedRanges {
		if got[rg] {
			covered++
		}
		if ogot[rg] {
			t.Fatalf("还有镜像可用时，失败分片 %s 不该回官方", rg)
		}
	}
	if covered == 0 {
		t.Fatalf("镜像A 失败的 %v 一段都没被镜像B 接住", failedRanges)
	}

}

// 动态派活的意义：慢源不该占着段位。40MB 里两台快源 + 一台每请求 30ms 的慢源，
// 分段仍然只有 20 段、并发仍然 4，但慢源拿到的段数必须明显少于快源。
func TestParallelDynamicBalancing(t *testing.T) {
	g, st, _, body, _, _, os_, ms := parFixture(t, 40<<20)
	plan, cand := parPlan(t, os_, body)

	slow := &rangeSrv{body: body, delay: 30 * time.Millisecond}
	os2 := httptest.NewServer(slow)
	t.Cleanup(os2.Close)
	third := &rangeSrv{body: body}
	os3 := httptest.NewServer(third)
	t.Cleanup(os3.Close)

	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))
	id, err := st.AddSource(store.Source{Name: "镜像A", BaseURL: ms.URL, Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id
	id2, err := st.AddSource(store.Source{Name: "慢源", BaseURL: os2.URL, Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	id3, err := st.AddSource(store.Source{Name: "镜像C", BaseURL: os3.URL, Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	c2 := &scheduler.Candidate{ID: id2, Name: "慢源", BaseURL: os2.URL}
	c3 := &scheduler.Candidate{ID: id3, Name: "镜像C", BaseURL: os3.URL}
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{
			{u: mustURL(t, ms.URL), c: cand},
			{u: mustURL(t, os2.URL), c: c2},
			{u: mustURL(t, os3.URL), c: c3},
			{u: plan.Upstream},
		}
	}
	rec := parRun(g, plan)
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatal("动态派活后内容必须仍然逐字节正确")
	}
	// 各减掉预检那 1 次请求。
	a := atomic.LoadInt32(&third.hits) - 1
	sl := atomic.LoadInt32(&slow.hits) - 1
	if a <= 0 {
		t.Fatalf("第三台源应参与分片，实际 %d 段", a)
	}
	if sl >= a {
		t.Fatalf("慢源不该比快源拿得多：慢 %d 段 vs 快 %d 段", sl, a)
	}
	if sl > 6 {
		t.Fatalf("慢源拿太多段（%d），动态派活没起作用", sl)
	}
}

// 官方源不能排第一：第 0 段是直通客户端的，官方直连被墙时整条流卡死。
// 实测 /v2/ghcr.io/sagernet/sing-box 31MB：官方排首时 22 KB/s，38 秒 1.2 MB。
func TestPreferMirrorsPutsOfficialLast(t *testing.T) {
	mk := func(u string) parSource {
		pu, _ := url.Parse(u)
		return parSource{t: fetchTarget{u: pu}}
	}
	srcs := []parSource{
		mk("https://ghcr.io/v2/sagernet/sing-box/blobs/sha256:x"),
		mk("https://ghcr.nju.edu.cn/v2/ghcr.io/sagernet/sing-box/blobs/sha256:x"),
		mk("https://docker.sparkcr.cn/v2/ghcr.io/sagernet/sing-box/blobs/sha256:x"),
	}
	preferMirrors(srcs, "https://ghcr.io/v2/sagernet/sing-box/blobs/sha256:x")
	if got := srcs[len(srcs)-1].t.u.Host; got != "ghcr.io" {
		t.Fatalf("官方源应排最后，实际最后是 %s", got)
	}
	if got := srcs[0].t.u.Host; got == "ghcr.io" {
		t.Fatal("第 0 段不能交给官方源")
	}
	// 只有一个源时保持原样，不能把唯一可用的源挪掉。
	one := []parSource{mk("https://ghcr.io/v2/x/blobs/y")}
	preferMirrors(one, "https://ghcr.io/v2/x/blobs/y")
	if len(one) != 1 || one[0].t.u.Host != "ghcr.io" {
		t.Fatal("单源不应改变")
	}
}

// 官方源卡死时预检不能跟着卡死：响应头要等预检结束才写，
// 官方拖满 20s 连接超时，用户看到的就是「点了没反应」。
func TestParPreflightCapsSlowOfficial(t *testing.T) {
	old := parProbeTimeout
	parProbeTimeout = 150 * time.Millisecond
	defer func() { parProbeTimeout = old }()

	g, _, _, body, origin, _, os, ms := parFixture(t, 9<<20)
	origin.delay = 10 * time.Second
	plan, cand := parPlan(t, os, body)
	mu, err := url.Parse(ms.URL + "/v2/library/x/blobs/sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	targets := []fetchTarget{{u: plan.Upstream}, {u: mu, c: cand}}

	start := time.Now()
	srcs, total := g.parPreflight(context.Background(), plan, targets, 0)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("预检被官方源拖住 %v", el)
	}
	if total != int64(len(body)) {
		t.Fatalf("总长应取自镜像源 %d，实际 %d", len(body), total)
	}
	if len(srcs) != 1 || srcs[0].t.u.Host != mu.Host {
		t.Fatalf("官方超时应被丢弃，只剩镜像源，实际 %d 个", len(srcs))
	}
}
