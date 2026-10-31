package scheduler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 删除源之后，调度器不能再把它排进候选，熔断/闸门/动态吞吐这些运行态
// 也要一起清掉——留着不但占内存，id 被新源复用时还会把旧限流套上去。
func TestForgetDropsRuntimeState(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(store.Source{
		Name: "gone", BaseURL: "https://gone.example", Status: "usable",
		Enabled: true, DockerCap: store.CapYes, GithubCap: store.CapYes,
	})
	if err != nil {
		t.Fatal(err)
	}

	metrics.Enable()
	t.Cleanup(metrics.Disable)

	s := New(st, Config{Enabled: true, MaxCandidates: 3, Dynamic: true, MaxPerSource: 4})
	if got := s.Candidates("docker"); len(got) != 1 {
		t.Fatalf("应有一个候选，得到 %+v", got)
	}

	// 制造运行态：熔断、并发闸门、动态吞吐、逐源指标。
	s.Report("docker", Candidate{ID: id, BaseURL: "https://gone.example"}, false, time.Second)
	s.Observe(id, 1<<20, time.Second, true, 200)
	metrics.SourceLimit(id, 3)
	metrics.SourceInflight(id, 1)

	s.mu.Lock()
	brk := len(s.brk)
	s.mu.Unlock()
	s.loadMu.Lock()
	gates := len(s.gates)
	s.loadMu.Unlock()
	s.mu.Lock()
	dyn := len(s.dyn)
	s.mu.Unlock()
	if brk == 0 || gates == 0 || dyn == 0 {
		t.Fatalf("前置状态没造出来: brk=%d gates=%d dyn=%d", brk, gates, dyn)
	}

	// 用户删除源。
	if err := st.DeleteSource(id); err != nil {
		t.Fatal(err)
	}
	s.Forget(id)

	s.mu.Lock()
	brk, dyn, lists := len(s.brk), len(s.dyn), len(s.lists)
	s.mu.Unlock()
	if got := s.Candidates("docker"); len(got) != 0 {
		t.Fatalf("删掉的源还在候选里: %+v", got)
	}
	if brk != 0 || dyn != 0 || lists != 0 {
		t.Fatalf("运行态没清干净: brk=%d dyn=%d lists=%d", brk, dyn, lists)
	}
	s.loadMu.Lock()
	gates = len(s.gates)
	s.loadMu.Unlock()
	if gates != 0 {
		t.Fatalf("并发闸门没清掉: %d 个", gates)
	}

	// 指标里也不该再有这个 source_id 的序列。
	body := scrapeMetrics(t)
	if containsSourceID(body, id) {
		t.Fatalf("指标里还留着已删除源的序列:\n%s", body)
	}
}

func scrapeMetrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Get().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func containsSourceID(body string, id int64) bool {
	return strings.Contains(body, fmt.Sprintf(`source_id="%d"`, id))
}
