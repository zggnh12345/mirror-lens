package store

import "time"

// RecentN 是面板状态条的格数，也是 /api/sources 里每个源的摘要条数。
// 这个常量只在这里写一次：接口把 recent_n 一起下发，前端照它铺格子，
// 不要在前端再硬编码一个 24。
const RecentN = 24

// RecentPoint 是一次探测的摘要。只带画状态条要用的字段，
// 原始错误、digest 这些留在 /api/series 与详情里。
type RecentPoint struct {
	Kind      string `json:"kind"`
	TS        int64  `json:"ts"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
}

// RecentSummaries 一次查完所有源的最近 n 次探测，按 source_id 分桶，
// 桶内按时间升序（左旧右新，前端直接铺格子）。
// 逐源查询在 100+ 个源时就是 100+ 条 SQL，这里用窗口函数一次出结果。
// 时间窗取最近 24 小时：状态条讲的是「最近」，不是全历史，
// 也避免 checks 表变大后每次 /api/sources 都全表排序。
func (s *Store) RecentSummaries(n int) (map[int64][]RecentPoint, error) {
	if n <= 0 {
		n = RecentN
	}
	since := time.Now().Add(-24 * time.Hour).Unix()
	rows, err := s.db.Query(`SELECT source_id,kind,ts,ok,latency_ms FROM (
		SELECT source_id,kind,ts,ok,latency_ms,
		       ROW_NUMBER() OVER (PARTITION BY source_id ORDER BY ts DESC, id DESC) AS rn
		FROM checks WHERE tier>=2 AND ts>=?
	) WHERE rn<=? ORDER BY source_id, ts`, since, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]RecentPoint{}
	for rows.Next() {
		var id int64
		var p RecentPoint
		if err := rows.Scan(&id, &p.Kind, &p.TS, &p.OK, &p.LatencyMS); err != nil {
			return nil, err
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

// SetSiteNameIfEmpty 只在站点名为空、或还是兜底「未知」时写入。
// 发现流程可能后补到真名，但不能覆盖已经落库的展示名。
func (s *Store) SetSiteNameIfEmpty(id int64, name string) error {
	if name == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE sources SET site_name=? WHERE id=? AND (site_name='' OR site_name='未知')`, name, id)
	return err
}
