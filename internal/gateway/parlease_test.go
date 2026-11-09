package gateway

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

// 名额耗尽（有限等待超时）只是「这台源此刻没空」，必须换下一台源，
// 而不是让整段失败、把已开始的响应掐断。
func TestParChunkLeaseTimeoutFallsBack(t *testing.T) {
	f := newMidFixture(t, 2)
	defer f.close()
	metrics.Enable()
	t.Cleanup(metrics.Disable)

	g := f.g
	g.SetScheduler(scheduler.New(f.st, scheduler.Config{
		Enabled: true, MaxPerSource: 1, MaxCandidates: 3,
		Admission: "wait_then_fallback", WaitTimeout: 80 * time.Millisecond,
	}))

	const busyID, freeID = int64(501), int64(502)
	held, err := g.sched.AcquireLease(context.Background(), busyID, scheduler.KindNormal)
	if err != nil || held == nil {
		t.Fatalf("占用 busy 源名额失败: %v", err)
	}
	defer held.Release(held.Token())

	targets := []fetchTarget{
		{u: f.urls[0], c: &scheduler.Candidate{ID: busyID, BaseURL: f.urls[0].String()}},
		{u: f.urls[1], c: &scheduler.Candidate{ID: freeID, BaseURL: f.urls[1].String()}},
	}
	g.parTargets = func(*adapter.Plan) []fetchTarget { return targets }
	srcs := []parSource{{t: targets[0]}, {t: targets[1]}}

	part, err := f.cs.OpenPartial(f.key)
	if err != nil {
		t.Fatal(err)
	}
	plan := &adapter.Plan{
		Upstream: f.urls[1], CacheKey: f.key, ExpectSHA: f.sha,
		Stream: true, Cacheable: true, Kind: "docker", TTL: time.Hour,
	}
	stats := make([]parStat, len(srcs))
	before := metricScalar(t, "par_fill_lease_timeout_total")

	if err := g.parChunk(context.Background(), plan, srcs[0], srcs, 0, 0, midTotal, midTotal, part, stats, nil); err != nil {
		t.Fatalf("首台源名额超时后应换源成功，实得错误: %v", err)
	}

	got, err := readAtFull(part, midTotal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.data) {
		t.Fatal("换源后写出的分片内容不正确")
	}
	if after := metricScalar(t, "par_fill_lease_timeout_total"); after <= before {
		t.Fatalf("未记录名额超时换源: before=%v after=%v", before, after)
	}
}

func readAtFull(f *os.File, n int64) ([]byte, error) {
	buf := make([]byte, n)
	_, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}
