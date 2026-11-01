package metrics

import (
	"testing"
	"time"
)

func TestSnapshotViewAggregates(t *testing.T) {
	Enable()
	defer Disable()

	// 缓存盘快照 + 源站数快照
	SetCache(1000, 3, 500)
	SetSources(map[SourceStat]uint64{
		{Kind: "docker", Status: "usable"}:   3,
		{Kind: "github", Status: "degraded"}: 1,
	})

	// 制造一些场景流量：7 次命中 + 3 次未命中 => 命中率 0.7
	for i := 0; i < 7; i++ {
		HTTP("docker", "GET", 200, time.Millisecond, 0, "hit")
	}
	for i := 0; i < 3; i++ {
		HTTP("docker", "GET", 200, time.Millisecond, 0, "miss")
	}
	SnapshotHitRatio()
	SnapshotSceneHotness()

	// 并发占用 + 动态上限
	SourceInflight(7, 2)
	SourceLimit(7, 8)
	SourceInflight(9, 1)
	SourceLimit(9, 4)

	v := SnapshotView()
	if !v.Enabled {
		t.Fatal("启用后 Enabled 应为 true")
	}
	if !v.Cache.HasCache || v.Cache.Bytes != 1000 || v.Cache.Blobs != 3 || v.Cache.Free != 500 {
		t.Fatalf("缓存快照不对：%+v", v.Cache)
	}
	if len(v.Sources) != 2 {
		t.Fatalf("源站数应为 2 组，实际 %d", len(v.Sources))
	}
	if !v.Ratio.HasRatio {
		t.Fatal("应有命中率")
	}
	if got := v.Ratio.Overall; got < 0.69 || got > 0.71 {
		t.Fatalf("整体命中率应为 ~0.7，实际 %v", got)
	}
	if v.Ratio.Scenes["docker"] < 0.69 || v.Ratio.Scenes["docker"] > 0.71 {
		t.Fatalf("docker 场景命中率应为 ~0.7，实际 %v", v.Ratio.Scenes["docker"])
	}
	if v.Heat["docker"] != 10 {
		t.Fatalf("docker 热度应为 10，实际 %d", v.Heat["docker"])
	}
	if v.Concurrency.Inflight != 3 {
		t.Fatalf("在途并发应为 3，实际 %d", v.Concurrency.Inflight)
	}
	if v.Concurrency.Limit != 12 {
		t.Fatalf("并发上限应为 12，实际 %d", v.Concurrency.Limit)
	}
	if len(v.Concurrency.Top) != 2 {
		t.Fatalf("Top 并发应含 2 台源，实际 %d", len(v.Concurrency.Top))
	}
	if v.Concurrency.Top[0].ID != 7 || v.Concurrency.Top[0].Inflight != 2 {
		t.Fatalf("Top[0] 应为 id=7 inflight=2，实际 %+v", v.Concurrency.Top[0])
	}
}

func TestSnapshotViewDisabled(t *testing.T) {
	Disable()
	v := SnapshotView()
	if v.Enabled {
		t.Fatal("未启用时 Enabled 应为 false")
	}
	if v.Ratio.Scenes == nil || v.Heat == nil {
		t.Fatal("未启用时也应返回非 nil 的 map，避免前端 nil 解引用")
	}
}
