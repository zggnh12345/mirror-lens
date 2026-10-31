package store

import (
	"sort"
	"time"
)

// SourceStat 是调度用的源画像。
//
// 速度与延迟只来自探测样本（checks.tier >= 2）；代理请求的快慢不写进
// checks，否则「本机到源站」和「用户到本机」两段会混成一个数字。
type SourceStat struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Status    string `json:"status"`
	Samples   int    `json:"samples"`
	OK        int    `json:"ok"`
	LatencyMS int64  `json:"latency_ms"` // 样本中位数
	SpeedKBps int64  `json:"speed_kbps"` // 样本中位数
	LastTS    int64  `json:"last_ts"`
}

const sourceSampleWindow = 10

// SourceCandidates 返回某类协议当前可参与调度的源。
// 只取 enabled 且状态为 usable/degraded 的；dead 与 pending 不参与。
//
// 能力按协议分开记：GitHub 前缀代理能代理 raw/release，但多数做不了 git
// smart HTTP。git 用单独的 git_cap，默认关，靠实测或手动打开。
func (s *Store) SourceCandidates(kind string) ([]SourceStat, error) {
	col := ""
	switch kind {
	case "docker":
		col = "docker_cap"
	case "git":
		col = "git_cap"
	case "raw", "release", "github":
		col = "github_cap"
	default:
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT id, name, base_url, status FROM sources
		WHERE enabled = 1 AND status IN ('usable','degraded') AND ` + col + ` = 1
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []SourceStat
	for rows.Next() {
		var st SourceStat
		if err := rows.Scan(&st.ID, &st.Name, &st.BaseURL, &st.Status); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		n, ok, lat, spd, last, err := s.sourceSamples(out[i].ID, kind)
		if err != nil {
			return nil, err
		}
		out[i].Samples, out[i].OK, out[i].LatencyMS, out[i].SpeedKBps, out[i].LastTS = n, ok, lat, spd, last
	}
	return out, nil
}

// sourceSamples 取最近 N 条样本，返回中位延迟与中位速度。
// 中位数比平均值稳：一次 25 秒的超时不会把整条源的分数拉到底。
//
// 按 kind 隔离：一台源做 docker 很快、做 raw 全挂，混在一起取中位数两边都会失真。
// 本协议还没有样本时只退回「没有 kind 的历史行」（升级前的老库），不退回别的协议：
// 否则一台源只有 docker 样本，查 raw 排名时又会被 docker 的数据带偏，隔离等于没做。
func (s *Store) sourceSamples(id int64, kind string) (n, ok int, lat, spd, last int64, err error) {
	n, ok, lat, spd, last, err = s.samples(id, kind, false)
	if err == nil && n == 0 && kind != "" {
		return s.samples(id, "", true)
	}
	return n, ok, lat, spd, last, err
}

// samples 取样本统计。legacy=true 时只取没有 kind 的历史行。
func (s *Store) samples(id int64, kind string, legacy bool) (n, ok int, lat, spd, last int64, err error) {
	q := `SELECT ts, ok, latency_ms, speed_kbps FROM checks
		WHERE source_id = ? AND tier >= 2`
	args := []any{id}
	switch {
	case legacy:
		q += ` AND (kind IS NULL OR kind = '')`
	case kind != "":
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, sourceSampleWindow)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	var lats, spds []int64
	for rows.Next() {
		var ts int64
		var o, l, sp int64
		if err = rows.Scan(&ts, &o, &l, &sp); err != nil {
			return
		}
		n++
		if o == 1 {
			ok++
			if l > 0 {
				lats = append(lats, l)
			}
			if sp > 0 {
				spds = append(spds, sp)
			}
		}
		if ts > last {
			last = ts
		}
	}
	err = rows.Err()
	lat = median(lats)
	spd = median(spds)
	return
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[len(v)/2]
}

// SetGitCap 记录「这个源确实能做 git 协议」。
//
// 只在实测成功时调用：前缀代理能取 raw 文件，不代表它实现了 git smart HTTP。
func (s *Store) SetGitCap(id int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE sources SET git_cap=? WHERE id=?`, v, id)
	return err
}

// SourceFailedRecently 返回「最近一次代理结果就是失败、且发生在 since 之后」的源。
//
// 熔断状态落在库里而不是只放内存：进程重启后不该再拿同一个坏源去等超时。
// 只看每个源的最近一条事件，所以它一旦成功就自动解除。
func (s *Store) SourceFailedRecently(kind string, since int64) (map[int64]bool, error) {
	rows, err := s.db.Query(`
		SELECT e.source_id FROM source_events e
		JOIN (SELECT source_id, MAX(id) AS mid FROM source_events WHERE kind=? GROUP BY source_id) m
		  ON m.mid = e.id
		WHERE e.ok=0 AND e.ts>=?`, kind, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RecordSourceEvent 记录一次「代理请求打到某个源」的结果。
// 这张表只用于熔断和运维观察，不参与速度评分。
func (s *Store) RecordSourceEvent(sourceID int64, kind string, ok bool, ms int64, code int) {
	if sourceID == 0 {
		return
	}
	b := 0
	if ok {
		b = 1
	}
	_, _ = s.db.Exec(`INSERT INTO source_events(source_id, kind, ts, ok, ms, code) VALUES(?,?,?,?,?,?)`,
		sourceID, kind, time.Now().Unix(), b, ms, code)
}

// SourceEventCodes 返回某源最近事件的状态码，新的在前。排障和测试用。
func (s *Store) SourceEventCodes(sourceID int64, limit int) ([]int, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT code FROM source_events WHERE source_id=? ORDER BY id DESC LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var c int
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneSourceEvents 只保留最近 7 天，避免事件表无限长。
func (s *Store) PruneSourceEvents() {
	cut := time.Now().AddDate(0, 0, -7).Unix()
	_, _ = s.db.Exec(`DELETE FROM source_events WHERE ts < ?`, cut)
}

// SourceEvents 返回某个源最近的事件，供面板/排障使用。
func (s *Store) SourceEvents(sourceID int64, limit int) (ok, fail int, lastErrAt int64, err error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT ts, ok FROM source_events WHERE source_id = ? ORDER BY ts DESC LIMIT ?`, sourceID, limit)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var o int
		if err = rows.Scan(&ts, &o); err != nil {
			return
		}
		if o == 1 {
			ok++
		} else {
			fail++
			if lastErrAt == 0 {
				lastErrAt = ts
			}
		}
	}
	err = rows.Err()
	return
}
