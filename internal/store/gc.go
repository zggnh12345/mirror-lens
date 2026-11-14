package store

import (
	"database/sql"
	"strings"
	"time"
)

// ---- 批量删除与对账 ----

// CacheDeleteMany 在一个事务里删一批索引，返回被删条目（含 sha 与大小），
// 供上层做引用计数与物理删除。部分键不存在不算错。
func (s *Store) CacheDeleteMany(keys []string) ([]CacheEntry, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var out []CacheEntry
	for _, k := range keys {
		var e CacheEntry
		err := tx.QueryRow(`SELECT key, sha256, size, content_type, etag, last_modified, upstream,
			created_at, last_hit, hits, COALESCE(ttl,300) FROM cache_entries WHERE key=?`, k).
			Scan(&e.Key, &e.SHA256, &e.Size, &e.ContentType, &e.ETag, &e.LastModified, &e.Upstream,
				&e.CreatedAt, &e.LastHit, &e.Hits, &e.TTL)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM cache_entries WHERE key=?`, k); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// CacheKeysByPrefix 取某个协议（按缓存键前缀）的全部条目。
func (s *Store) CacheKeysByPrefix(prefix string) ([]CacheEntry, error) {
	rows, err := s.db.Query(`SELECT key, sha256, size, content_type, etag, last_modified, upstream,
		created_at, last_hit, hits, COALESCE(ttl,300) FROM cache_entries WHERE key LIKE ? ESCAPE '\'`,
		escapeLike(prefix)+"%")
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

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// CacheExpired 取「已过期、且至少 grace 秒没人碰过」的条目。
// 过期没人访问不代表可以删：低频资源删了就得整份重下。
func (s *Store) CacheExpired(now int64, grace int64, limit int) ([]CacheEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT key, sha256, size, content_type, etag, last_modified, upstream,
		created_at, last_hit, hits, COALESCE(ttl,300) FROM cache_entries
		WHERE COALESCE(ttl,300) > 0
		  AND created_at + COALESCE(ttl,300) < ?
		  AND MAX(COALESCE(last_hit,0), created_at) < ?
		ORDER BY COALESCE(NULLIF(last_hit,0), created_at) ASC LIMIT ?`, now, now-grace, limit)
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

// CacheRefs 返回每个内容哈希被多少个键引用，对账用。
func (s *Store) CacheRefs() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT sha256, COUNT(*) FROM cache_entries GROUP BY sha256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var sha string
		var n int64
		if err := rows.Scan(&sha, &n); err != nil {
			return nil, err
		}
		out[sha] = n
	}
	return out, rows.Err()
}

// CacheRef 返回某个 blob 被多少条索引引用。
// 删文件前用它做最后确认：CacheRefs 是整表快照，之后才写进的条目不在里面，
// 只看快照会把「刚提交、还没入库」的 blob 当孤儿删掉。
func (s *Store) CacheRef(sha string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM cache_entries WHERE sha256 = ?`, sha).Scan(&n)
	return n, err
}

// CacheAll 取全部索引，对账时逐条核对文件是否存在。
func (s *Store) CacheAll(limit int) ([]CacheEntry, error) {
	if limit <= 0 {
		limit = 100000
	}
	return s.CacheList(limit)
}

// ---- 删不掉的文件排队重试 ----

func (s *Store) PendingBlobAdd(sha string) error {
	_, err := s.db.Exec(`INSERT INTO pending_blobs(sha, queued_at) VALUES(?,?)
		ON CONFLICT(sha) DO NOTHING`, sha, time.Now().Unix())
	return err
}

func (s *Store) PendingBlobs(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT sha FROM pending_blobs ORDER BY queued_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

func (s *Store) PendingBlobDone(sha string) error {
	_, err := s.db.Exec(`DELETE FROM pending_blobs WHERE sha=?`, sha)
	return err
}

// ---- 小配置（水位告警状态等） ----

func (s *Store) MetaGet(k string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) MetaSet(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}
