package store

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// 3.5.18 18c：公式级测试（冻结稿 §3.3）。
// 覆盖 0/1/49/50 样本、成功率边界、无可测 TTFB、P50、TTFB 口径、并列稳定、窗口容量。
func TestDynamicScoreFormula(t *testing.T) {
	mk := func(n, succ, ttfbN int, ttfb int64) MappingState {
		var sm []DynamicSample
		for i := 0; i < n; i++ {
			ok := i < succ
			meas := i < ttfbN
			sm = append(sm, DynamicSample{TS: int64(i), OK: ok, TTFBMeasurable: meas, TTFBMS: ttfb})
		}
		return MappingState{RegistryHost: "r.example", SourceID: 1, Samples: sm}
	}

	// 0 样本
	e, ready := ScoreOf(mk(0, 0, 0, 0))
	if ready || e.ValidSamples != 0 || e.Score != 0 {
		t.Fatalf("0 样本应未就绪且分数 0: %+v ready=%v", e, ready)
	}
	// 1 样本：成功 + 可测 TTFB=800 → SR=1, LS=1/(1+1)=0.5, Score=0.6+0.2=0.8
	e, ready = ScoreOf(mk(1, 1, 1, 800))
	if ready || !approx(e.SuccessRate, 1.0) || !approx(e.LatencyScore, 0.5) || !approx(e.Score, 0.8) {
		t.Fatalf("1 样本评分错: %+v ready=%v", e, ready)
	}
	// 49 样本：仍未达门槛
	_, ready = ScoreOf(mk(49, 49, 49, 100))
	if ready {
		t.Fatal("49 样本不应就绪")
	}
	// 50 样本：达门槛；全成功 + TTFB=0(可测) → LS=1, Score=1.0
	e, ready = ScoreOf(mk(50, 50, 50, 0))
	if !ready || !approx(e.Score, 1.0) {
		t.Fatalf("50 样本全成功评分应为 1: %+v ready=%v", e, ready)
	}
	// 成功率边界：全失败 → SR=0
	e, _ = ScoreOf(mk(50, 0, 50, 800))
	if !approx(e.SuccessRate, 0) || !approx(e.LatencyScore, 0.5) || !approx(e.Score, 0.2) {
		t.Fatalf("全失败评分错: %+v", e)
	}
	// 无可测 TTFB → LatencyScore=0，分数=0.6*SR
	e, _ = ScoreOf(mk(50, 25, 0, 0))
	if !approx(e.SuccessRate, 0.5) || e.P50TTFBMS != 0 || !approx(e.LatencyScore, 0) || !approx(e.Score, 0.3) {
		t.Fatalf("无可测 TTFB 评分错: %+v", e)
	}
	// P50：偶数取两中间平均（TTFB 100,300 → P50=200 → LS=1/(1+0.25)=0.8）
	m := MappingState{RegistryHost: "r", SourceID: 2, Samples: []DynamicSample{
		{OK: true, TTFBMeasurable: true, TTFBMS: 100},
		{OK: true, TTFBMeasurable: true, TTFBMS: 300},
	}}
	e, _ = ScoreOf(m)
	if !approx(e.P50TTFBMS, 200) || !approx(e.LatencyScore, 0.8) {
		t.Fatalf("P50/延迟评分错: %+v", e)
	}
	// 失败样本有响应头 → TTFB 也进 P50（采集方保证 OK=false 但 Measurable=true）
	m = MappingState{RegistryHost: "r", SourceID: 3, Samples: []DynamicSample{
		{OK: false, TTFBMeasurable: true, TTFBMS: 100},
		{OK: true, TTFBMeasurable: true, TTFBMS: 300},
	}}
	e, _ = ScoreOf(m)
	if !approx(e.P50TTFBMS, 200) || !approx(e.SuccessRate, 0.5) {
		t.Fatalf("失败样本延迟应参与 P50: %+v", e)
	}
	// 超时无响应头 → 只进分母，不进 P50
	m = MappingState{RegistryHost: "r", SourceID: 4, Samples: []DynamicSample{
		{OK: false, TTFBMeasurable: false, TTFBMS: 0},
		{OK: true, TTFBMeasurable: true, TTFBMS: 100},
	}}
	e, _ = ScoreOf(m)
	if !approx(e.P50TTFBMS, 100) || !approx(e.LatencyScore, 1/(1.0+100.0/800)) {
		t.Fatalf("无响应头样本不应虚构 TTFB: %+v", e)
	}
}

// 并列按固定顺序兜底、不随机。
func TestDynamicScoreStableTie(t *testing.T) {
	maps := []MappingState{
		{RegistryHost: "z.example", SourceID: 1, Samples: []DynamicSample{{OK: true, TTFBMeasurable: true, TTFBMS: 100}}},
		{RegistryHost: "a.example", SourceID: 2, Samples: []DynamicSample{{OK: true, TTFBMeasurable: true, TTFBMS: 100}}},
		{RegistryHost: "a.example", SourceID: 1, Samples: []DynamicSample{{OK: true, TTFBMeasurable: true, TTFBMS: 100}}},
	}
	got := ComputeScores(maps)
	// 分数全相等 → 按 host 字典序、sid 升序稳定兜底
	want := []MappingIdentity{{"a.example", 1}, {"a.example", 2}, {"z.example", 1}}
	for i, w := range want {
		if got[i].RegistryHost != w.Host || got[i].SourceID != w.SID {
			t.Fatalf("并列稳定排序第%d位错: got %s/%d want %s/%d", i, got[i].RegistryHost, got[i].SourceID, w.Host, w.SID)
		}
	}
}

// 窗口容量：每次写入后每映射最多 50 样本。
func TestDynamicWindowCapacity(t *testing.T) {
	s, _ := openTestStore(t)
	active := []MappingIdentity{{"r.example", 1}}
	for i := 0; i < 120; i++ {
		st, err := s.ApplyDynamicSample(DynamicSampleInput{
			RegistryHost: "r.example", SourceID: 1, OK: true,
			TTFBMeasurable: true, TTFBMS: int64(50 + i%100), TS: int64(i),
		}, active)
		if err != nil {
			t.Fatalf("写样本 %d: %v", i, err)
		}
		if len(st.Mappings) != 1 || len(st.Mappings[0].Samples) > 50 {
			t.Fatalf("窗口容量超限 @%d: %d", i, len(st.Mappings[0].Samples))
		}
	}
	st, _ := s.LoadDynamicState()
	if len(st.Mappings[0].Samples) != 50 {
		t.Fatalf("持久后窗口应为 50: %d", len(st.Mappings[0].Samples))
	}
}

// 重启恢复：新建 Store 实例指向同一库，状态一致。
func TestDynamicRestartRecovery(t *testing.T) {
	s, path := openTestStore(t)
	active := []MappingIdentity{{"r.example", 1}}
	s.ApplyDynamicSample(DynamicSampleInput{RegistryHost: "r.example", SourceID: 1, OK: true, TTFBMeasurable: true, TTFBMS: 80, TS: 1}, active)
	s.ApplyDynamicSample(DynamicSampleInput{RegistryHost: "r.example", SourceID: 1, OK: false, TTFBMeasurable: false, TS: 2}, active)
	before, _ := s.LoadDynamicState()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s2.LoadDynamicState()
	if err != nil {
		t.Fatalf("重启后加载失败: %v", err)
	}
	if StateHash(before) != StateHash(after) {
		t.Fatalf("重启前后状态不一致: %s vs %s", StateHash(before), StateHash(after))
	}
}

// 并发更新：多协程写样本不丢失（窗口最终 50、修订号单调递增且快照修订一致）。
func TestDynamicConcurrentUpdate(t *testing.T) {
	s, _ := openTestStore(t)
	active := []MappingIdentity{{"r.example", 1}}
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.ApplyDynamicSample(DynamicSampleInput{
				RegistryHost: "r.example", SourceID: 1, OK: i%2 == 0,
				TTFBMeasurable: i%3 != 0, TTFBMS: int64(50 + i), TS: int64(100 + i),
			}, active); err != nil {
				t.Errorf("并发写 %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	st, err := s.LoadDynamicState()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Mappings[0].Samples) != 50 {
		t.Fatalf("并发后窗口应为 50: %d", len(st.Mappings[0].Samples))
	}
	if st.Revision != uint64(60) || st.Revision != st.SnapshotRevision {
		t.Fatalf("修订号应=60且快照一致: rev=%d snap=%d", st.Revision, st.SnapshotRevision)
	}
}

// 映射变更：不再有效的旧映射样本被清除，不迁移给新映射。
func TestDynamicMappingChangeClears(t *testing.T) {
	s, _ := openTestStore(t)
	s.ApplyDynamicSample(DynamicSampleInput{RegistryHost: "old.example", SourceID: 1, OK: true, TTFBMeasurable: true, TTFBMS: 100, TS: 1},
		[]MappingIdentity{{"old.example", 1}})
	st, _ := s.LoadDynamicState()
	if len(st.Mappings) != 1 {
		t.Fatalf("初始应有 1 条映射: %d", len(st.Mappings))
	}
	// 可信关系变化：旧映射不在 active 中，写入新映射
	_, err := s.ApplyDynamicSample(DynamicSampleInput{RegistryHost: "new.example", SourceID: 9, OK: true, TTFBMeasurable: true, TTFBMS: 100, TS: 2},
		[]MappingIdentity{{"new.example", 9}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ = s.LoadDynamicState()
	if len(st.Mappings) != 1 || st.Mappings[0].RegistryHost != "new.example" || st.Mappings[0].SourceID != 9 {
		t.Fatalf("旧映射应被清除、只留新映射: %+v", st.Mappings)
	}
	// 显式 reset 也清除
	if err := s.ResetDynamicStateMapping("new.example", 9); err != nil {
		t.Fatal(err)
	}
	st, _ = s.LoadDynamicState()
	if len(st.Mappings) != 0 || len(st.Snapshot) != 0 {
		t.Fatalf("reset 后应为空: %+v", st)
	}
}

// JSON 损坏：Load 返回错误，不阻断（调用方回退固定顺序）。
func TestDynamicCorruptJSON(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.SetSetting(dynamicStateKey, "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadDynamicState(); err == nil {
		t.Fatal("损坏 JSON 应返回错误")
	}
	//  Repair via sample: 重建空状态，服务不受影响
	st, err := s.ApplyDynamicSample(DynamicSampleInput{RegistryHost: "r", SourceID: 1, OK: true, TTFBMeasurable: true, TTFBMS: 100, TS: 1},
		[]MappingIdentity{{"r", 1}})
	if err != nil || len(st.Mappings) != 1 {
		t.Fatalf("损坏后应能重建: %+v %v", st, err)
	}
}

// 未知状态版本：拒绝使用，不阻断服务。
func TestDynamicUnknownVersion(t *testing.T) {
	s, _ := openTestStore(t)
	bad := DynamicState{Version: 999, FormulaVersion: FormulaVersion, Revision: 1, SnapshotRevision: 1}
	raw, _ := json.Marshal(bad)
	s.SetSetting(dynamicStateKey, string(raw))
	if _, err := s.LoadDynamicState(); err == nil {
		t.Fatal("未知版本应被拒绝")
	}
}

// 原子一致性：快照修订号与样本修订号始终一致（多次写入后校验）。
func TestDynamicAtomicRevision(t *testing.T) {
	s, _ := openTestStore(t)
	active := []MappingIdentity{{"r1", 1}, {"r2", 2}}
	for i := 0; i < 20; i++ {
		host := "r1"
		sid := int64(1)
		if i%2 == 1 {
			host, sid = "r2", 2
		}
		st, err := s.ApplyDynamicSample(DynamicSampleInput{RegistryHost: host, SourceID: sid, OK: true, TTFBMeasurable: true, TTFBMS: int64(100), TS: int64(i)}, active)
		if err != nil {
			t.Fatal(err)
		}
		if st.Revision != st.SnapshotRevision {
			t.Fatalf("修订不一致 @%d: rev=%d snap=%d", i, st.Revision, st.SnapshotRevision)
		}
	}
	if err := validateDynamicState(mustLoad(t, s)); err != nil {
		t.Fatalf("最终状态校验失败: %v", err)
	}
}

func mustLoad(t *testing.T, s *Store) DynamicState {
	t.Helper()
	st, err := s.LoadDynamicState()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

var _ = fmt.Sprintf // 保留 fmt 以备诊断
