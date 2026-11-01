package store

import (
	"sort"
	"time"
)

// SourceStat 是调度用的源画像。
// 速度与延迟只取探测样本（checks.tier >= 2）；代理请求不写进 checks，
// 否则「本机到源站」和「用户到本机」两段会混成一个数字。
type SourceStat struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Status  string `json:"status"`
	// Preferred 用户手动设为优先，调度时排在前面。
	Preferred bool  `json:"preferred"`
	Samples   int   `json:"samples"`
	OK        int   `json:"ok"`
	LatencyMS int64 `json:"latency_ms"` // 样本中位数
	SpeedKBps int64 `json:"speed_kbps"` // 样本中位数
	LastTS    int64 `json:"last_ts"`
	// Trust 是这台源在该类型上的可信度级别（见 trust.go）
	Trust int `json:"trust"`
}

const sourceSampleWindow = 10

// HealthFails 某协议族连续失败到这个次数，就把它从该协议的候选里摘掉。
// 只影响这一个协议，源站整体状态和其他协议不受牵连。
const HealthFails = 3

// Family 把请求类型归到三个协议族：docker / github / git。
// raw、release 是 GitHub 内容的另一种地址形态，健康度跟 github 一致。
func Family(kind string) string {
	switch kind {
	case "docker":
		return "docker"
	case "git":
		return "git"
	default:
		return "github"
	}
}

// NoteProbe 记一次协议族探测结果：成功清零并记时间，失败累加。
// 与源站整体 status 分开：那边答「这台还活着吗」，这里答「这个协议还灵吗」。
func (s *Store) NoteProbe(id int64, family string, ok bool) error {
	now := time.Now().Unix()
	if ok {
		_, err := s.db.Exec(`INSERT INTO source_health(source_id, family, fails, ok_at, last_at)
			VALUES(?,?,0,?,?)
			ON CONFLICT(source_id, family) DO UPDATE SET fails=0, ok_at=excluded.ok_at, last_at=excluded.last_at`,
			id, family, now, now)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO source_health(source_id, family, fails, last_at)
		VALUES(?,?,1,?)
		ON CONFLICT(source_id, family) DO UPDATE SET fails=fails+1, last_at=excluded.last_at`,
		id, family, now)
	return err
}

// HealthOf 返回某个源各协议族的连续失败次数，面板/排查用。
func (s *Store) HealthOf(id int64) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT family, fails FROM source_health WHERE source_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var f string
		var n int
		if err := rows.Scan(&f, &n); err != nil {
			return nil, err
		}
		out[f] = n
	}
	return out, rows.Err()
}

// SourceCandidates 返回某类协议当前可参与调度的源。
// 只取 enabled 且 usable/degraded 的，dead 与 pending 不参与。
// 能力按协议分开记：GitHub 前缀代理能代 raw/release，但多数做不了 git
// smart HTTP，git 用单独的 git_cap。
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
	// 按协议族过滤：docker 挂了不能拖累 github，反之亦然。
	// 源整体可用但某协议连续失败时，只摘掉这个协议。
	rows, err := s.db.Query(`SELECT id, name, base_url, status, preferred FROM sources
		WHERE enabled = 1 AND status IN ('usable','degraded') AND `+col+` = 1
		  AND id NOT IN (SELECT source_id FROM source_health WHERE family = ? AND fails >= ?)
		ORDER BY id`, Family(kind), HealthFails)
	if err != nil {
		return nil, err
	}
	var out []SourceStat
	for rows.Next() {
		var st SourceStat
		if err := rows.Scan(&st.ID, &st.Name, &st.BaseURL, &st.Status, &st.Preferred); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	tm, err := s.trustMap(kind, time.Now())
	if err != nil {
		return nil, err
	}
	for i := range out {
		n, ok, lat, spd, last, err := s.sourceSamples(out[i].ID, kind)
		if err != nil {
			return nil, err
		}
		out[i].Samples, out[i].OK, out[i].LatencyMS, out[i].SpeedKBps, out[i].LastTS = n, ok, lat, spd, last
		out[i].Trust = tm[out[i].ID].Level
	}
	return out, nil
}

// sourceSamples 取最近 N 条样本，返回中位延迟与中位速度。
// 中位数比平均值稳：一次 25 秒超时不至于把整条源的分数拉到底。
// 按 kind 隔离；本协议无样本时只退回无 kind 的老行，不退回别的协议，
// 否则只有 docker 样本的源查 raw 排名时会被 docker 数据带偏。
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

// SetGitCap 记录该源确实支持 git 协议。
// 只在实测成功时调用：能取 raw 文件不代表实现了 git smart HTTP。
func (s *Store) SetGitCap(id int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE sources SET git_cap=? WHERE id=?`, v, id)
	return err
}

// SourceFailedRecently 返回「最近一次代理失败、且在 since 之后」的源。
// 熔断状态落库而非只放内存：进程重启后不该再拿同一个坏源等超时。
// 只看每个源的最近一条事件，成功即自动解除。
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
// 只用于熔断与运维观察，不参与速度评分。
func (s *Store) RecordSourceEvent(sourceID int64, kind string, ok bool, ms int64, code int) {
	if sourceID == 0 {
		return
	}
	b := 0
	if ok {
		b = 1
	}
	// 事件表没有外键，源删掉后迟到的上报会把它重新插回来。
	// 用 INSERT ... SELECT ... WHERE EXISTS 把「源还在」交给数据库判断，
	// 堵住调用方检查到真正写入之间的空隙。
	_, _ = s.db.Exec(`INSERT INTO source_events(source_id, kind, ts, ok, ms, code)
		SELECT ?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM sources WHERE id=?)`,
		sourceID, kind, time.Now().Unix(), b, ms, code, sourceID)
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
