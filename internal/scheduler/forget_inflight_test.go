package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 删源时仍有请求在跑：迟到回调不能重建熔断、吞吐、闸门和事件行。
func TestForgetSurvivesLateCallbacks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(store.Source{
		Name: "inflight", BaseURL: "https://inflight.example", Status: "usable",
		Enabled: true, DockerCap: store.CapYes, GithubCap: store.CapYes,
	})
	if err != nil {
		t.Fatal(err)
	}

	metrics.Enable()
	t.Cleanup(metrics.Disable)

	s := New(st, Config{Enabled: true, MaxCandidates: 3, Dynamic: true, MaxPerSource: 4})
	cand := Candidate{ID: id, BaseURL: "https://inflight.example"}

	// 请求在途：占着闸门、有动态吞吐样本、已经上报过一次。
	release, err := s.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	s.Observe(id, 1<<20, time.Second, true, 200)
	s.Report("docker", cand, true, 20*time.Millisecond)
	metrics.SourceLimit(id, 3)
	metrics.SourceInflight(id, 1)

	// 用户删除源，运行态清空。
	if err := st.DeleteSource(id); err != nil {
		t.Fatal(err)
	}
	s.Forget(id)

	// 在途请求结束：先释放闸门，再补上迟到的回调和分片观测。
	release()
	s.Report("docker", cand, false, time.Second)
	s.Observe(id, 1<<20, time.Second, true, 200)
	if rel, err := s.Acquire(context.Background(), id); err != nil {
		t.Fatalf("在途请求不该因为源被删而失败: %v", err)
	} else {
		rel()
	}

	s.mu.Lock()
	brk, dyn, lists, sticky := len(s.brk), len(s.dyn), len(s.lists), len(s.sticky)
	s.mu.Unlock()
	if brk != 0 || dyn != 0 || lists != 0 || sticky != 0 {
		t.Fatalf("迟到的回调重建了运行态: brk=%d dyn=%d lists=%d sticky=%d", brk, dyn, lists, sticky)
	}
	s.loadMu.Lock()
	gates := len(s.gates)
	s.loadMu.Unlock()
	if gates != 0 {
		t.Fatalf("迟到的 Acquire 重建了闸门: %d 个", gates)
	}
	if got := s.Candidates("docker"); len(got) != 0 {
		t.Fatalf("删掉的源又回到候选里: %+v", got)
	}
	if body := scrapeMetrics(t); containsSourceID(body, id) {
		t.Fatalf("指标里又冒出已删除源的序列:\n%s", body)
	}

	// source_events 没有外键：迟到的上报不能把行插回去。
	codes, err := st.SourceEventCodes(id, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 0 {
		t.Fatalf("删除后 source_events 又多了 %d 行: %v", len(codes), codes)
	}
	// 最坏的一瞬：上报方已经过了墓碑检查，Forget 才完成。事件表没有外键，
	// 只能靠 SQL 里的存在性判断挡住这一行。
	st.RecordSourceEvent(id, "docker", true, 12, 200)
	if codes, err := st.SourceEventCodes(id, 50); err != nil {
		t.Fatal(err)
	} else if len(codes) != 0 {
		t.Fatalf("源已删除，事件仍被插回 %d 行: %v", len(codes), codes)
	}
}
