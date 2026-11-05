package scheduler

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// 排队拿到名额后，必须留下闸门等待样本和 inflight 均值。
func TestGateWaitAndInflightAvg(t *testing.T) {
	metrics.Enable()
	defer metrics.Disable()

	g := newGate(7, 1, 1)
	release, err := g.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		rel, err := g.acquire(ctx)
		if err == nil {
			time.Sleep(10 * time.Millisecond)
			rel()
		}
		done <- err
	}()

	time.Sleep(80 * time.Millisecond) // 让第二个请求真的排进队列
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	txt := scrapeMetrics(t)
	if !strings.Contains(txt, `mirrorlens_source_gate_wait_seconds_count{source_id="7"} 1`) {
		t.Fatalf("缺少闸门等待样本：\n%s", grepLines(txt, "gate_wait"))
	}
	if !strings.Contains(txt, `mirrorlens_source_inflight_avg{source_id="7"}`) {
		t.Fatalf("缺少 inflight 均值：\n%s", grepLines(txt, "inflight_avg"))
	}
}

// ctx 取消不该被当成一次排队等待。
func TestGateWaitIgnoresCancel(t *testing.T) {
	metrics.Enable()
	defer metrics.Disable()

	g := newGate(9, 1, 1)
	release, err := g.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.acquire(ctx); err == nil {
		t.Fatal("已取消的 ctx 应返回错误")
	}

	if txt := scrapeMetrics(t); strings.Contains(txt, `source_gate_wait_seconds_count{source_id="9"}`) {
		t.Fatalf("取消不应计入等待：\n%s", grepLines(txt, "gate_wait"))
	}
}

// 平均值在只有并发没有等待时也要能反映占用。
func TestInflightAvgReflectsBusy(t *testing.T) {
	metrics.Enable()
	defer metrics.Disable()

	g := newGate(11, 4, 4)
	rel, err := g.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	rel()

	txt := scrapeMetrics(t)
	line := grepLines(txt, `source_inflight_avg{source_id="11"}`)
	if line == "" {
		t.Fatal("缺少 inflight 均值")
	}
	f := parseMetricValue(t, line)
	if f <= 0 {
		t.Fatalf("均值应大于 0，实际 %v", f)
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func parseMetricValue(t *testing.T, line string) float64 {
	t.Helper()
	i := strings.LastIndex(line, " ")
	if i < 0 {
		t.Fatalf("无法解析指标行: %q", line)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
	if err != nil {
		t.Fatalf("无法解析数值 %q: %v", line, err)
	}
	return v
}
