package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 3.5.18 18c：动态排序样本与排名快照的持久化内部状态。
//
// 载体：settings 表单一键 cross_mapping_dynamic_state，value 是 JSON。
// 这是有界运行统计状态（每条映射最多 50 个样本 + 派生排名快照），不是用户配置项：
// 不暴露给 Web API、不进静态配置导出、不进 Canonical 配置 Hash、不新增表、不改 Schema。
//
// 原子一致：样本窗口与排名快照属于同一 Revision。完整读改写由进程内 mutex +
// 数据库事务保护，避免并发请求读改写覆盖；写失败不影响当前下载请求，
// 也不发布未持久化的排名。

const dynamicStateKey = "cross_mapping_dynamic_state"

// DynamicStateVersion 是 JSON 状态格式版本；不兼容变更必须 bump，
// 旧版本状态被拒绝使用并触发回退固定顺序（不猜测恢复）。
const DynamicStateVersion = 1

const (
	dynamicSampleWindow = 50 // W1：每条映射最近 50 个有效样本
	dynamicTTFBBaseMS   = 800.0
	dynamicWeightSR     = 0.6
	dynamicWeightLS     = 0.4
)

// DynamicSampleWindow 返回冻结的样本窗口容量 W1=50。
func DynamicSampleWindow() int { return dynamicSampleWindow }

// FormulaVersion 是冻结评分公式版本常量（避免 F02 编号复用歧义）。
const FormulaVersion = "B-DYNAMIC-SCORE-FORMULA"

// DynamicSample 是一条有效映射回源样本。
// TTFBMeasurable=false 表示无响应头（超时等），只进成功率分母、不计入 P50。
// Success=true 还要求该样本已通过 Manifest 身份验证（由采集方保证）。
type DynamicSample struct {
	TS             int64 `json:"ts"`
	OK             bool  `json:"ok"`
	TTFBMS         int64 `json:"ttfb_ms"`         // 可测量时 >0
	TTFBMeasurable bool  `json:"ttfb_measurable"` // 是否收到响应头
}

// MappingState 是一条可信映射的样本窗口。身份 = RegistryHost + SourceID。
type MappingState struct {
	RegistryHost string          `json:"registry_host"`
	SourceID     int64           `json:"source_id"`
	Samples      []DynamicSample `json:"samples"` // 最多 50，按 TS 升序（最旧在前）
}

// SnapshotEntry 是排名快照中的一条派生结果。
type SnapshotEntry struct {
	RegistryHost string  `json:"registry_host"`
	SourceID     int64   `json:"source_id"`
	ValidSamples int     `json:"valid_samples"`
	SuccessRate  float64 `json:"success_rate"`
	P50TTFBMS    float64 `json:"p50_ttfb_ms"`
	LatencyScore float64 `json:"latency_score"`
	Score        float64 `json:"score"`
}

// DynamicState 是持久化的完整内部状态。Revision 同时标识样本与快照，
// 二者必须属于同一修订；SnapshotRevision 单独保存用于启动一致性校验。
type DynamicState struct {
	Version          int             `json:"version"`
	Revision         uint64          `json:"revision"`
	SnapshotRevision uint64          `json:"snapshot_revision"`
	FormulaVersion   string          `json:"formula_version"`
	UpdatedAt        int64           `json:"updated_at"`
	Mappings         []MappingState  `json:"mappings"`
	Snapshot         []SnapshotEntry `json:"snapshot"`
}

// dynamicStateMu 串行化 settings 内部状态的完整读改写。
// 多进程场景由 SQLite 事务的写锁进一步保护（同事务内 SELECT+UPSERT）。
var dynamicStateMu sync.Mutex

// MappingIdentity 是一条可信映射的身份（RegistryHost + SourceID），导出给
// gateway 构造有效映射集合。映射身份或可信关系变化即以此为准清零。
type MappingIdentity struct {
	Host string
	SID  int64
}

func keyOf(m MappingState) MappingIdentity { return MappingIdentity{m.RegistryHost, m.SourceID} }

// LoadDynamicState 读取并校验持久化状态。任何不一致都返回 error，
// 调用方据此退回固定顺序，不使用未验证的动态排名。
func (s *Store) LoadDynamicState() (DynamicState, error) {
	var zero DynamicState
	raw := s.GetSetting(dynamicStateKey, "")
	if raw == "" {
		return zero, errors.New("dynamic state 未初始化")
	}
	var st DynamicState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return zero, fmt.Errorf("dynamic state JSON 损坏: %w", err)
	}
	if err := validateDynamicState(st); err != nil {
		return zero, err
	}
	return st, nil
}

// validateDynamicState 校验版本、公式、容量与修订一致性。
func validateDynamicState(st DynamicState) error {
	if st.Version != DynamicStateVersion {
		return fmt.Errorf("dynamic state 版本不兼容: %d（需要 %d）", st.Version, DynamicStateVersion)
	}
	if st.FormulaVersion != FormulaVersion {
		return fmt.Errorf("dynamic state 公式版本不兼容: %q（需要 %q）", st.FormulaVersion, FormulaVersion)
	}
	if st.Revision != st.SnapshotRevision {
		return fmt.Errorf("dynamic state 修订不一致: 样本=%d 快照=%d", st.Revision, st.SnapshotRevision)
	}
	seen := map[MappingIdentity]bool{}
	for _, m := range st.Mappings {
		k := keyOf(m)
		if seen[k] {
			return fmt.Errorf("dynamic state 映射重复: %s/%d", k.Host, k.SID)
		}
		seen[k] = true
		if len(m.Samples) > dynamicSampleWindow {
			return fmt.Errorf("dynamic state 映射 %s/%d 样本窗口超限: %d", k.Host, k.SID, len(m.Samples))
		}
		for _, sm := range m.Samples {
			if sm.TTFBMeasurable && sm.TTFBMS < 0 {
				return fmt.Errorf("dynamic state 样本 TTFB 非法: %d", sm.TTFBMS)
			}
		}
	}
	for _, e := range st.Snapshot {
		if !seen[MappingIdentity{e.RegistryHost, e.SourceID}] {
			return fmt.Errorf("dynamic state 快照引用未知映射: %s/%d", e.RegistryHost, e.SourceID)
		}
		if e.ValidSamples < 0 || e.ValidSamples > dynamicSampleWindow {
			return fmt.Errorf("dynamic state 快照样本数非法: %d", e.ValidSamples)
		}
	}
	return nil
}

// DynamicStateView 是 Web 展示用的只读动态排序视图。
// 只暴露派生的统计与排名，不暴露内部 JSON 状态键、不参与任何配置导出。
type DynamicStateView struct {
	Enabled        bool               `json:"enabled"`
	FormulaVersion string             `json:"formula_version"`
	Revision       uint64             `json:"revision"`
	UpdatedAt      int64              `json:"updated_at"`
	Reason         string             `json:"reason,omitempty"`
	Entries        []DynamicViewEntry `json:"entries"`
}

type DynamicViewEntry struct {
	RegistryHost string  `json:"registry_host"`
	SourceID     int64   `json:"source_id"`
	ValidSamples int     `json:"valid_samples"`
	Ready        bool    `json:"ready"`
	SuccessRate  float64 `json:"success_rate"`
	P50TTFBMS    float64 `json:"p50_ttfb_ms"`
	LatencyScore float64 `json:"latency_score"`
	Score        float64 `json:"score"`
	Rank         int     `json:"rank"`
}

// DynamicStateView 构造 Web 展示视图。状态缺失/损坏时返回带 reason 的空视图，
// 不阻断 API，也不猜测恢复数据。
func (s *Store) DynamicStateView(sortMode string) DynamicStateView {
	v := DynamicStateView{Enabled: sortMode == "dynamic", FormulaVersion: FormulaVersion}
	if v.Enabled == false {
		v.Reason = "动态排序未开启"
		return v
	}
	st, err := s.LoadDynamicState()
	if err != nil {
		v.Reason = "动态状态不可用：" + err.Error()
		return v
	}
	v.Revision = st.Revision
	v.UpdatedAt = st.UpdatedAt
	for i, e := range st.Snapshot {
		v.Entries = append(v.Entries, DynamicViewEntry{
			RegistryHost: e.RegistryHost, SourceID: e.SourceID,
			ValidSamples: e.ValidSamples, Ready: e.ValidSamples >= dynamicSampleWindow,
			SuccessRate: e.SuccessRate, P50TTFBMS: e.P50TTFBMS,
			LatencyScore: e.LatencyScore, Score: e.Score, Rank: i + 1,
		})
	}
	if len(v.Entries) == 0 {
		v.Reason = "尚无动态样本"
	}
	return v
}

// ActiveMappingIdentities 返回当前所有有效可信映射的身份集合。
// 供 gateway 采集样本时构造 active 集合：不在其中的旧映射状态会被清零与清理。
func (s *Store) ActiveMappingIdentities(now time.Time) []MappingIdentity {
	all, err := s.ListTrustedMappings("")
	if err != nil {
		return nil
	}
	out := make([]MappingIdentity, 0, len(all))
	for _, m := range all {
		if m.ActiveAt(now) {
			out = append(out, MappingIdentity{Host: m.RegistryHost, SID: m.SourceID})
		}
	}
	return out
}

// DynamicSampleInput 是一次真实回源尝试采集到的样本输入。
type DynamicSampleInput struct {
	RegistryHost   string
	SourceID       int64
	OK             bool // 已通过 Manifest 身份验证
	TTFBMS         int64
	TTFBMeasurable bool // 收到响应头
	TS             int64
}

// ApplyDynamicSample 原子地：载入状态→追加样本→裁剪窗口→重算快照→增修订→事务写回。
// 写回失败返回错误，调用方应保留最近一次成功持久化的状态，且不把本次样本当作已提交。
// active 是当前有效映射身份集合；不在其中的旧映射状态被清理（映射变化清零 + 防止无限累积）。
func (s *Store) ApplyDynamicSample(in DynamicSampleInput, active []MappingIdentity) (DynamicState, error) {
	dynamicStateMu.Lock()
	defer dynamicStateMu.Unlock()

	st, err := s.LoadDynamicState()
	if err != nil {
		// 损坏/缺失：从空状态重建，不猜测恢复历史样本。
		st = DynamicState{
			Version:        DynamicStateVersion,
			FormulaVersion: FormulaVersion,
		}
	}

	activeSet := make(map[MappingIdentity]bool, len(active))
	for _, k := range active {
		activeSet[k] = true
	}

	// 清理失效映射；仅保留当前有效映射。
	kept := make([]MappingState, 0, len(st.Mappings))
	for _, m := range st.Mappings {
		if activeSet[keyOf(m)] {
			kept = append(kept, m)
		}
	}
	st.Mappings = kept

	if in.RegistryHost == "" || in.SourceID <= 0 {
		return st, fmt.Errorf("dynamic sample 映射身份非法: %s/%d", in.RegistryHost, in.SourceID)
	}
	if !activeSet[MappingIdentity{in.RegistryHost, in.SourceID}] {
		return st, fmt.Errorf("dynamic sample 映射不在有效集合: %s/%d", in.RegistryHost, in.SourceID)
	}

	found := false
	for i := range st.Mappings {
		if st.Mappings[i].RegistryHost == in.RegistryHost && st.Mappings[i].SourceID == in.SourceID {
			st.Mappings[i].Samples = appendWindow(st.Mappings[i].Samples, DynamicSample{
				TS: in.TS, OK: in.OK, TTFBMS: in.TTFBMS, TTFBMeasurable: in.TTFBMeasurable,
			})
			found = true
			break
		}
	}
	if !found {
		st.Mappings = append(st.Mappings, MappingState{
			RegistryHost: in.RegistryHost,
			SourceID:     in.SourceID,
			Samples: appendWindow(nil, DynamicSample{
				TS: in.TS, OK: in.OK, TTFBMS: in.TTFBMS, TTFBMeasurable: in.TTFBMeasurable,
			}),
		})
	}

	st.Snapshot = computeSnapshot(st.Mappings)
	st.Revision++
	st.SnapshotRevision = st.Revision
	st.UpdatedAt = in.TS

	raw, err := json.Marshal(st)
	if err != nil {
		return st, err
	}
	if err := s.Tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, dynamicStateKey, string(raw))
		return err
	}); err != nil {
		return st, fmt.Errorf("持久化 dynamic state 失败: %w", err)
	}
	return st, nil
}

// ResetDynamicStateMapping 清除指定映射的历史样本与快照（映射身份/可信关系变化时调用）。
func (s *Store) ResetDynamicStateMapping(host string, sid int64) error {
	dynamicStateMu.Lock()
	defer dynamicStateMu.Unlock()

	st, err := s.LoadDynamicState()
	if err != nil {
		return nil // 无有效状态则无需清理
	}
	kept := st.Mappings[:0]
	for _, m := range st.Mappings {
		if m.RegistryHost == host && m.SourceID == sid {
			continue
		}
		kept = append(kept, m)
	}
	st.Mappings = kept
	st.Snapshot = computeSnapshot(st.Mappings)
	st.Revision++
	st.SnapshotRevision = st.Revision
	st.UpdatedAt = time.Now().Unix()
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.Tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, dynamicStateKey, string(raw))
		return err
	})
}

// appendWindow 追加一个样本并保持窗口 ≤50（最旧移出）。
func appendWindow(samples []DynamicSample, sm DynamicSample) []DynamicSample {
	if len(samples) >= dynamicSampleWindow {
		copy(samples, samples[1:])
		samples[len(samples)-1] = sm
		return samples
	}
	return append(samples, sm)
}

// ComputeScores 从映射样本计算每条映射的评分（导出给 gateway 与测试）。
func ComputeScores(mappings []MappingState) []SnapshotEntry {
	return computeSnapshot(mappings)
}

// ScoreOf 对单条映射计算评分。不足 50 样本返回 valid 供调用方决定回退。
func ScoreOf(m MappingState) (SnapshotEntry, bool) {
	n := len(m.Samples)
	e := SnapshotEntry{
		RegistryHost: m.RegistryHost, SourceID: m.SourceID, ValidSamples: n,
	}
	if n == 0 {
		return e, false
	}
	var succ int
	ttfbs := make([]int64, 0, n)
	for _, sm := range m.Samples {
		if sm.OK {
			succ++
		}
		if sm.TTFBMeasurable {
			ttfbs = append(ttfbs, sm.TTFBMS)
		}
	}
	e.SuccessRate = float64(succ) / float64(n)
	if len(ttfbs) > 0 {
		sort.Slice(ttfbs, func(i, j int) bool { return ttfbs[i] < ttfbs[j] })
		e.P50TTFBMS = percentile50(ttfbs)
		e.LatencyScore = 1.0 / (1.0 + e.P50TTFBMS/dynamicTTFBBaseMS)
	}
	// 无可测 TTFB → LatencyScore 保持 0。
	e.Score = dynamicWeightSR*e.SuccessRate + dynamicWeightLS*e.LatencyScore
	return e, n >= dynamicSampleWindow
}

// percentile50 对已排序切片取中位数（偶数取两中间值平均）。
func percentile50(sorted []int64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return float64(sorted[n/2])
	}
	return float64(sorted[n/2-1]+sorted[n/2]) / 2.0
}

// computeSnapshot 计算全部映射的评分快照，按 Score 降序、并列按固定顺序
// （RegistryHost、SourceID、Priority 已由调用顺序稳定）→ 这里按 host/sid 稳定兜底。
func computeSnapshot(mappings []MappingState) []SnapshotEntry {
	out := make([]SnapshotEntry, 0, len(mappings))
	for _, m := range mappings {
		e, _ := ScoreOf(m)
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		// 并列：固定顺序兜底，不随机（按映射身份字典序/数值）。
		if out[i].RegistryHost != out[j].RegistryHost {
			return out[i].RegistryHost < out[j].RegistryHost
		}
		return out[i].SourceID < out[j].SourceID
	})
	return out
}

// StateHash 供测试/审计对当前状态做内容寻址校验（不进 Canonical 配置 Hash）。
func StateHash(st DynamicState) string {
	raw, _ := json.Marshal(st)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
