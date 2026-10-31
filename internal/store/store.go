// Package store persists sources, probe results, settings and sessions in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// 能力标记：-1 还没测过，0 不支持，1 支持
const (
	CapUnknown = -1
	CapNo      = 0
	CapYes     = 1
)

type Source struct {
	ID      int64  `json:"id"`
	BaseURL string `json:"base_url"`
	Name    string `json:"name"`
	Origin  string `json:"origin"` // seed | discovered | manual
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"` // pending | usable | degraded | dead | blocked | archived
	Note    string `json:"note"`
	// StatusAt 是进入当前状态的时间，StatusReason 是为什么进去的。
	// 归档/清理都靠 StatusAt 判断「坏了多久」，所以只在状态真的变化时更新。
	StatusAt     int64  `json:"status_at"`
	StatusReason string `json:"status_reason"`
	// ProbeAt 是最近一次探测尝试的时间；死源/封禁源靠它做低频恢复探测。
	ProbeAt int64 `json:"probe_at"`

	DockerCap int `json:"docker_cap"`
	GithubCap int `json:"github_cap"`
	// GitCap 单独记：前缀代理能代 raw/release，不代表能做 git smart HTTP。
	// 默认 0（不确定），实测通过后由调度器置 1。
	GitCap int `json:"git_cap"`

	CreatedAt int64 `json:"created_at"`

	// 每种类型最近一次检测结果
	Last map[string]*Check `json:"last,omitempty"`
	// 每种类型 24h 可用率，-1 表示无数据
	Uptime map[string]float64 `json:"uptime,omitempty"`
	// 每种类型 24h 延迟 P95（毫秒，只算成功样本）；0 表示样本不足
	P95 map[string]int64 `json:"p95_ms,omitempty"`
	// 每种类型 24h 上游 429 占比（0~1）。分母只算真正拿到上游 HTTP 响应的请求
	// （code 100–599）；DNS、连接失败、超时没有响应，不进分母。有效样本不足时
	// 显式给 null——用 0 冒充「没有限流」比不给数字更容易误导。
	Rate429 map[string]*float64 `json:"rate429,omitempty"`
	// 每种类型的可信度及证据（近 7 天）
	Trust map[string]Trust `json:"trust,omitempty"`
}

// Kinds 返回这个源被确认支持的加速类型。
func (s Source) Kinds() []string {
	var out []string
	if s.DockerCap == CapYes {
		out = append(out, "docker")
	}
	if s.GithubCap == CapYes {
		out = append(out, "github")
	}
	return out
}

// Supports 表示这个类型是否已被确认可用。
func (s Source) Supports(kind string) bool {
	switch kind {
	case "docker":
		return s.DockerCap == CapYes
	case "github":
		return s.GithubCap == CapYes
	}
	return false
}

type Check struct {
	ID        int64  `json:"id"`
	SourceID  int64  `json:"source_id"`
	Kind      string `json:"kind"`
	TS        int64  `json:"ts"`
	Tier      int    `json:"tier"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	SpeedKBps int64  `json:"speed_kbps"`
	Digest    string `json:"digest"`
	DigestOK  int    `json:"digest_ok"` // -1 未知 0 不一致 1 一致
	BlobOK    int    `json:"blob_ok"`   // layer 字节：-1 未校验 0 不符 1 一致
	TTFBMS    int64  `json:"ttfb_ms"`   // 首字节耗时；0 表示这次没测
	TLSOK     bool   `json:"tls_ok"`
	CertDays  int    `json:"cert_days"`
	HTTPCode  int    `json:"http_code"`
	Error     string `json:"error"`
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	// foreign_keys 要显式打开，否则 checks 的 ON DELETE CASCADE 形同虚设，
	// 删源之后检测记录会留成孤儿。
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc/sqlite: 单写者最稳
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	// 启动时立刻验证可写：bind mount 权限不对时，SQLite 可能能读能建表，
	// 却在第一次写检测记录时才失败，那种失败会静默几分钟。
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _writable(x INTEGER);
		INSERT INTO _writable(x) VALUES(1); DELETE FROM _writable;`); err != nil {
		return nil, fmt.Errorf("数据目录不可写（%v）：容器内以 uid 10001 运行，请先 chown -R 10001:10001 挂载目录，或改用 named volume", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS sources (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  base_url TEXT NOT NULL,
  name TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT 'manual',
  enabled INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'pending',
  note TEXT NOT NULL DEFAULT '',
  docker_cap INTEGER NOT NULL DEFAULT -1,
  github_cap INTEGER NOT NULL DEFAULT -1,
  git_cap INTEGER NOT NULL DEFAULT 0,
  status_at INTEGER NOT NULL DEFAULT 0,
  status_reason TEXT NOT NULL DEFAULT '',
  probe_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE(base_url)
);
CREATE TABLE IF NOT EXISTS checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  kind TEXT NOT NULL DEFAULT '',
  ts INTEGER NOT NULL,
  tier INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  speed_kbps INTEGER NOT NULL DEFAULT 0,
  digest TEXT NOT NULL DEFAULT '',
  digest_ok INTEGER NOT NULL DEFAULT -1,
  blob_ok INTEGER NOT NULL DEFAULT -1,
  ttfb_ms INTEGER NOT NULL DEFAULT 0,
  tls_ok INTEGER NOT NULL DEFAULT 1,
  cert_days INTEGER NOT NULL DEFAULT 0,
  http_code INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_checks_src_ts ON checks(source_id, ts DESC);
CREATE TABLE IF NOT EXISTS source_health (
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  family TEXT NOT NULL,
  fails INTEGER NOT NULL DEFAULT 0,
  ok_at INTEGER NOT NULL DEFAULT 0,
  last_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(source_id, family)
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
`)
	if err != nil {
		return err
	}
	if err := s.upgradeFromV1(); err != nil {
		return err
	}
	if err := s.ensureColumns("checks", map[string]string{
		"kind":      "TEXT NOT NULL DEFAULT ''",
		"digest":    "TEXT NOT NULL DEFAULT ''",
		"tls_ok":    "INTEGER NOT NULL DEFAULT 1",
		"cert_days": "INTEGER NOT NULL DEFAULT 0",
		"http_code": "INTEGER NOT NULL DEFAULT 0",
		"blob_ok":   "INTEGER NOT NULL DEFAULT -1",
		"ttfb_ms":   "INTEGER NOT NULL DEFAULT 0",
	}); err != nil {
		return err
	}
	if err := s.ensureColumns("sources", map[string]string{
		"git_cap":       "INTEGER NOT NULL DEFAULT 0",
		"status_at":     "INTEGER NOT NULL DEFAULT 0",
		"status_reason": "TEXT NOT NULL DEFAULT ''",
		"probe_at":      "INTEGER NOT NULL DEFAULT 0",
	}); err != nil {
		return err
	}
	// 老库没有 status_at：从升级这一刻开始计时，而不是拿 created_at 倒推。
	// 倒推会把所有历史死源在第一次巡检时一起归档，升级即清空列表。
	if _, err := s.db.Exec(`UPDATE sources SET status_at=? WHERE status_at=0`, time.Now().Unix()); err != nil {
		return err
	}
	return s.migrateAccel()
}

// ensureColumns 给老库补上后来新增的列，避免 SELECT 报 no such column。
func (s *Store) ensureColumns(table string, want map[string]string) error {
	have, err := s.columns(table)
	if err != nil {
		return err
	}
	for name, decl := range want {
		if have[name] {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + name + " " + decl); err != nil {
			return fmt.Errorf("补列 %s.%s: %w", table, name, err)
		}
	}
	return nil
}

// upgradeFromV1 把 v1 的 (kind, base_url) 表结构升级成能力标记。
//
// v1 的 sources 表是 kind TEXT NOT NULL 且 UNIQUE(base_url,kind)。只加列不够：
// 新代码不再写 kind，INSERT 会撞 NOT NULL，而 INSERT OR IGNORE 会把它当"已存在"
// 静默跳过，表现成"什么地址都提示已在列表里"。所以这里整表重建。
func (s *Store) upgradeFromV1() error {
	cols, err := s.columns("sources")
	if err != nil {
		return err
	}
	if !cols["kind"] {
		return nil // 已经是新结构
	}

	// 先把旧的 kind 映射存下来，重建表之后 checks 的历史记录还要用它回填
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS _v1kind`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE _v1kind AS SELECT id, kind FROM sources`); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE sources_v2 (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  base_url TEXT NOT NULL,
		  name TEXT NOT NULL,
		  origin TEXT NOT NULL DEFAULT 'manual',
		  enabled INTEGER NOT NULL DEFAULT 1,
		  status TEXT NOT NULL DEFAULT 'pending',
		  note TEXT NOT NULL DEFAULT '',
		  docker_cap INTEGER NOT NULL DEFAULT -1,
		  github_cap INTEGER NOT NULL DEFAULT -1,
		  git_cap INTEGER NOT NULL DEFAULT 0,
		  created_at INTEGER NOT NULL,
		  UNIQUE(base_url)
		)`,
		`INSERT INTO sources_v2(id,base_url,name,origin,enabled,status,note,docker_cap,github_cap,git_cap,created_at)
		 SELECT id,base_url,name,origin,enabled,status,note,
		   (SELECT MAX(CASE WHEN s2.kind='docker' THEN 1 ELSE -1 END) FROM sources s2 WHERE s2.base_url=sources.base_url),
		   (SELECT MAX(CASE WHEN s2.kind='github' THEN 1 ELSE -1 END) FROM sources s2 WHERE s2.base_url=sources.base_url),
		   0,
		   created_at
		 FROM sources WHERE id IN (SELECT MIN(id) FROM sources GROUP BY base_url)`,
		`DROP TABLE sources`,
		`ALTER TABLE sources_v2 RENAME TO sources`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("v1→v2 迁移失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if c, err := s.columns("checks"); err == nil && !c["kind"] {
		if _, err := s.db.Exec(`ALTER TABLE checks ADD COLUMN kind TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE checks SET kind=COALESCE((SELECT kind FROM _v1kind WHERE _v1kind.id=checks.source_id),'')`); err != nil {
			return err
		}
	}
	_, _ = s.db.Exec(`DROP TABLE IF EXISTS _v1kind`)
	return nil
}

func (s *Store) columns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

const srcCols = `id,base_url,name,origin,enabled,status,note,docker_cap,github_cap,git_cap,status_at,status_reason,probe_at,created_at`

func scanSource(sc interface{ Scan(...any) error }) (Source, error) {
	var x Source
	err := sc.Scan(&x.ID, &x.BaseURL, &x.Name, &x.Origin, &x.Enabled, &x.Status, &x.Note,
		&x.DockerCap, &x.GithubCap, &x.GitCap, &x.StatusAt, &x.StatusReason, &x.ProbeAt, &x.CreatedAt)
	return x, err
}

func (s *Store) ListSources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT ` + srcCols + ` FROM sources ORDER BY status='usable' DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		x, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	since := time.Now().Add(-24 * time.Hour).Unix()
	for i := range out {
		out[i].Last = map[string]*Check{}
		out[i].Uptime = map[string]float64{}
		for _, k := range []string{"docker", "github"} {
			if c, err := s.LastCheck(out[i].ID, k); err == nil && c != nil {
				out[i].Last[k] = c
			}
			out[i].Uptime[k] = s.uptime(out[i].ID, since, k)
		}
	}

	// P95 一次查完再分桶：逐源查会变成 N 条 SQL。
	// 只算成功样本——失败那几条的「延迟」是超时时间，混进去量的是耐心，不是速度。
	type key struct {
		id   int64
		kind string
	}
	samples := map[key][]int64{}
	prows, err := s.db.Query(`SELECT source_id,kind,latency_ms FROM checks
		WHERE ts>=? AND tier>=2 AND ok=1 AND latency_ms>0`, since)
	if err == nil {
		for prows.Next() {
			var k key
			var ms int64
			if err := prows.Scan(&k.id, &k.kind, &ms); err == nil {
				samples[k] = append(samples[k], ms)
			}
		}
		prows.Close()
	}
	for i := range out {
		out[i].P95 = map[string]int64{}
		for _, k := range []string{"docker", "github"} {
			out[i].P95[k] = p95(samples[key{out[i].ID, k}])
		}
	}

	// 限流比例：同一个窗口里，这台源回了多少次 429。
	// 分母只数 code 100–599：那才是一次真实的上游响应。code=0 是没拿到响应
	// （DNS/连接/超时），把它算进分母等于拿「网络不通」稀释「被限流」。
	// 老数据里的 code=0 不做任何推断，直接不进统计。
	type rate struct{ all, c429 int64 }
	rates := map[key]*rate{}
	erows, err := s.db.Query(`SELECT source_id,kind,COUNT(*),SUM(code=429) FROM source_events
		WHERE ts>=? AND code BETWEEN 100 AND 599 GROUP BY source_id,kind`, since)
	if err == nil {
		for erows.Next() {
			var k key
			var r rate
			if err := erows.Scan(&k.id, &k.kind, &r.all, &r.c429); err == nil {
				rates[k] = &r
			}
		}
		erows.Close()
	}
	for i := range out {
		out[i].Rate429 = map[string]*float64{}
		for _, k := range []string{"docker", "github"} {
			// 两种类型都给出键：没有有效样本时是 null，不是「缺字段」。
			out[i].Rate429[k] = nil
			if r := rates[key{out[i].ID, k}]; r != nil && r.all >= min429Samples {
				v := float64(r.c429) / float64(r.all)
				out[i].Rate429[k] = &v
			}
		}
	}

	// 可信度按类型算，和上面的延迟、限流同一个窗口口径。
	for _, k := range []string{"docker", "github"} {
		tm, err := s.trustMap(k, time.Now())
		if err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].Trust == nil {
				out[i].Trust = map[string]Trust{}
			}
			t := tm[out[i].ID]
			t.Level = trustLevel(t)
			out[i].Trust[k] = t
		}
	}
	return out, nil
}

// min429Samples 是报限流比例的最小样本数。十次里两次 429 说明不了什么，
// 但写成 20% 会让人立刻把一台好源停掉。
const min429Samples = 20

// minP95Samples 是出 P95 的最小样本数。三个样本的「95 分位」就是最大值，
// 标成 P95 会让人误以为有统计意义。
const minP95Samples = 5

// p95 用 nearest-rank 算第 95 百分位；样本不足返回 0。
func p95(v []int64) int64 {
	if len(v) < minP95Samples {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	idx := (len(v)*95 + 99) / 100 // ceil(0.95*n)
	if idx < 1 {
		idx = 1
	}
	return v[idx-1]
}

func (s *Store) GetSource(id int64) (*Source, error) {
	x, err := scanSource(s.db.QueryRow(`SELECT `+srcCols+` FROM sources WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) ByURL(baseURL string) (*Source, error) {
	x, err := scanSource(s.db.QueryRow(`SELECT `+srcCols+` FROM sources WHERE base_url=?`, baseURL))
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) AddSource(x Source) (int64, error) {
	if x.Origin == "" {
		x.Origin = "manual"
	}
	if x.Status == "" {
		x.Status = "pending"
	}
	if x.CreatedAt == 0 {
		x.CreatedAt = time.Now().Unix()
	}
	if x.DockerCap == 0 && x.GithubCap == 0 {
		x.DockerCap, x.GithubCap = CapUnknown, CapUnknown
	}
	if x.StatusAt == 0 {
		x.StatusAt = x.CreatedAt
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO sources(base_url,name,origin,enabled,status,note,docker_cap,github_cap,git_cap,status_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		x.BaseURL, x.Name, x.Origin, x.Enabled, x.Status, x.Note, x.DockerCap, x.GithubCap, x.GitCap, x.StatusAt, x.CreatedAt)
	if err != nil {
		return 0, err
	}
	// INSERT OR IGNORE 时 LastInsertId 会返回上一次插入的 rowid，
	// 必须用 RowsAffected 判断是否真的写进去了。
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrExists
	}
	id, _ := res.LastInsertId()
	return id, nil
}

var ErrExists = errors.New("source already exists")

// ErrArchived：已归档的源不能直接启用，只能走恢复流程。
var ErrArchived = errors.New("已归档的源不能直接启用，请用恢复流程重新验证")

func (s *Store) UpdateSource(id int64, enabled *bool, name, note *string) error {
	if enabled != nil {
		if *enabled {
			// 归档是人工/自动做过的判断，不能被一个「启用」开关顺手撤销：
			// 恢复必须走显式流程（status=pending → 重新探测验证）。
			var status string
			if err := s.db.QueryRow(`SELECT status FROM sources WHERE id=?`, id).Scan(&status); err != nil {
				return err
			}
			if status == "archived" {
				return ErrArchived
			}
		}
		if _, err := s.db.Exec(`UPDATE sources SET enabled=? WHERE id=?`, *enabled, id); err != nil {
			return err
		}
	}
	if name != nil && *name != "" {
		if _, err := s.db.Exec(`UPDATE sources SET name=? WHERE id=?`, *name, id); err != nil {
			return err
		}
	}
	if note != nil {
		if _, err := s.db.Exec(`UPDATE sources SET note=? WHERE id=?`, *note, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetCaps(id int64, docker, github int) error {
	_, err := s.db.Exec(`UPDATE sources SET docker_cap=?, github_cap=? WHERE id=?`, docker, github, id)
	return err
}

func (s *Store) SetStatus(id int64, status string) error {
	return s.SetStatusWhy(id, status, "")
}

// SetStatusWhy 改状态并记下时间和原因。状态没变就不动 status_at——
// 否则一个常年 dead 的源每次探测都会把「坏了多久」清零，永远归档不了。
func (s *Store) SetStatusWhy(id int64, status, reason string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE sources SET
		status_at = CASE WHEN status<>? THEN ? ELSE status_at END,
		status_reason = CASE WHEN status<>? THEN ? ELSE status_reason END,
		status = ?
		WHERE id=?`, status, now, status, reason, status, id)
	return err
}

// TouchProbe0 清掉探测时间，让恢复的源不受低频门限阻挡，立刻可探。
func (s *Store) TouchProbe0(id int64) error {
	_, err := s.db.Exec(`UPDATE sources SET probe_at=0 WHERE id=?`, id)
	return err
}

// TouchProbe 记一次探测尝试（不论成败）。
func (s *Store) TouchProbe(id int64) error {
	_, err := s.db.Exec(`UPDATE sources SET probe_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

// ConsecutiveClean 数最近连续多少次 tier>=2 探测既通过又没被篡改。
// 封禁源靠它决定能不能放回去：一次干净不够，要连续两次。
func (s *Store) ConsecutiveClean(sourceID int64, kind string) int {
	rows, err := s.db.Query(`SELECT ok, digest_ok FROM checks
		WHERE source_id=? AND kind=? AND tier>=2 ORDER BY ts DESC LIMIT 5`, sourceID, kind)
	if err != nil {
		return 0
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ok bool
		var digestOK int
		if err := rows.Scan(&ok, &digestOK); err != nil {
			return n
		}
		if ok && digestOK == 1 {
			n++
			continue
		}
		break
	}
	return n
}

// ArchiveStale 把「坏了/被封超过 before 秒」的源归档：不再探测、不再参与调度，
// 记录保留，面板可一键恢复。返回归档条数。
func (s *Store) ArchiveStale(before int64) (int64, error) {
	res, err := s.db.Exec(`UPDATE sources SET status='archived', status_at=?, status_reason=?
		WHERE enabled=1 AND status IN ('dead','blocked') AND status_at>0 AND status_at<=?`,
		time.Now().Unix(), "长时间不可用，自动归档", before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupArchived 彻底删除归档超过 before 秒的源，连带检测记录和调度事件。
// 只删 archived——别的状态再久也留着，删数据是单向操作。
func (s *Store) CleanupArchived(before int64) (int64, error) {
	rows, err := s.db.Query(`SELECT id FROM sources WHERE status='archived' AND status_at>0 AND status_at<=?`, before)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if err := s.DeleteSource(id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) DeleteSource(id int64) error {
	// source_events 没建外键，显式清；checks 有 CASCADE，但老库可能没开
	// foreign_keys，也顺手清掉，不留孤儿。
	if _, err := s.db.Exec(`DELETE FROM source_events WHERE source_id=?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM checks WHERE source_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sources WHERE id=?`, id)
	return err
}

func (s *Store) AddCheck(c Check) error {
	if c.TS == 0 {
		c.TS = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO checks(source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,blob_ok,ttfb_ms,tls_ok,cert_days,http_code,error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.SourceID, c.Kind, c.TS, c.Tier, c.OK, c.LatencyMS, c.SpeedKBps, c.Digest, c.DigestOK, c.BlobOK, c.TTFBMS, c.TLSOK, c.CertDays, c.HTTPCode, c.Error)
	return err
}

// LastCheck 返回某源某类型最近一次检测，优先 tier>=2 的真实探测。
func (s *Store) LastCheck(sourceID int64, kind string) (*Check, error) {
	var c Check
	err := s.db.QueryRow(`SELECT id,source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,blob_ok,ttfb_ms,tls_ok,cert_days,http_code,error
		FROM checks WHERE source_id=? AND kind=? ORDER BY (tier>=2) DESC, ts DESC LIMIT 1`, sourceID, kind).
		Scan(&c.ID, &c.SourceID, &c.Kind, &c.TS, &c.Tier, &c.OK, &c.LatencyMS, &c.SpeedKBps, &c.Digest, &c.DigestOK, &c.BlobOK, &c.TTFBMS, &c.TLSOK, &c.CertDays, &c.HTTPCode, &c.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ConsecutiveFailures 返回某类型最近连续失败次数（按时间倒序，遇到成功即停）。
func (s *Store) ConsecutiveFailures(sourceID int64, kind string) int {
	rows, err := s.db.Query(`SELECT ok FROM checks WHERE source_id=? AND kind=? AND tier>=2 ORDER BY ts DESC LIMIT 5`, sourceID, kind)
	if err != nil {
		return 0
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ok bool
		if err := rows.Scan(&ok); err != nil {
			return n
		}
		if ok {
			break
		}
		n++
	}
	return n
}

// uptime 返回 since 之后该类型的成功率；无样本返回 -1。
func (s *Store) uptime(sourceID, since int64, kind string) float64 {
	var total, ok int
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(ok),0) FROM checks WHERE source_id=? AND kind=? AND ts>=? AND tier>=2`, sourceID, kind, since).
		Scan(&total, &ok)
	if err != nil || total == 0 {
		return -1
	}
	return float64(ok) / float64(total)
}

// Series 返回某源最近 limit 条 tier>=2 的检测（两种类型混合），按时间正序。
func (s *Store) Series(sourceID int64, limit int) ([]Check, error) {
	rows, err := s.db.Query(`SELECT id,source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,blob_ok,ttfb_ms,tls_ok,cert_days,http_code,error
		FROM (SELECT * FROM checks WHERE source_id=? AND tier>=2 ORDER BY ts DESC LIMIT ?) ORDER BY ts ASC`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		if err := rows.Scan(&c.ID, &c.SourceID, &c.Kind, &c.TS, &c.Tier, &c.OK, &c.LatencyMS, &c.SpeedKBps, &c.Digest, &c.DigestOK, &c.BlobOK, &c.TTFBMS, &c.TLSOK, &c.CertDays, &c.HTTPCode, &c.Error); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnabledSources 返回所有启用的源（runner 自己决定测哪几种类型）。
func (s *Store) EnabledSources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT ` + srcCols + ` FROM sources WHERE enabled=1 AND status<>'archived'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		x, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n)
	return n, err
}

// Prune 删除 retention 之前的检测记录。
func (s *Store) Prune(before int64) error {
	_, err := s.db.Exec(`DELETE FROM checks WHERE ts < ?`, before)
	return err
}

// ---- settings ----

func (s *Store) GetSetting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// Tx 在一个事务里执行 fn，fn 返回错误就整体回滚。
// 需要「先判断再写」的地方都用它：单条 SQL 的原子性管不了跨语句的状态。
func (s *Store) Tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) AllSettings() map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT key,value FROM settings`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// ---- sessions ----

func (s *Store) CreateSession(token string, ttl time.Duration) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO sessions(token,created_at,expires_at) VALUES(?,?,?)`,
		token, now, now+int64(ttl.Seconds()))
	return err
}

func (s *Store) ValidSession(token string) bool {
	if token == "" {
		return false
	}
	var exp int64
	if err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token=?`, token).Scan(&exp); err != nil {
		return false
	}
	if exp < time.Now().Unix() {
		s.DeleteSession(token)
		return false
	}
	return true
}

func (s *Store) DeleteSession(token string) {
	s.db.Exec(`DELETE FROM sessions WHERE token=?`, token)
}

func (s *Store) DeleteAllSessions() {
	s.db.Exec(`DELETE FROM sessions`)
}

func (s *Store) PruneSessions() {
	s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
}

func (s *Store) String() string { return fmt.Sprintf("store(%p)", s.db) }
