package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// 流量分类。源站评分只吃 probe 样本；代理流量单独记账，
// 否则拉大镜像时所有源看起来都变慢了。
const (
	TrafficProbe  = "probe"
	TrafficBypass = "bypass"
	TrafficHit    = "cache_hit"
	TrafficFill   = "cache_fill"
)

// 缓存键前缀 → 协议。统计按它拆，历史记录没有 kind 就留空（未知），不猜。
const (
	KindRaw    = "raw"
	KindHTTP   = "http"
	KindGit    = "git"
	KindDocker = "docker"
	KindGH     = "gh"
	KindAll    = "" // 未知/汇总
)

func KindOfKey(key string) string {
	switch {
	case strings.HasPrefix(key, "raw:"):
		return KindRaw
	case strings.HasPrefix(key, "http:"):
		return KindHTTP
	case strings.HasPrefix(key, "git:"):
		return KindGit
	case strings.HasPrefix(key, "docker:"):
		return KindDocker
	case strings.HasPrefix(key, "gh:"):
		return KindGH
	}
	return ""
}

type Token struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	Revoked   bool   `json:"revoked"`
}

type CacheEntry struct {
	Key          string `json:"key"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type"`
	ETag         string `json:"etag"`
	LastModified string `json:"last_modified"`
	Upstream     string `json:"upstream"`
	CreatedAt    int64  `json:"created_at"`
	LastHit      int64  `json:"last_hit"`
	Hits         int64  `json:"hits"`
	TTL          int64  `json:"ttl"` // 秒；0 表示每次都要回源验证
	// Verify 是这份内容「凭什么可信」：
	//   verified   有客户端给出的摘要（docker digest）做参照，落盘前逐字节比对过
	//   unverified 只是完整收到了，没有可信摘要可对
	// 没有 failed：校验不过的内容一律不落盘，所以不存在这条记录。
	Verify string `json:"verify"`
}

const (
	VerifyVerified   = "verified"
	VerifyUnverified = "unverified"
)

type TrafficRow struct {
	Day     string `json:"day"`
	Traffic string `json:"traffic"`
	Kind    string `json:"kind"`
	N       int64  `json:"n"`
	Bytes   int64  `json:"bytes"`
}

func (s *Store) migrateAccel() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tokens(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			hash TEXT NOT NULL UNIQUE,
			prefix TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_used INTEGER NOT NULL DEFAULT 0,
			revoked INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS cache_entries(
			key TEXT PRIMARY KEY,
			sha256 TEXT NOT NULL,
			size INTEGER NOT NULL,
			content_type TEXT NOT NULL DEFAULT '',
			etag TEXT NOT NULL DEFAULT '',
			last_modified TEXT NOT NULL DEFAULT '',
			upstream TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			last_hit INTEGER NOT NULL DEFAULT 0,
			hits INTEGER NOT NULL DEFAULT 0,
			ttl INTEGER NOT NULL DEFAULT 300,
			verify TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS idx_cache_sha ON cache_entries(sha256)`,
		`CREATE INDEX IF NOT EXISTS idx_cache_hit ON cache_entries(last_hit)`,
		`CREATE TABLE IF NOT EXISTS traffic_stats(
			day TEXT NOT NULL,
			traffic TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			n INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(day, traffic, kind))`,
		`CREATE TABLE IF NOT EXISTS meta(
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS pending_blobs(
			sha TEXT PRIMARY KEY,
			queued_at INTEGER NOT NULL)`,
		// 代理请求落到哪个源、成不成。只做熔断与排障，不参与速度评分。
		`CREATE TABLE IF NOT EXISTS source_events(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			ts INTEGER NOT NULL,
			ok INTEGER NOT NULL,
			ms INTEGER NOT NULL DEFAULT 0,
			code INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS idx_sev_src_ts ON source_events(source_id, ts DESC)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("加速器表结构: %w", err)
		}
	}
	// 老库补列：阶段 1 建的 cache_entries 没有 ttl
	if err := s.ensureColumns("cache_entries", map[string]string{
		"ttl":    "INTEGER NOT NULL DEFAULT 300",
		"verify": "TEXT NOT NULL DEFAULT ''",
	}); err != nil {
		return err
	}
	// 老库补列：事件表原来只记成败，没有状态码，算不出限流比例。
	if err := s.ensureColumns("source_events", map[string]string{
		"code": "INTEGER NOT NULL DEFAULT 0",
	}); err != nil {
		return err
	}
	return s.upgradeTrafficKind()
}

// ---- Token ----

// CreateToken 只在返回值里给一次明文，库里存 SHA-256。
func (s *Store) CreateToken(name string) (string, Token, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", Token{}, err
	}
	plain := "ml_" + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])
	prefix := plain[:11] + "…"
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO tokens(name, hash, prefix, created_at) VALUES(?,?,?,?)`,
		name, hash, prefix, now)
	if err != nil {
		return "", Token{}, err
	}
	id, _ := res.LastInsertId()
	return plain, Token{ID: id, Name: name, Prefix: prefix, CreatedAt: now}, nil
}

func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(`SELECT id, name, prefix, created_at, last_used, revoked FROM tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		var rev int
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.CreatedAt, &t.LastUsed, &rev); err != nil {
			return nil, err
		}
		t.Revoked = rev != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeToken(id int64) error {
	_, err := s.db.Exec(`UPDATE tokens SET revoked=1 WHERE id=?`, id)
	return err
}

// VerifyToken 按哈希查，未吊销才算通过；顺手记一次使用时间。
func (s *Store) VerifyToken(plain string) (*Token, bool) {
	if plain == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])
	var t Token
	var rev int
	err := s.db.QueryRow(`SELECT id, name, prefix, created_at, last_used, revoked FROM tokens WHERE hash=?`, hash).
		Scan(&t.ID, &t.Name, &t.Prefix, &t.CreatedAt, &t.LastUsed, &rev)
	if err != nil || rev != 0 {
		return nil, false
	}
	t.Revoked = false
	_, _ = s.db.Exec(`UPDATE tokens SET last_used=? WHERE id=?`, time.Now().Unix(), t.ID)
	return &t, true
}

// ---- 缓存索引 ----

func (s *Store) CacheGet(key string) (*CacheEntry, error) {
	var e CacheEntry
	err := s.db.QueryRow(`SELECT key, sha256, size, content_type, etag, last_modified, upstream,
		created_at, last_hit, hits, COALESCE(ttl,300), COALESCE(verify,'') FROM cache_entries WHERE key=?`, key).
		Scan(&e.Key, &e.SHA256, &e.Size, &e.ContentType, &e.ETag, &e.LastModified, &e.Upstream,
			&e.CreatedAt, &e.LastHit, &e.Hits, &e.TTL, &e.Verify)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) CachePut(e CacheEntry) error {
	if e.Verify == "" {
		e.Verify = VerifyUnverified
	}
	_, err := s.db.Exec(`INSERT INTO cache_entries(key, sha256, size, content_type, etag, last_modified, upstream, created_at, last_hit, hits, ttl, verify)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
			sha256=excluded.sha256, size=excluded.size, content_type=excluded.content_type,
			etag=excluded.etag, last_modified=excluded.last_modified, upstream=excluded.upstream,
			created_at=excluded.created_at, ttl=excluded.ttl, verify=excluded.verify`,
		e.Key, e.SHA256, e.Size, e.ContentType, e.ETag, e.LastModified, e.Upstream, e.CreatedAt, e.LastHit, e.Hits, e.TTL, e.Verify)
	return err
}

// CacheHit 只记命中，不动 created_at（TTL 不因命中而延长）。
func (s *Store) CacheHit(key string) {
	_, _ = s.db.Exec(`UPDATE cache_entries SET hits=hits+1, last_hit=? WHERE key=?`, time.Now().Unix(), key)
}

// CacheRefresh 用于 304：内容没变，TTL 重新计时。
func (s *Store) CacheRefresh(key string) {
	_, _ = s.db.Exec(`UPDATE cache_entries SET created_at=? WHERE key=?`, time.Now().Unix(), key)
}

func (s *Store) CacheDelete(key string) error {
	_, err := s.db.Exec(`DELETE FROM cache_entries WHERE key=?`, key)
	return err
}

func (s *Store) CacheList(limit int) ([]CacheEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT key, sha256, size, content_type, etag, last_modified, upstream,
		created_at, last_hit, hits, COALESCE(ttl,300), COALESCE(verify,'') FROM cache_entries ORDER BY last_hit DESC, created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CacheEntry{}
	for rows.Next() {
		var e CacheEntry
		if err := rows.Scan(&e.Key, &e.SHA256, &e.Size, &e.ContentType, &e.ETag, &e.LastModified,
			&e.Upstream, &e.CreatedAt, &e.LastHit, &e.Hits, &e.TTL, &e.Verify); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CacheTotals() (int64, int64, error) {
	var n, bytes int64
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM cache_entries`).Scan(&n, &bytes)
	return n, bytes, err
}

// ---- 流量账 ----

func (s *Store) RecordTraffic(traffic, kind string, nbytes int64) {
	day := time.Now().Format("2006-01-02")
	_, _ = s.db.Exec(`INSERT INTO traffic_stats(day, traffic, kind, n, bytes) VALUES(?,?,?,1,?)
		ON CONFLICT(day, traffic, kind) DO UPDATE SET n=n+1, bytes=bytes+excluded.bytes`,
		day, traffic, kind, nbytes)
}

func (s *Store) TrafficByDay(days int) ([]TrafficRow, error) {
	if days <= 0 {
		days = 7
	}
	since := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`SELECT day, traffic, kind, n, bytes FROM traffic_stats WHERE day>=? ORDER BY day DESC, traffic, kind`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrafficRow{}
	for rows.Next() {
		var r TrafficRow
		if err := rows.Scan(&r.Day, &r.Traffic, &r.Kind, &r.N, &r.Bytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CacheVerifyCounts 按验证状态统计条目数。空值（老记录）算 unverified：
// 阶段 6 之前落盘的内容本来就没有验证依据，不能因为列是空的就当成已验证。
func (s *Store) CacheVerifyCounts() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT CASE WHEN COALESCE(verify,'')='verified' THEN 'verified' ELSE 'unverified' END, COUNT(*)
		FROM cache_entries GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{VerifyVerified: 0, VerifyUnverified: 0}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// CacheOldest 按最后访问时间升序取，给容量淘汰用。
func (s *Store) CacheOldest(limit int) ([]CacheEntry, error) {
	rows, err := s.db.Query(`SELECT key, sha256, size, content_type, etag, last_modified, upstream,
		created_at, last_hit, hits, COALESCE(ttl,300) FROM cache_entries
		ORDER BY COALESCE(NULLIF(last_hit,0), created_at) ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheEntry
	for rows.Next() {
		var e CacheEntry
		if err := rows.Scan(&e.Key, &e.SHA256, &e.Size, &e.ContentType, &e.ETag, &e.LastModified,
			&e.Upstream, &e.CreatedAt, &e.LastHit, &e.Hits, &e.TTL); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CacheCountSHA 数还有几个键引用同一份内容。内容寻址下多个键可能共用
// 一个 blob 文件，删文件前必须确认没别人用。
func (s *Store) CacheCountSHA(sha string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM cache_entries WHERE sha256=?`, sha).Scan(&n)
	return n, err
}
