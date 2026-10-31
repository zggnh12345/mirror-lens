package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 流式回源要分别记两个数：首字节（源站响应快不快）和整次传输耗时
// （调度评分该用的那个）。混在一起的话，一个握手快、传输慢的源会显得最快。
func TestStreamRecordsTTFBAndTotalSeparately(t *testing.T) {
	metrics.Enable()
	t.Cleanup(metrics.Disable)

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

	body := bytes.Repeat([]byte("0123456789abcdef"), 2<<16) // 2 MiB
	const chunk = 64 << 10
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		for off := 0; off < len(body); off += chunk {
			end := off + chunk
			if end > len(body) {
				end = len(body)
			}
			_, _ = w.Write(body[off:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(20 * time.Millisecond) // 模拟慢速传输：头很快，身子很慢
		}
	}))
	defer mirror.Close()
	addSchedSource(t, st, "mirror", mirror.URL, 50, 5000)

	g := New(st, Options{
		Cache:    cs,
		RawBase:  "https://raw.githubusercontent.com",
		MaxBytes: 1 << 20, // 小于 body，走流式
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	rec := doGH(g, token(t, st), "http://gw/gh/o/r/releases/download/v1/slow.bin")
	if rec.Code != http.StatusOK {
		t.Fatalf("下载失败 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != len(body) {
		t.Fatalf("应发完整 %d 字节，实际 %d", len(body), rec.Body.Len())
	}

	out := scrapeMetrics(t)
	ttfb := histSum(t, out, `mirrorlens_source_ttfb_seconds_sum{kind="release",mode="stream"}`)
	total := histSum(t, out, `mirrorlens_source_fetch_duration_seconds_sum{kind="release",mode="stream"}`)

	if ttfb <= 0 {
		t.Fatalf("没记首字节耗时:\n%s", out)
	}
	// 32 段 × 20ms ≈ 0.64s 的传输，首字节应该远小于它。
	if total < ttfb+0.3 {
		t.Fatalf("完整耗时没含传输时间：ttfb=%.3fs total=%.3fs", ttfb, total)
	}
	if ttfb > 0.3 {
		t.Fatalf("首字节不该包含传输时间，得到 %.3fs", ttfb)
	}
}

func scrapeMetrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Get().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func histSum(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, series+" ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, series)), 64)
		if err != nil {
			t.Fatalf("解析 %q: %v", line, err)
		}
		return v
	}
	return 0
}
