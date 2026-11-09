package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

// 同源接管走真实调用点：midFill → Transfer → Activate → newLeaseBody。
// 这条路径曾经绕过构造函数，defer resp0.Body.Close() 必 panic。
func TestMidFillSameSourceLeaseBodyClose(t *testing.T) {
	f := newMidFixture(t, 2)
	defer f.close()
	metrics.Enable()
	t.Cleanup(metrics.Disable)

	g := f.g
	g.SetScheduler(scheduler.New(f.st, scheduler.Config{Enabled: true, MaxPerSource: 4, MaxCandidates: 3}))

	const srcID = int64(77)
	l, err := g.sched.AcquireLease(context.Background(), srcID, scheduler.KindNormal)
	if err != nil || l == nil {
		t.Fatalf("acquire lease: %v", err)
	}
	retireOld, err := l.Activate()
	if err != nil {
		t.Fatal(err)
	}

	ctx, hm := withLeaseHolder(context.Background())
	hm.set(l)
	// 两台源都映射到同一个源 ID：命中同源接管分支（Transfer，不新开名额）。
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{
			{u: f.urls[0], c: &scheduler.Candidate{ID: srcID, BaseURL: f.urls[0].String()}},
			{u: f.urls[1], c: &scheduler.Candidate{ID: srcID, BaseURL: f.urls[1].String()}},
		}
	}

	if err := f.cs.ResetPartial(f.key); err != nil {
		t.Fatal(err)
	}
	part, err := f.cs.OpenPartial(f.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(f.data[:midSent]); err != nil {
		t.Fatal(err)
	}
	plan := &adapter.Plan{
		Upstream: f.urls[0], CacheKey: f.key, ExpectSHA: f.sha,
		Stream: true, Cacheable: true, Kind: "docker", TTL: time.Hour,
	}
	req := httptest.NewRequest(http.MethodGet, "/v2/library/postgres/blobs/sha256:"+f.sha, nil).WithContext(ctx)
	old := &http.Response{Request: req, Header: http.Header{}}

	var out bytes.Buffer
	taken, ok := g.midFill(&out, plan, part, midSent, midTotal, nil, old, func() {})
	if !taken || !ok {
		t.Fatalf("同源接管失败: taken=%v ok=%v", taken, ok)
	}
	if !bytes.Equal(out.Bytes(), f.data[midSent:]) {
		t.Fatal("同源接管后客户端字节不一致")
	}

	// 新 Body 已关闭，但旧 Body 还持着引用：名额不能提前归还。
	if st := l.State(); st != scheduler.StateReclaiming {
		t.Fatalf("接管 Body 关闭后 Lease 状态 %s，期望 reclaiming", st)
	}
	if got := leaseGauge(t, "leases_held", srcID); got != 1 {
		t.Fatalf("旧 Body 未退出时 held=%v，期望 1", got)
	}
	if got := leaseGauge(t, "a_bodies_active", srcID); got != 1 {
		t.Fatalf("旧 Body 未退出时 bodies=%v，期望 1", got)
	}

	retireOld()
	retireOld() // 必须幂等
	if st := l.State(); st != scheduler.StateReleased {
		t.Fatalf("最后一个 Body 退出后状态 %s，期望 released", st)
	}
	if got := leaseGauge(t, "leases_held", srcID); got != 0 {
		t.Fatalf("名额应恰好归还一次，held=%v", got)
	}
	if got := leaseGauge(t, "a_bodies_active", srcID); got != 0 {
		t.Fatalf("bodies 应归零，得到 %v", got)
	}
}

func leaseGauge(t *testing.T, name string, id int64) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Get().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	prefix := "mirrorlens_" + name + "{"
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, prefix) || !strings.Contains(line, `source_id="`+strconv.FormatInt(id, 10)+`"`) {
			continue
		}
		f := strings.Fields(line)
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", line, err)
		}
		return v
	}
	return 0
}
