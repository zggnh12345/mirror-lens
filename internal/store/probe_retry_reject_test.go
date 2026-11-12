package store

import (
	"strings"
	"testing"
	"time"
)

// 拒绝原因必须与真实 CAS 条件一致，否则指标会撒谎。
func TestScheduleRetryRejectClassification(t *testing.T) {
	st, id, gen := retryTestStore(t)
	now := time.Now().Unix()
	req := func(gen int64) HostProbeRetry {
		return HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: now}
	}

	// 代次不符
	if rej, _, err := st.ScheduleHostProbeRetryClassified(req(gen + 99)); err != nil || rej != RetryRejectGenMismatch {
		t.Fatalf("代次不符应报 gen_mismatch: rej=%q err=%v", rej, err)
	}

	// 第一次写入成功，第二次同 At 已不到期 → cas_lost
	if rej, d, err := st.ScheduleHostProbeRetryClassified(req(gen)); err != nil || rej != RetryWritten || d != 15*time.Second {
		t.Fatalf("首次写入应成功且延迟 15s: rej=%q d=%v err=%v", rej, d, err)
	}
	if rej, _, err := st.ScheduleHostProbeRetryClassified(req(gen)); err != nil || rej != RetryRejectCASLost {
		t.Fatalf("未到期应报 cas_lost: rej=%q err=%v", rej, err)
	}

	// 源被禁用 → source_unavailable（分类不再看调度时间）
	off := false
	if err := st.UpdateSource(id, &off, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if rej, _, err := st.ScheduleHostProbeRetryClassified(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: now + 3600}); err != nil || rej != RetryRejectSourceUnavailable {
		t.Fatalf("禁用源应报 source_unavailable: rej=%q err=%v", rej, err)
	}

	// hard-excluded 同样算源不可用，且不得被推到未来
	if _, err := st.db.Exec(`UPDATE registry_host_recovery SET state='hard-excluded' WHERE source_id=? AND family='registry'`, id); err != nil {
		t.Fatal(err)
	}
	on := true
	if err := st.UpdateSource(id, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if rej, _, err := st.ScheduleHostProbeRetryClassified(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: now + 7200}); err != nil || rej != RetryRejectSourceUnavailable {
		t.Fatalf("hard-excluded 应报 source_unavailable: rej=%q err=%v", rej, err)
	}
}

func TestCountRetryPending(t *testing.T) {
	st, id, gen := retryTestStore(t)
	now := time.Now().Unix()
	if rej, _, err := st.ScheduleHostProbeRetryClassified(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: now}); err != nil || rej != RetryWritten {
		t.Fatal(err)
	}
	if n, err := st.CountRetryPending(now); err != nil || n != 1 {
		t.Fatalf("退避中应计 1: n=%d err=%v", n, err)
	}
	// 到期后不再计入 pending（仍然失败计数，但不在退避）
	if n, err := st.CountRetryPending(now + 3600); err != nil || n != 0 {
		t.Fatalf("到期后不应计入: n=%d err=%v", n, err)
	}
	// 参数错误走 db_error，且不写库
	rej, _, err := st.ScheduleHostProbeRetryClassified(HostProbeRetry{SourceID: 0, Host: "", At: now})
	if err == nil || rej != RetryRejectDBError || !strings.Contains(err.Error(), "参数不完整") {
		t.Fatalf("参数错误应报 db_error: rej=%q err=%v", rej, err)
	}
}
