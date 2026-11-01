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
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// Range/HEAD 走的是 passThrough，以前这条路收到响应就直接转发，既不记
// source_http_responses_total 也不记 429——最费流量的那部分请求在统计里
// 不存在，429 也不会让这台源进冷却。
func TestPassThroughRecords429AndCoolsSource(t *testing.T) {
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

	var mirrorHits int32
	msrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mirrorHits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer msrv.Close()

	id := addSchedSource(t, st, "mirror", msrv.URL, 10, 9000)

	host := strings.TrimPrefix(osrv.URL, "http://")
	ad := registryadapter.New(host, 4<<30, 5*time.Minute)
	ad.Scheme = "http"

	g := New(st, Options{
		Cache:    cs,
		MaxBytes: 256 << 20,
		Registry: ad,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	sch := scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	g.SetScheduler(sch)

	reg := metrics.Enable()
	defer metrics.Disable()

	blobPath := "/v2/library/alpine/blobs/" + origin.blobDigest()
	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://gw"+blobPath, nil)
		r.Header.Set("Range", "bytes=0-9")
		w := httptest.NewRecorder()
		g.ServeRegistry(w, r)
		return w
	}

	// 第一次：镜像回 429，客户端拿到 429，但这次响应必须记账。
	if w := do(); w.Code != http.StatusTooManyRequests {
		t.Fatalf("第一次应原样返回 429，实际 %d", w.Code)
	}
	if n := atomic.LoadInt32(&mirrorHits); n != 1 {
		t.Fatalf("第一次应只打镜像一次，实际 %d", n)
	}

	out := scrape(t, reg)
	for _, want := range []string{
		`mirrorlens_source_429_total{kind="docker"} 1`,
		`mirrorlens_source_http_responses_total{kind="docker"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("指标缺 %s:\n%s", want, out)
		}
	}

	// 429 要进冷却：下一次不该再选这台源，直接打源站。
	w2 := do()
	if w2.Code != http.StatusPartialContent && w2.Code != http.StatusOK {
		t.Fatalf("第二次应换到源站，实际 %d", w2.Code)
	}
	if n := atomic.LoadInt32(&mirrorHits); n != 1 {
		t.Fatalf("冷却中的源不该再被选中，镜像被打了 %d 次", n)
	}
	if got := sch.InFlight(id); got != 0 {
		t.Fatalf("透传结束后在途计数应归零，实际 %d", got)
	}
}

func scrape(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}
