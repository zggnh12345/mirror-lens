// Package store persists sources and probe results in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Source struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`   // docker | github
	BaseURL   string `json:"base_url"`
	Name      string `json:"name"`
	Origin    string `json:"origin"` // seed | discovered | manual
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status"` // pending | usable | degraded | dead | blocked
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`

	// 最近一次检测结果（列表接口直接带出来，省一次查询）
	Last *Check `json:"last,omitempty"`
	// 24h 可用率 0..1，-1 表示无数据
	Uptime24h float64 `json:"uptime_24h"`
}

type Check struct {
	ID        int64  `json:"id"`
	SourceID  int64  `json:"source_id"`
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
  kind TEXT NOT NULL,
  base_url TEXT NOT NULL,
  name TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT 'manual',
  enabled INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'pending',
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  UNIQUE(kind, base_url)
);
CREATE TABLE IF NOT EXISTS checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
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
`)
	return err
}

func (s *Store) ListSources(kind string) ([]Source, error) {
	q := `SELECT id,kind,base_url,name,origin,enabled,status,note,created_at FROM sources`
	var args []any
	if kind != "" {
		q += ` WHERE kind=?`
		args = append(args, kind)
	}
	q += ` ORDER BY kind, name`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var x Source
		if err := rows.Scan(&x.ID, &x.Kind, &x.BaseURL, &x.Name, &x.Origin, &x.Enabled, &x.Status, &x.Note, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	since := time.Now().Add(-24 * time.Hour).Unix()
	for i := range out {
		last, err := s.LastCheck(out[i].ID)
		if err == nil {
			out[i].Last = last
		}
		out[i].Uptime24h = s.uptime(out[i].ID, since)
	}
	return out, nil
}

func (s *Store) GetSource(id int64) (*Source, error) {
	var x Source
	err := s.db.QueryRow(`SELECT id,kind,base_url,name,origin,enabled,status,note,created_at FROM sources WHERE id=?`, id).
		Scan(&x.ID, &x.Kind, &x.BaseURL, &x.Name, &x.Origin, &x.Enabled, &x.Status, &x.Note, &x.CreatedAt)
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
	res, err := s.db.Exec(`INSERT OR IGNORE INTO sources(kind,base_url,name,origin,enabled,status,note,created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		x.Kind, x.BaseURL, x.Name, x.Origin, x.Enabled, x.Status, x.Note, x.CreatedAt)
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

func (s *Store) DeleteSource(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE id=?`, id)
	return err
}

func (s *Store) SetStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE sources SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) AddCheck(c Check) error {
	if c.TS == 0 {
		c.TS = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO checks(source_id,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,tls_ok,cert_days,http_code,error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.SourceID, c.TS, c.Tier, c.OK, c.LatencyMS, c.SpeedKBps, c.Digest, c.DigestOK, c.TLSOK, c.CertDays, c.HTTPCode, c.Error)
	return err
}

func (s *Store) LastCheck(sourceID int64) (*Check, error) {
	var c Check
	// 优先展示真实探测（tier>=2）的结果；只有 L1 时才退回存活检查。
	err := s.db.QueryRow(`SELECT id,source_id,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,tls_ok,cert_days,http_code,error
		FROM checks WHERE source_id=? ORDER BY (tier>=2) DESC, ts DESC LIMIT 1`, sourceID).
		Scan(&c.ID, &c.SourceID, &c.TS, &c.Tier, &c.OK, &c.LatencyMS, &c.SpeedKBps, &c.Digest, &c.DigestOK, &c.TLSOK, &c.CertDays, &c.HTTPCode, &c.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// uptime 返回 since 之后的成功率；无样本返回 -1。
func (s *Store) uptime(sourceID, since int64) float64 {
	var total, ok int
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(ok),0) FROM checks WHERE source_id=? AND ts>=? AND tier>=2`, sourceID, since).
		Scan(&total, &ok)
	if err != nil || total == 0 {
		return -1
	}
	return float64(ok) / float64(total)
}

// Series 返回某源最近 limit 条 tier>=2 的检测，按时间正序。
func (s *Store) Series(sourceID int64, limit int) ([]Check, error) {
	rows, err := s.db.Query(`SELECT id,source_id,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,tls_ok,cert_days,http_code,error
		FROM (SELECT * FROM checks WHERE source_id=? AND tier>=2 ORDER BY ts DESC LIMIT ?) ORDER BY ts ASC`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		if err := rows.Scan(&c.ID, &c.SourceID, &c.TS, &c.Tier, &c.OK, &c.LatencyMS, &c.SpeedKBps, &c.Digest, &c.DigestOK, &c.TLSOK, &c.CertDays, &c.HTTPCode, &c.Error); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Enabled(kind string, maxTier int) ([]Source, error) {
	q := `SELECT id,kind,base_url,name,origin,enabled,status,note,created_at FROM sources WHERE enabled=1`
	var args []any
	if kind != "" {
		q += ` AND kind=?`
		args = append(args, kind)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var x Source
		if err := rows.Scan(&x.ID, &x.Kind, &x.BaseURL, &x.Name, &x.Origin, &x.Enabled, &x.Status, &x.Note, &x.CreatedAt); err != nil {
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

func (s *Store) String() string { return fmt.Sprintf("store(%p)", s.db) }
