package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func newMissFixture(t *testing.T, originCode int) (*Gateway, *store.Store, *scheduler.Scheduler, string, int64) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	origin := srv(t, originCode, "origin-body")
	miss := srv(t, http.StatusNotFound, "")
	id := addSchedSource(t, st, "miss", miss.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	sch := scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	g.SetScheduler(sch)
	return g, st, sch, token(t, st), id
}

func hasSource(list []scheduler.Candidate, id int64) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

// 镜像上没有这个文件：换下一个源，但别把它当故障——不冷却、不扣健康分。
func TestMirrorMissFailsOverWithoutCooling(t *testing.T) {
	g, st, sch, tok, id := newMissFixture(t, 0)
	reg := metrics.Enable()
	defer metrics.Disable()

	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("镜像 404 后应换源成功，实际 %d", rec.Code)
	}
	if got := rec.Body.String(); got != "origin-body" {
		t.Fatalf("应由源站供货，实际 %q", got)
	}

	// 未命中按「答得正常，只是没有」记账：成功事件 + 状态码 404。
	okN, failN, _, err := st.SourceEvents(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if okN != 1 || failN != 0 {
		t.Fatalf("未命中不该记成故障，实际 ok=%d fail=%d", okN, failN)
	}
	if codes, err := st.SourceEventCodes(id, 10); err != nil || len(codes) != 1 || codes[0] != 404 {
		t.Fatalf("事件应记下 404，实际 %v %v", codes, err)
	}

	if !hasSource(sch.Candidates("raw"), id) {
		t.Fatal("404 不该让源进冷却")
	}
	if out := scrape(t, reg); !strings.Contains(out, `mirrorlens_source_miss_total{kind="raw",code="404"} 1`) {
		t.Fatalf("缺未命中计数:\n%s", out)
	}
}

// 全部源都明确说没有：返回 404，而不是把「镜像池没有」说成别的错误。
func TestAllSourcesMissReturns404(t *testing.T) {
	g, _, _, tok, _ := newMissFixture(t, http.StatusNotFound)
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("全部未命中应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 404 后面跟着一个暂时性故障：结果不确定，不能伪装成资源不存在。
func TestMissFollowedByFaultIsNot404(t *testing.T) {
	g, _, _, tok, _ := newMissFixture(t, http.StatusInternalServerError)
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("混合结果不能报 404，实际 %d", rec.Code)
	}
}

// 直通（Range/HEAD）同样不能因为第一台镜像 404 就收工。
func TestPassThroughMissFailsOver(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	origin := &regUpstream{}
	osrv := httptest.NewServer(origin)
	defer osrv.Close()
	miss := srv(t, http.StatusNotFound, "")
	id := addSchedSource(t, st, "miss", miss.URL, 10, 9000)

	ad := registryadapter.New(strings.TrimPrefix(osrv.URL, "http://"), 4<<30, 5*time.Minute)
	ad.Scheme = "http"
	g := New(st, Options{
		Cache:    cs,
		MaxBytes: 256 << 20,
		Registry: ad,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	sch := scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	g.SetScheduler(sch)

	r := httptest.NewRequest(http.MethodGet, "http://gw/v2/library/alpine/blobs/"+origin.blobDigest(), nil)
	r.Header.Set("Range", "bytes=0-9")
	w := httptest.NewRecorder()
	g.ServeRegistry(w, r)

	if w.Code != http.StatusPartialContent && w.Code != http.StatusOK {
		t.Fatalf("镜像 404 后应换源成功，实际 %d", w.Code)
	}
	if !hasSource(sch.Candidates("docker"), id) {
		t.Fatal("直通路径的 404 也不该冷却源")
	}
}
