package gateway

import (
	"testing"
	"time"
)

func TestTaskSnapshotAndSpeed(t *testing.T) {
	r := newTaskReg()
	task := r.start("docker", "library/nginx:latest", "parallel", 8<<20, []string{"A", "B"})
	task.add(1 << 20)
	task.addSource(0, 1<<20, true)
	time.Sleep(300 * time.Millisecond)
	task.add(2 << 20)
	task.addSource(1, 2<<20, true)

	snap := r.snapshot()
	if len(snap.Active) != 1 {
		t.Fatalf("应有 1 条活跃任务，得到 %d", len(snap.Active))
	}
	a := snap.Active[0]
	if a.Done != 3<<20 || a.Total != 8<<20 || a.Mode != "parallel" {
		t.Fatalf("任务字段不对: %+v", a)
	}
	if a.BPS <= 0 {
		t.Fatalf("速度应大于 0，得到 %d", a.BPS)
	}
	if len(a.Sources) != 2 || a.Sources[0].Bytes != 1<<20 || !a.Sources[1].OK {
		t.Fatalf("每源字节不对: %+v", a.Sources)
	}
	if snap.WindowBytes != 3<<20 {
		t.Fatalf("整机吞吐窗口应记 3MB，得到 %d", snap.WindowBytes)
	}

	task.finish(true)
	snap = r.snapshot()
	if len(snap.Active) != 0 {
		t.Fatalf("结束后不该还在活跃表")
	}
	if len(snap.Recent) != 1 || !snap.Recent[0].OK || snap.Recent[0].Total != 3<<20 {
		t.Fatalf("最近完成不对: %+v", snap.Recent)
	}
}

func TestTaskRecentRing(t *testing.T) {
	r := newTaskReg()
	for i := 0; i < recentKeep+5; i++ {
		task := r.start("raw", "f.txt", "stream", 10, nil)
		task.add(10)
		task.finish(i%2 == 0)
	}
	rec := r.snapshot().Recent
	if len(rec) != recentKeep {
		t.Fatalf("最近完成最多留 %d 条，得到 %d", recentKeep, len(rec))
	}
	if rec[0].Target != "f.txt" {
		t.Fatalf("新的应该排前面: %+v", rec[0])
	}
}

// 上游不给长度时 total=-1，面板要能显示「不知道有多大」而不是 0。
func TestTaskUnknownTotal(t *testing.T) {
	r := newTaskReg()
	task := r.start("http", "big.iso", "stream", -1, []string{"官方"})
	task.add(1024)
	if a := r.snapshot().Active[0]; a.Total != -1 || a.Done != 1024 {
		t.Fatalf("未知长度字段不对: %+v", a)
	}
}
