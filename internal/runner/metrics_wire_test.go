package runner

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/probe"
)

// A2-2：调度写入必须能被指标看见，且 pending 由 SQL 统计而不是内存推算。
func TestScheduleRetryMetrics(t *testing.T) {
	reg := metrics.Enable()
	defer metrics.Disable()

	f := newHostProbeFixture(t, "quay.io")
	f.now = time.Now()
	gen, err := f.st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	jobs := []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", Gen: gen, BaseURL: "https://mirror.example", Repo: "r"}}
	r, _ := f.runner(t, jobs, func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
		// 结果自相矛盾 → retryOutcomeInconsistent，落一次持久化退避。
		return probe.RegistryHostOutcome{Host: "quay.io", Cap: probe.CapYes, Selected: "native"}
	})
	r.Once(context.Background())

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`hostprobe_retry_scheduled_total{reason="outcome_inconsistent"} 1`,
		`hostprobe_retry_pending 1`,
		`hostprobe_retry_delay_seconds_count 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
}
