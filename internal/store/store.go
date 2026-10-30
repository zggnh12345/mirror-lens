// Package store persists sources, probe results, settings and sessions in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
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
	Status  string `json:"status"` // pending | usable | degraded | dead | blocked
	Note    string `json:"note"`

	DockerCap int `json:"docker_cap"`
	GithubCap int `json:"github_cap"`

	CreatedAt int64 `json:"created_at"`

	// 每种类型最近一次检测结果
	Last map[string]*Check `json:"last,omitempty"`
	// 每种类型 24h 可用率，-1 表示无数据
	Uptime map[string]float64 `json:"uptime,omitempty"`
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
	TLSOK     bool   `json:"tls_ok"`
	CertDays  int    `json:"cert_days"`
	HTTPCode  int    `json:"http_code"`
	Error     string `json:"error"`
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
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
  tls_ok INTEGER NOT NULL DEFAULT 1,
  cert_days INTEGER NOT NULL DEFAULT 0,
  http_code INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_checks_src_ts ON checks(source_id, ts DESC);
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
	return s.ensureColumns("checks", map[string]string{
		"kind":      "TEXT NOT NULL DEFAULT ''",
		"digest":    "TEXT NOT NULL DEFAULT ''",
		"tls_ok":    "INTEGER NOT NULL DEFAULT 1",
		"cert_days": "INTEGER NOT NULL DEFAULT 0",
		"http_code": "INTEGER NOT NULL DEFAULT 0",
	})
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
		  created_at INTEGER NOT NULL,
		  UNIQUE(base_url)
		)`,
		`INSERT INTO sources_v2(id,base_url,name,origin,enabled,status,note,docker_cap,github_cap,created_at)
		 SELECT id,base_url,name,origin,enabled,status,note,
		   (SELECT MAX(CASE WHEN s2.kind='docker' THEN 1 ELSE -1 END) FROM sources s2 WHERE s2.base_url=sources.base_url),
		   (SELECT MAX(CASE WHEN s2.kind='github' THEN 1 ELSE -1 END) FROM sources s2 WHERE s2.base_url=sources.base_url),
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

const srcCols = `id,base_url,name,origin,enabled,status,note,docker_cap,github_cap,created_at`

func scanSource(sc interface{ Scan(...any) error }) (Source, error) {
	var x Source
	err := sc.Scan(&x.ID, &x.BaseURL, &x.Name, &x.Origin, &x.Enabled, &x.Status, &x.Note,
		&x.DockerCap, &x.GithubCap, &x.CreatedAt)
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
	return out, nil
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
	res, err := s.db.Exec(`INSERT OR IGNORE INTO sources(base_url,name,origin,enabled,status,note,docker_cap,github_cap,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		x.BaseURL, x.Name, x.Origin, x.Enabled, x.Status, x.Note, x.DockerCap, x.GithubCap, x.CreatedAt)
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

func (s *Store) UpdateSource(id int64, enabled *bool, name, note *string) error {
	if enabled != nil {
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
	_, err := s.db.Exec(`UPDATE sources SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) DeleteSource(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE id=?`, id)
	return err
}

func (s *Store) AddCheck(c Check) error {
	if c.TS == 0 {
		c.TS = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO checks(source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,tls_ok,cert_days,http_code,error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.SourceID, c.Kind, c.TS, c.Tier, c.OK, c.LatencyMS, c.SpeedKBps, c.Digest, c.DigestOK, c.TLSOK, c.CertDays, c.HTTPCode, c.Error)
	return err
}

// LastCheck 返回某源某类型最近一次检测，优先 tier>=2 的真实探测。
func (s *Store) LastCheck(sourceID int64, kind string) (*Check, error) {
	var c Check
	err := s.db.QueryRow(`SELECT id,source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,tls_ok,cert_days,http_code,error
		FROM checks WHERE source_id=? AND kind=? ORDER BY (tier>=2) DESC, ts DESC LIMIT 1`, sourceID, kind).
		Scan(&c.ID, &c.SourceID, &c.Kind, &c.TS, &c.Tier, &c.OK, &c.LatencyMS, &c.SpeedKBps, &c.Digest, &c.DigestOK, &c.TLSOK, &c.CertDays, &c.HTTPCode, &c.Error)
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
	rows, err := s.db.Query(`SELECT id,source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,tls_ok,cert_days,http_code,error
		FROM (SELECT * FROM checks WHERE source_id=? AND tier>=2 ORDER BY ts DESC LIMIT ?) ORDER BY ts ASC`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		if err := rows.Scan(&c.ID, &c.SourceID, &c.Kind, &c.TS, &c.Tier, &c.OK, &c.LatencyMS, &c.SpeedKBps, &c.Digest, &c.DigestOK, &c.TLSOK, &c.CertDays, &c.HTTPCode, &c.Error); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnabledSources 返回所有启用的源（runner 自己决定测哪几种类型）。
func (s *Store) EnabledSources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT ` + srcCols + ` FROM sources WHERE enabled=1`)
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
