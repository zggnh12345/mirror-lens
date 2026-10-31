package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func srv(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "0")
		if code == 0 {
			w.Header().Set("Content-Length", "")
			w.Header().Set("ETag", `"x"`)
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s
}

func addSchedSource(t *testing.T, st *store.Store, name, base string, lat, spd int64) int64 {
	t.Helper()
	id, err := st.AddSource(store.Source{Name: name, BaseURL: base, Status: "usable", Enabled: true, GithubCap: 1, DockerCap: 1})
	if err != nil {
		t.Fatalf("加源 %s: %v", name, err)
	}
	for i := 0; i < 3; i++ {
		if err := st.AddCheck(store.Check{SourceID: id, Kind: "github", Tier: 3, OK: true, LatencyMS: lat, SpeedKBps: spd}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// 排第一的源回 500 时，同一请求内换下一个源，客户端只看到一次成功响应。
func TestSchedulerFailover(t *testing.T) {
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
	origin := srv(t, 0, "origin-body")
	bad := srv(t, 500, "")
	good := srv(t, 0, "mirror-body")

	badID := addSchedSource(t, st, "bad", bad.URL, 50, 2000)
	addSchedSource(t, st, "good", good.URL, 400, 400)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	tok := token(t, st)
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "mirror-body" {
		t.Fatalf("应由镜像供货，实际内容 %q", got)
	}
	if got := rec.Header().Get("X-ML-Source"); got != "good" {
		t.Fatalf("X-ML-Source 应为 good，实际 %q", got)
	}

	// 失败与成功都要落到 source_events，供面板和熔断使用
	okN, failN, _, err := st.SourceEvents(badID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if okN != 0 || failN != 1 {
		t.Fatalf("bad 应记录 1 次失败，实际 ok=%d fail=%d", okN, failN)
	}
}

// TTL 到期的重新验证必须回官方源：镜像站把旧内容当「没变」时不能固化。
func TestRevalidateGoesToOrigin(t *testing.T) {
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

	cur := "v1"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"`+cur+`"`)
		io.WriteString(w, cur)
	}))
	defer origin.Close()
	mirror := srv(t, 0, "stale-v1") // 镜像一直给旧内容
	addSchedSource(t, st, "mirror", mirror.URL, 100, 900)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true}))

	now := time.Now()
	g.now = func() time.Time { return now }

	tok := token(t, st)
	if rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil); rec.Body.String() != "stale-v1" {
		t.Fatalf("首次应由镜像填充，实际 %q", rec.Body.String())
	}

	// 源站内容变了，镜像还是旧的；TTL 到期后必须拿到新内容
	cur = "v2"
	now = now.Add(30 * time.Minute)
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Body.String() != "v2" {
		t.Fatalf("重新验证应回官方源拿到 v2，实际 %q (cache=%s src=%s)", rec.Body.String(), rec.Header().Get("X-ML-Cache"), rec.Header().Get("X-ML-Source"))
	}
}
