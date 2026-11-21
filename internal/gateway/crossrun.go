package gateway

import (
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// CrossRunState 是一次跨 Registry 调度的运行时可观测快照（18f）。
// 只保存低基数的判定字段：Registry host、SourceID、Digest、结果分类；
// 不保存 URL / Authorization / Token / 查询串，避免凭据泄漏与高基数标签。
type CrossRunState struct {
	Seq                 uint64    `json:"seq"`                  // 进程内递增序号，关联一次逻辑请求
	At                  time.Time `json:"at"`                   // 结论时间
	Mode                string    `json:"mode"`                 // sequential / race
	Registry            string    `json:"registry"`             // 胜者所在逻辑 Registry（低基数）
	WinnerID            int64     `json:"winner_id"`            // 胜者 SourceID（0=权威兜底）
	Digest              string    `json:"digest"`               // 已校验摘要（内容寻址，低基数）
	Candidates          int       `json:"candidates"`           // 参与候选数（同 Registry 多 Source 计 1）
	Sources             int       `json:"sources"`              // 内层实际尝试的 Source 数（A+B 嵌套）
	Authority           bool      `json:"authority"`            // 本次是否真的问了官方权威
	CachePut            string    `json:"cache_put"`            // committed / rejected / skipped
	Outcome             string    `json:"outcome"`              // ok / fallback / fail
	VoteState           string    `json:"vote_state"`           // 投票结论：confirmed/mismatch/authority_compare/window_closed/collecting
	Backfill            int       `json:"backfill"`             // 真实失败补位次数（首轮 C 之外的补位回源）
	TruncatedCandidates int       `json:"truncated_candidates"` // 被 M 截断的候选数（诊断，不等于补位）
	Canceled            int       `json:"canceled"`             // 被取消的候选/Source 数
	Nested              bool      `json:"nested"`               // 是否触发同 Registry 内 A 站内竞速
	Generation          uint64    `json:"generation"`           // 配置代次（关联审计）
	RequestHint         string    `json:"request_hint"`         // 脱敏请求关联键（cache_key 的摘要，不含查询串）
}

// crossRunRing 是固定容量的运行时状态环形缓冲。
// 容量固定 → 内存有界、指标/API 基数有界；新状态覆盖最旧状态。
type crossRunRing struct {
	mu    sync.RWMutex
	buf   []CrossRunState
	head  int
	count int
	seq   uint64
}

func newCrossRunRing(capacity int) *crossRunRing {
	if capacity <= 0 {
		capacity = 128
	}
	return &crossRunRing{buf: make([]CrossRunState, capacity)}
}

func (r *crossRunRing) add(s CrossRunState) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	s.Seq = r.seq
	if r.count < len(r.buf) {
		r.buf[(r.head+r.count)%len(r.buf)] = s
		r.count++
	} else {
		r.buf[r.head] = s
		r.head = (r.head + 1) % len(r.buf)
	}
}

// snapshot 返回最新在前的有界快照。
func (r *crossRunRing) snapshot(limit int) []CrossRunState {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > r.count {
		limit = r.count
	}
	out := make([]CrossRunState, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (r.head + r.count - 1 - i + len(r.buf)) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

// recordCrossRun 记录一次跨站调度结论；ring 为 nil（测试用 &Gateway{}）时静默跳过。
func (g *Gateway) recordCrossRun(s CrossRunState) {
	if g == nil || g.crossRuns == nil {
		return
	}
	if s.At.IsZero() {
		s.At = time.Now()
	}
	g.crossRuns.add(s)
	// API 环形缓冲与指标用同一份状态更新，保证二者一致（18f 验收）。
	metrics.CrossRun(s.Mode, s.Outcome, s.VoteState)
}

// CrossRunSnapshot 返回最近的跨站运行时状态（最新在前），供 API/Web 查询。
func (g *Gateway) CrossRunSnapshot(limit int) []CrossRunState {
	if g == nil || g.crossRuns == nil {
		return []CrossRunState{}
	}
	return g.crossRuns.snapshot(limit)
}

// sanitizeHint 把 cache_key 折成不含查询串、不含凭据的短关联键。
func sanitizeHint(cacheKey string) string {
	if i := strings.IndexByte(cacheKey, '?'); i >= 0 {
		cacheKey = cacheKey[:i]
	}
	// 只保留 key 主体，去掉可能携带 token 的 query；长度上限防止高基数膨胀。
	const max = 120
	if len(cacheKey) > max {
		cacheKey = cacheKey[:max]
	}
	return cacheKey
}
