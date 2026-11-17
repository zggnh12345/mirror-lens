package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MetaCacheEpoch 是 Registry 缓存键命名空间代次的 meta 键。
//
// 3.5.15 起所有 Registry 缓存键带 epoch：docker:m:<epoch>:… / docker:b:<epoch>:…。
// epoch 是持久化状态，不是进程内变量；升级与每次新增私有认证 Host 时递增。
// epoch=0 表示「从未启用私有认证」，键保持 3.5.14c 的旧格式，部署零变化。
const MetaCacheEpoch = "registry_cache_epoch"

// CacheEpoch 读取当前缓存命名空间代次；缺省为 0。
func (s *Store) CacheEpoch() (int64, error) {
	v, err := s.MetaGet(MetaCacheEpoch)
	if err != nil {
		return 0, err
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("meta.%s 非法: %q", MetaCacheEpoch, v)
	}
	return n, nil
}

// BumpCacheEpoch 在单事务内把 epoch 加一并返回新值。
// 递增是全局失效：旧 epoch 的条目立刻不可达，物理清理由缓存管理路径负责。
func (s *Store) BumpCacheEpoch() (int64, error) {
	return s.setCacheEpoch(func(cur int64) int64 { return cur + 1 })
}

// EnsureCacheEpoch 保证 epoch 至少为 min；不足时一次性抬到 min。
// 启用私有认证时必须 >= 1：epoch=0 的键空间与 3.5.14c 相同，
// 不能用来隔离升级前的私有内容。
func (s *Store) EnsureCacheEpoch(min int64) (int64, error) {
	return s.setCacheEpoch(func(cur int64) int64 {
		if cur < min {
			return min
		}
		return cur
	})
}

func (s *Store) setCacheEpoch(next func(int64) int64) (int64, error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var raw string
	err = conn.QueryRowContext(ctx, `SELECT v FROM meta WHERE k=?`, MetaCacheEpoch).Scan(&raw)
	cur := int64(0)
	if err == nil {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			n, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil || n < 0 {
				return 0, fmt.Errorf("meta.%s 非法: %q", MetaCacheEpoch, raw)
			}
			cur = n
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	nextVal := next(cur)
	if nextVal < 0 {
		return 0, errors.New("缓存 epoch 不能为负")
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES(?,?)
ON CONFLICT(k) DO UPDATE SET v=excluded.v`, MetaCacheEpoch, strconv.FormatInt(nextVal, 10)); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	committed = true
	return nextVal, nil
}

// RegistryCacheKey 给 Registry 缓存键加 epoch 前缀。
// epoch=0 保持旧格式（docker:manifest:… / docker:blob:…），
// 这样未启用私有认证的部署与 3.5.14c 完全兼容。
//
// 键格式（epoch>0）：
//
//	docker:m:<epoch>:<origin>/<name>:<ref>|accept=…
//	docker:b:<epoch>:sha256:…
func RegistryCacheKey(epoch int64, kind, rest string) string {
	if epoch <= 0 {
		return "docker:" + kind + ":" + rest
	}
	short := "m"
	if kind == "blob" {
		short = "b"
	}
	return "docker:" + short + ":" + strconv.FormatInt(epoch, 10) + ":" + rest
}

// RegistryCacheKind 从键里解析 Registry 缓存类型（manifest/blob）。
// 同时接受 3.5.14c 旧格式与 3.5.15 epoch 格式；非 Registry 键返回空。
func RegistryCacheKind(key string) string {
	switch {
	case strings.HasPrefix(key, "docker:manifest:"):
		return "manifest"
	case strings.HasPrefix(key, "docker:blob:"):
		return "blob"
	case strings.HasPrefix(key, "docker:m:"):
		return "manifest"
	case strings.HasPrefix(key, "docker:b:"):
		return "blob"
	}
	return ""
}

// RegistryCacheKeyEpoch 解析键里的 epoch；旧格式返回 0，非 Registry 键返回 -1。
func RegistryCacheKeyEpoch(key string) int64 {
	if RegistryCacheKind(key) == "" {
		return -1
	}
	if strings.HasPrefix(key, "docker:manifest:") || strings.HasPrefix(key, "docker:blob:") {
		return 0
	}
	parts := strings.SplitN(key, ":", 4)
	if len(parts) < 4 {
		return -1
	}
	n, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// CacheKeyPrefixesBefore 返回「属于更早 epoch」的 Registry 键前缀集合。
// 旧格式（无 epoch）也算更早：升级后必须让它不可达。
func CacheKeyPrefixesBefore(epoch int64) []string {
	var out []string
	if epoch > 0 {
		out = append(out, "docker:manifest:", "docker:blob:")
	}
	// epoch=0 是旧格式，已由上面两条精确前缀覆盖；这里从 1 开始，
	// 避免 "docker:" 这种宽前缀把当前 epoch 的键一起删掉。
	for e := int64(1); e < epoch; e++ {
		// manifest 与 blob 两个命名空间都要清：只清 manifest 会留下
		// 私有 blob 的索引（S14 要求物理条目消失）。
		out = append(out, "docker:m:"+strconv.FormatInt(e, 10)+":",
			"docker:b:"+strconv.FormatInt(e, 10)+":")
	}
	return out
}

// RegistryCacheKeyPrefix 返回某 epoch 的 Registry 键前缀。
func RegistryCacheKeyPrefix(epoch int64) string {
	if epoch <= 0 {
		return "docker:"
	}
	return "docker:m:" + strconv.FormatInt(epoch, 10) + ":"
}

// CacheEpochSweep 物理清理早于当前 epoch 的 Registry 缓存索引。
//
// 限速、分批、可重试：每次最多删 limit 行，返回删除条数；调用方（janitor）
// 循环到 0 为止。物理文件由 janitor 的常规回收处理——索引删掉后文件变成
// 孤儿，下一轮 orphan 扫描会删掉。
// CacheEpochKeysBefore 返回早于 epoch 的 Registry 缓存键，供 GC 删索引+物理文件。
func (s *Store) CacheEpochKeysBefore(epoch int64, limit int) ([]string, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	prefixes := CacheKeyPrefixesBefore(epoch)
	if len(prefixes) == 0 {
		return nil, nil
	}
	var (
		conds []string
		args  []any
	)
	for _, p := range prefixes {
		conds = append(conds, "key LIKE ?")
		args = append(args, p+"%")
	}
	args = append(args, limit)
	q := `SELECT key FROM cache_entries WHERE (` + strings.Join(conds, " OR ") + `) LIMIT ?`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CacheEpochSweep 只删索引；物理文件由 GC 的 DeleteKeys/Orphans 收走。
// 新代码应优先用 CacheEpochKeysBefore + GC.DeleteKeys，保证索引和文件同批处理。
func (s *Store) CacheEpochSweep(epoch int64, limit int) (int, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	prefixes := CacheKeyPrefixesBefore(epoch)
	if len(prefixes) == 0 {
		return 0, nil
	}
	var (
		conds []string
		args  []any
	)
	for _, p := range prefixes {
		conds = append(conds, "key LIKE ?")
		args = append(args, p+"%")
	}
	args = append(args, limit)
	q := `DELETE FROM cache_entries WHERE key IN (
		SELECT key FROM cache_entries WHERE (` + strings.Join(conds, " OR ") + `) LIMIT ?)`
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CacheEpochStats 汇总各 epoch 的条目数，供验收与面板展示。
type CacheEpochStat struct {
	Epoch int64 `json:"epoch"`
	Keys  int   `json:"keys"`
}

func (s *Store) CacheEpochStats(current int64) ([]CacheEpochStat, error) {
	rows, err := s.db.Query(`SELECT key FROM cache_entries WHERE key LIKE 'docker:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[int64]int{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		e := RegistryCacheKeyEpoch(k)
		if e < 0 {
			continue
		}
		counts[e]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]CacheEpochStat, 0, len(counts))
	for e, n := range counts {
		out = append(out, CacheEpochStat{Epoch: e, Keys: n})
	}
	// 稳定输出：epoch 升序
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Epoch < out[i].Epoch {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	_ = current
	return out, nil
}

// AuditCacheEpoch 记录一次 epoch 切换（运维审计）。
func (s *Store) AuditCacheEpoch(epoch int64, actor, reason string) {
	s.Audit(AuditEvent{
		TS:     time.Now().Unix(),
		User:   actor,
		Action: "cache_epoch_bump",
		Target: MetaCacheEpoch,
		Detail: fmt.Sprintf("epoch=%d reason=%s", epoch, reason),
		OK:     true,
	})
}

// metaCacheEpochInit 记录「3.5.15 已做过一次升级隔离」。
// 没有它的话每次启动都会递增 epoch，等于每次重启清空全部缓存。
const metaCacheEpochInit = "registry_cache_epoch_initialized"

// InitCacheEpoch 在 3.5.15 首次启动时递增一次 epoch（0 → 1）。
//
// 仅从本版开始禁止写入不能证明升级前的私有内容已消失，所以升级必须切一次
// 命名空间；标记位保证只切一次。返回 (epoch, bumped, err)。
func (s *Store) InitCacheEpoch() (int64, bool, error) {
	marker, err := s.MetaGet(metaCacheEpochInit)
	if err != nil {
		return 0, false, err
	}
	if marker == "1" {
		e, err := s.CacheEpoch()
		return e, false, err
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var raw string
	err = conn.QueryRowContext(ctx, `SELECT v FROM meta WHERE k=?`, MetaCacheEpoch).Scan(&raw)
	cur := int64(0)
	if err == nil {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			n, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil || n < 0 {
				return 0, false, fmt.Errorf("meta.%s 非法: %q", MetaCacheEpoch, raw)
			}
			cur = n
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	next := cur
	if next < 1 {
		next = 1
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES(?,?)
ON CONFLICT(k) DO UPDATE SET v=excluded.v`, MetaCacheEpoch, strconv.FormatInt(next, 10)); err != nil {
		return 0, false, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES(?,'1')
ON CONFLICT(k) DO UPDATE SET v='1'`, metaCacheEpochInit); err != nil {
		return 0, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, false, err
	}
	committed = true
	return next, next != cur, nil
}
