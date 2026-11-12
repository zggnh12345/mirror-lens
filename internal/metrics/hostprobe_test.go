package metrics

import (
	"strings"
	"testing"
	"time"
)

// 四类原因必须能分别观测；未知值折叠为 other，避免标签被外部输入撑开。
func TestHostProbeRetryCounters(t *testing.T) {
	r := Enable()
	defer Disable()

	for _, s := range []string{"credential_pending", "local_execution", "commit_failed", "outcome_inconsistent"} {
		HostProbeRetryScheduled(s)
	}
	HostProbeRetryScheduled("bogus")
	for _, s := range []string{"gen_mismatch", "cas_lost", "source_unavailable", "db_error"} {
		HostProbeRetryRejected(s)
	}
	HostProbeRetryRejected("bogus")

	out := string(r.render())
	want := []string{
		`hostprobe_retry_scheduled_total{reason="credential_pending"} 1`,
		`hostprobe_retry_scheduled_total{reason="outcome_inconsistent"} 1`,
		`hostprobe_retry_scheduled_total{reason="other"} 1`,
		`hostprobe_retry_rejected_total{reason="gen_mismatch"} 1`,
		`hostprobe_retry_rejected_total{reason="db_error"} 1`,
		`hostprobe_retry_rejected_total{reason="other"} 1`,
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Fatalf("缺少 %q:\n%s", line, out)
		}
	}
}

// 桶边界：15 落 le=15，14 也落 le=15，601 只落 +Inf。
func TestHostProbeRetryDelayBuckets(t *testing.T) {
	r := Enable()
	defer Disable()

	HostProbeRetryDelay(14 * time.Second)
	HostProbeRetryDelay(15 * time.Second)
	HostProbeRetryDelay(601 * time.Second)

	out := string(r.render())
	for _, line := range []string{
		`hostprobe_retry_delay_seconds_bucket{le="15"} 2`,
		`hostprobe_retry_delay_seconds_bucket{le="30"} 2`,
		`hostprobe_retry_delay_seconds_bucket{le="600"} 2`,
		`hostprobe_retry_delay_seconds_bucket{le="+Inf"} 3`,
		`hostprobe_retry_delay_seconds_count 3`,
		`hostprobe_retry_delay_seconds_sum 630`,
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("缺少 %q:\n%s", line, out)
		}
	}
}

func TestHostProbeRetryPendingGauge(t *testing.T) {
	r := Enable()
	defer Disable()

	if strings.Contains(string(r.render()), "hostprobe_retry_pending") {
		t.Fatal("未刷新前不应输出 gauge")
	}
	SetHostProbeRetryPending(4)
	out := string(r.render())
	if !strings.Contains(out, "hostprobe_retry_pending 4") {
		t.Fatalf("gauge 值不对:\n%s", out)
	}
}

// 指标关闭时所有入口必须是零开销空操作，不能 panic。
func TestHostProbeRetryDisabledNoop(t *testing.T) {
	Disable()
	HostProbeRetryScheduled("commit_failed")
	HostProbeRetryRejected("cas_lost")
	HostProbeRetryDelay(time.Second)
	SetHostProbeRetryPending(1)
}
