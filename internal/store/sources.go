package store

import (
	"database/sql"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// minProxySpeedBytes 是单条真实吞吐样本的最小字节数。
// 与 gateway.minSpeedSample 必须一致：写入门槛 16MiB、读门槛 64MiB 时，
// 16–64MiB 的样本永远读不到，等于白记。
const minProxySpeedBytes = 16 << 20

// sampleKind 把「实际请求协议」映射到「样本族」。
//
// 探针只覆盖 docker/ghcr/github 三类（runner.allKinds），而 raw 和 release
// 都是走 GitHub 族代理取内容，能力判断也共用 github_cap。如果不映射，
// 这两条路径永远查不到样本，Samples=0 时评分退化成 0.5 先验，等于不参与排序。
//
// git 不映射：它有自己的 git_cap，且 github 探针测的是 4MB raw 抓取，
// 与 git smart HTTP 的吞吐口径不同，借用会污染解释。
func sampleKind(kind string) string {
	// Registry 的每个 Host 是独立调度族，但探测样本按「registry」共用一个族：
	// 样本只反映这台源到 Registry Host 的链路质量，分 host 反而让新 Host 没有先验。
	if strings.HasPrefix(kind, "registry:") {
		return "registry"
	}
	switch kind {
	case "raw", "release":
		return "github"
	default:
		return kind
	}
}

// proxySpeedMaxAgeSec 是真实速度样本的绝对寿命。72h 半衰期只是相对权重，
// 加权平均会把共同衰减约掉，7 天前的样本照样能算出速度，所以必须有硬期限。
const proxySpeedMaxAgeSec = 7 * 24 * 3600

// SourceStat 是调度用的源画像。
// 速度与延迟只取探测样本（checks.tier >= 2）；代理请求不写进 checks，
// 否则「本机到源站」和「用户到本机」两段会混成一个数字。
type SourceStat struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Status  string `json:"status"`
	// Preferred 用户手动设为优先，调度时排在前面。
	Preferred bool `json:"preferred"`
	// GroupRank 所属分组的优先级，未分组给 999（排在所有组之后）。
	GroupRank int `json:"group_rank"`
	// GhcrPrefix 代 ghcr 时是否用带 ghcr.io 前缀的路径格式（见 Source.GhcrPrefix）。
	GhcrPrefix bool  `json:"ghcr_prefix"`
	Samples    int   `json:"samples"`
	OK         int   `json:"ok"`
	LatencyMS  int64 `json:"latency_ms"` // 样本中位数
	SpeedKBps  int64 `json:"speed_kbps"` // 样本中位数
	LastTS     int64 `json:"last_ts"`
	// Trust 是这台源在该类型上的可信度级别（见 trust.go）
	Trust int `json:"trust"`
	// RangeCap 是最近一次 Range 预检结论：1 支持，0 不支持，-1 没测过。
	// RangeAt 是那次预检的时间戳，过期要重测。
	RangeCap int   `json:"range_cap"`
	RangeAt  int64 `json:"range_at"`
	// RegistryPath 是该源对 RepoHost 已验证的路径模式（native | host_prefix）。
	// 只有 registry:<host> 查询会填；调度时与 RegistryCap 一起作为不可拆分的选路依据。
	RegistryPath string `json:"registry_path_mode,omitempty"`
}

const sourceSampleWindow = 10

// HealthFails 某协议族连续失败到这个次数，就把它从该协议的候选里摘掉。
// 只影响这一个协议，源站整体状态和其他协议不受牵连。
const HealthFails = 3

// Family 把请求类型归到协议族：docker / ghcr / github / git。
// raw、release 是 GitHub 内容的另一种地址形态，健康度跟 github 一致。
func Family(kind string) string {
	// Registry 是独立协议族：既不能记进 docker，也不能落进 default 的 github。
	if strings.HasPrefix(kind, "registry:") {
		return "registry"
	}
	switch kind {
	case "registry":
		return "registry"
	case "docker":
		return "docker"
	case "ghcr":
		return "ghcr"
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

// LastProbeAt 返回某协议族上次探测（NoteProbe）的时间戳，没探测过返回 0。
// 用于 CapNo 协议降频重探：判死可能是探测格式过时导致的误判，隔一段时间
// 再试一次，误判的能爬回来，真不支持的仍会 404 快速返回。
func (s *Store) LastProbeAt(id int64, family string) (int64, error) {
	var at int64
	err := s.db.QueryRow(`SELECT last_at FROM source_health WHERE source_id=? AND family=?`,
		id, family).Scan(&at)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return at, err
}

// SourceCandidates 返回某类协议当前可参与调度的源。
// 只取 enabled 且 usable/degraded 的，dead 与 pending 不参与。
// 能力按协议分开记：GitHub 前缀代理能代 raw/release，但多数做不了 git
// smart HTTP，git 用单独的 git_cap。
func (s *Store) SourceCandidates(kind string) ([]SourceStat, error) {
	// registry:<host> 是独立调度族：只认 registry_cap 与已验证的路径模式，
	// 绝不借用 docker_cap/ghcr_cap 间接放行。
	if raw, ok := strings.CutPrefix(kind, "registry:"); ok {
		host, err := registryhost.Check(raw)
		if err != nil {
			return nil, nil
		}
		return s.registryCandidates(host)
	}
	col := ""
	switch kind {
	case "docker":
		col = "docker_cap"
	case "ghcr":
		col = "ghcr_cap"
	case "git":
		col = "git_cap"
	case "raw", "release", "github":
		col = "github_cap"
	default:
		return nil, nil
	}
	// 按协议族过滤：docker 挂了不能拖累 github，反之亦然。
	// 源整体可用但某协议连续失败时，只摘掉这个协议。
	rows, err := s.db.Query(`SELECT s.id, s.name, s.base_url, s.status, s.preferred,
			s.ghcr_prefix, COALESCE(g.rank, 999), s.range_cap, s.range_at
		FROM sources s LEFT JOIN source_groups g ON g.id = s.group_id
		WHERE s.enabled = 1 AND s.status IN ('usable','degraded') AND s.`+col+` = 1
		  AND s.id NOT IN (SELECT source_id FROM source_health WHERE family = ? AND fails >= ?)
		ORDER BY s.id`, Family(kind), HealthFails)
	if err != nil {
		return nil, err
	}
	var out []SourceStat
	for rows.Next() {
		var st SourceStat
		if err := rows.Scan(&st.ID, &st.Name, &st.BaseURL, &st.Status, &st.Preferred, &st.GhcrPrefix, &st.GroupRank, &st.RangeCap, &st.RangeAt); err != nil {
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

// registryCandidates 返回某个 Registry Host 下通过验证的源。
//
// 选路依据是不可拆分的一组条件：cap=1（已验证）+ 已知路径模式 + Host 精确匹配。
// cap=2（内容不一致）、-1（未知）、0（不支持）都不能进候选；空路径也不进，
// 否则调度会拿到一个不知道该怎么拼 URL 的候选。
func (s *Store) registryCandidates(host string) ([]SourceStat, error) {
	rows, err := s.db.Query(`SELECT s.id, s.name, s.base_url, s.status, s.preferred,
			s.ghcr_prefix, COALESCE(g.rank, 999), s.range_cap, s.range_at, s.registry_path_mode
		FROM sources s LEFT JOIN source_groups g ON g.id = s.group_id
		WHERE s.enabled = 1 AND s.status IN ('usable','degraded')
		  AND s.registry_cap = 1 AND s.repo_host = ?
		  AND s.registry_path_mode IN ('native','host_prefix')
		  AND s.id NOT IN (SELECT source_id FROM source_health WHERE family = 'registry' AND fails >= ?)
		ORDER BY s.id`, host, HealthFails)
	if err != nil {
		return nil, err
	}
	var out []SourceStat
	for rows.Next() {
		var st SourceStat
		if err := rows.Scan(&st.ID, &st.Name, &st.BaseURL, &st.Status, &st.Preferred,
			&st.GhcrPrefix, &st.GroupRank, &st.RangeCap, &st.RangeAt, &st.RegistryPath); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	kind := "registry:" + host
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

// sourceSamples 取最近 N 条样本。
// 中位数比平均值稳：一次 25 秒超时不至于把整条源的分数拉到底。
// 按 kind 隔离；本协议无样本时只退回无 kind 的老行，不退回别的协议，
// 否则只有 docker 样本的源查 raw 排名时会被 docker 数据带偏。
func (s *Store) sourceSamples(id int64, kind string) (n, ok int, lat, spd, last int64, err error) {
	sk := sampleKind(kind)
	n, ok, lat, spd, last, err = s.samples(id, sk, false)
	// 本协议没样本时才整体退回无 kind 的老行；不和新数据合并，避免重复计入。
	if err == nil && n == 0 && sk != "" {
		return s.samples(id, "", true)
	}
	return n, ok, lat, spd, last, err
}

// samples 取样本统计。legacy=true 时只取没有 kind 的历史行。
// AddProxySpeed 记一次真实回源下载的吞吐。和 checks 分开：
// 探测是「冷启动 4MB」，这里是持续传输，口径不同不能混。
func (s *Store) AddProxySpeed(sourceID int64, kind string, bytes, ms int64) error {
	if sourceID <= 0 || bytes <= 0 || ms <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO proxy_speed(source_id, kind, ts, bytes, ms) VALUES(?,?,?,?,?)`,
		sourceID, sampleKind(kind), time.Now().Unix(), bytes, ms)
	return err
}

// AddLiveSpeed 记一次下载过程中的窗口测速（KB/s = bytes/(window_s*1000)）。
func (s *Store) AddLiveSpeed(sourceID int64, kind string, bytes int64, windowSec int) error {
	if sourceID <= 0 || bytes <= 0 || windowSec <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO live_speed(source_id, kind, ts, bytes, window_s) VALUES(?,?,?,?,?)`,
		sourceID, sampleKind(kind), time.Now().Unix(), bytes, windowSec)
	return err
}

// RecentLiveSpeeds 返回 since 之后最近 n 条窗口测速（KB/s），新的在前。
func (s *Store) RecentLiveSpeeds(id int64, kind string, since int64, n int) []int64 {
	rows, err := s.db.Query(`SELECT bytes, window_s FROM live_speed
		WHERE source_id = ? AND kind = ? AND ts >= ? AND window_s > 0
		ORDER BY ts DESC LIMIT ?`, id, sampleKind(kind), since, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var b int64
		var w int
		if rows.Scan(&b, &w) == nil && w > 0 {
			out = append(out, b/int64(w*1000))
		}
	}
	return out
}

// RecentProxySpeeds 返回最近 n 条真实单流下载速度（KB/s），新的在前。
func (s *Store) RecentProxySpeeds(id int64, kind string, n int) []int64 {
	rows, err := s.db.Query(`SELECT bytes, ms FROM proxy_speed
		WHERE source_id = ? AND kind = ? AND ms > 0 AND bytes >= ? AND ts >= ?
		ORDER BY ts DESC LIMIT ?`, id, sampleKind(kind), minProxySpeedBytes, time.Now().Unix()-proxySpeedMaxAgeSec, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var b, ms int64
		if rows.Scan(&b, &ms) == nil && ms > 0 {
			out = append(out, b/ms)
		}
	}
	return out
}

// proxySpeed 用最近的真实回源吞吐估这台源能跑多快（KB/s），没样本返回 0。
func (s *Store) proxySpeed(id int64, kind string) int64 {
	kind = sampleKind(kind)
	rows, err := s.db.Query(`SELECT bytes, ms, ts FROM proxy_speed
		WHERE source_id = ? AND kind = ? AND ms > 0 AND bytes >= ? AND ts >= ?
		ORDER BY ts DESC LIMIT 20`, id, kind, minProxySpeedBytes, time.Now().Unix()-proxySpeedMaxAgeSec)
	if err != nil {
		return 0
	}
	defer rows.Close()
	// 72h 半衰期：24h 硬切会让多数源没样本，直接回退到 4MB 探测值。
	// 最少 3 条才生效，避免单条慢启动样本把估计带偏；ghcr 样本稀，放宽到 2 条。
	const halfLifeSec = 72 * 3600
	minN := 3
	if kind == "ghcr" {
		minN = 2
	}
	now := time.Now().Unix()
	var sw, sv float64
	n := 0
	for rows.Next() {
		var b, ms, ts int64
		if rows.Scan(&b, &ms, &ts) != nil || ms <= 0 {
			continue
		}
		age := float64(now - ts)
		if age < 0 {
			age = 0
		}
		w := math.Pow(0.5, age/halfLifeSec)
		sv += float64(b) / float64(ms) * w // bytes/ms 正好是 KB/s
		sw += w
		n++
	}
	if n < minN || sw == 0 {
		return 0
	}
	return int64(sv / sw)
}

func (s *Store) samples(id int64, kind string, legacy bool) (n, ok int, lat, spd, last int64, err error) {
	// 绝对寿命对两种来源都生效：样本窗口只限条数，不设下限的话，
	// 停更半年的源仍会拿旧数据参与评分。
	q := `SELECT ts, ok, latency_ms, speed_kbps FROM checks
		WHERE source_id = ? AND tier >= 2 AND ts >= ?`
	args := []any{id, time.Now().Unix() - proxySpeedMaxAgeSec}
	switch {
	case legacy:
		// 无 kind 的是 2.x 遗留行，同样受绝对寿命约束：
		// 新数据过期后不能让更老的数据接替成为评分来源。
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
	// 速度改用指数加权（最新的权重 1，往前每条乘 0.8）：中位数对链路突变
	// 反应太慢，一次掉速要等整个窗口换完才反映出来。
	var sw, sv, w float64
	w = 1
	for _, v := range spds {
		sv += float64(v) * w
		sw += w
		w *= 0.8
	}
	if sw > 0 {
		spd = int64(sv / sw)
	}
	// 真实回源吞吐优先：探测那 4MB 量的是冷启动，估大文件会低一个量级。
	if v := s.proxySpeed(id, kind); v > 0 {
		spd = v
	}
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

// SourceFailedRecently 返回「最近一条有成败意义的事件是失败」的源，以及
// 那条事件的时间（Unix 秒）。只用于没有 breaker_state 的老库兜底。
//
// 中性事件不能把更早的失败盖掉：之前的实现取 MAX(id) 后判 ok，429 之后
// 来一条 404 就会让重启后的调度器以为源已经恢复。这里先筛出「有意义」
// 的事件再取最后一条。3.5.1 之前写的事件没有 neutral 列，按状态码判定：
// 401/403/404/410 视为中性。
func (s *Store) SourceFailedRecently(kind string, since int64) (map[int64]int64, error) {
	rows, err := s.db.Query(`
		SELECT e.source_id, e.ts FROM source_events e
		JOIN (SELECT source_id, MAX(id) AS mid FROM source_events
		      WHERE kind=? AND neutral=0 AND (ok=0 OR code NOT IN (401,403,404,410))
		      GROUP BY source_id) m
		  ON m.mid = e.id
		WHERE e.ok=0 AND e.ts>=?`, kind, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, ts int64
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		out[id] = ts
	}
	return out, rows.Err()
}

// BreakerState 是一个（源, 协议）的熔断运行时状态。
type BreakerState struct {
	SourceID  int64
	Kind      string
	Fails     int
	Until     int64 // Unix 秒，绝对截止时间
	UpdatedAt int64 // Unix 纳秒，并发写入按它比新旧
}

// OutcomeRecord 是一次回源结果的持久化载荷。
//
// Breaker 非 nil 时，事件和熔断状态必须在同一个事务里落库：只写进一件，
// 就会留下「事件说 429、状态说没事」的分裂，重启后又回到错误的那一边。
type OutcomeRecord struct {
	SourceID int64
	Kind     string
	OK       bool
	Neutral  bool
	Code     int
	MS       int64
	Breaker  *BreakerState
}

// RecordOutcome 原子地记录一次回源结果，并按需更新熔断状态。
//
// 失败与恢复都走 upsert 且带 updated_at 守卫：同一源并发上报时，晚到的
// 旧结果不会覆盖新状态。恢复写的是 fails=0 的墓碑行，不删行——删掉就
// 没法再比新旧了。
func (s *Store) RecordOutcome(rec OutcomeRecord) error {
	if rec.SourceID == 0 {
		return nil
	}
	b, n := 0, 0
	// ok 列对中性事件也写 1：老版本读端只认这一列，写成 0 会把 404 当故障。
	// 真正的三态语义靠 neutral 区分，新读端不能只看 ok。
	if rec.OK || rec.Neutral {
		b = 1
	}
	if rec.Neutral {
		n = 1
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 事件表没有外键，源删掉后迟到的上报会把它重新插回来。
	// 用 INSERT ... SELECT ... WHERE EXISTS 把「源还在」交给数据库判断，
	// 堵住调用方检查到真正写入之间的空隙。
	res, err := tx.Exec(`INSERT INTO source_events(source_id, kind, ts, ok, neutral, ms, code)
		SELECT ?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM sources WHERE id=?)`,
		rec.SourceID, rec.Kind, time.Now().Unix(), b, n, rec.MS, rec.Code, rec.SourceID)
	if err != nil {
		return err
	}
	if aff, _ := res.RowsAffected(); aff == 0 {
		// 源已经删了：状态也不写，别把刚清理的熔断复活。
		return tx.Commit()
	}
	if rec.Breaker != nil {
		bs := *rec.Breaker
		fails, until := bs.Fails, bs.Until
		if fails <= 0 {
			fails, until = 0, 0
		}
		if _, err := tx.Exec(`INSERT INTO breaker_state(source_id, kind, fails, until, updated_at)
			VALUES(?,?,?,?,?)
			ON CONFLICT(source_id, kind) DO UPDATE SET
				fails=excluded.fails, until=excluded.until, updated_at=excluded.updated_at
			WHERE excluded.updated_at > breaker_state.updated_at`,
			rec.SourceID, rec.Kind, fails, until, bs.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadBreakerStates 读出仍然有效的熔断状态（fails=0 的墓碑不算）。
func (s *Store) LoadBreakerStates() ([]BreakerState, error) {
	rows, err := s.db.Query(`SELECT source_id, kind, fails, until, updated_at
		FROM breaker_state WHERE fails > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BreakerState
	for rows.Next() {
		var b BreakerState
		if err := rows.Scan(&b.SourceID, &b.Kind, &b.Fails, &b.Until, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ClearBreakerStates 在源被删除或改地址时清掉持久化状态：
// 同一个 id 换了一台服务器，旧熔断不该继续生效。
func (s *Store) ClearBreakerStates(sourceID int64) {
	if sourceID == 0 {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM breaker_state WHERE source_id=?`, sourceID)
}

// RecordSourceEvent 兼容旧调用：只记事件，不动熔断状态。
func (s *Store) RecordSourceEvent(sourceID int64, kind string, ok bool, ms int64, code int) {
	_ = s.RecordOutcome(OutcomeRecord{SourceID: sourceID, Kind: kind, OK: ok, MS: ms, Code: code})
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

// SourceEvents 返回某个源最近事件的三态计数，供面板/排障使用。
// 成功 = ok 且非中性；中性 = 404/401 这类「源没坏、只是没给内容」；
// 失败 = ok=0。三者相加等于样本数，别把中性并进成功。
func (s *Store) SourceEvents(sourceID int64, limit int) (ok, neutral, fail int, lastErrAt int64, err error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT ts, ok, neutral FROM source_events
		WHERE source_id = ? ORDER BY ts DESC LIMIT ?`, sourceID, limit)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var o, n int
		if err = rows.Scan(&ts, &o, &n); err != nil {
			return
		}
		switch {
		case n == 1:
			neutral++
		case o == 1:
			ok++
		default:
			fail++
			if lastErrAt == 0 {
				lastErrAt = ts
			}
		}
	}
	err = rows.Err()
	return
}
