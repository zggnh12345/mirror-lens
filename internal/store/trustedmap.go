package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// 3.5.17 F08：显式可信映射。
//
// 运行时表 trusted_source_mappings 只保存当前配置；每次变更都必须在
// 同一个事务里递增 trusted_mapping_generation 并写 trusted_mapping_audit。
// 审计是耐久事实，不能用异步 Store.Audit 队列代替（队列满即丢）。

// ErrMappingExists / ErrMappingMissing 是管理 API 需要的可区分错误。
var (
	ErrMappingExists  = errors.New("trusted mapping already exists")
	ErrMappingMissing = errors.New("trusted mapping not found")
)

// TrustedMapping 是一行运行时映射。
type TrustedMapping struct {
	RegistryHost string `json:"registry_host"`
	SourceID     int64  `json:"source_id"`
	Priority     int    `json:"priority"`
	ValidFrom    int64  `json:"valid_from"`
	ValidUntil   int64  `json:"valid_until"`
	Enabled      bool   `json:"enabled"`
}

// ActiveAt 判断该映射在 t 时刻是否参与候选选择。
// valid_from=0 立即生效，valid_until=0 永久有效。
func (m TrustedMapping) ActiveAt(t time.Time) bool {
	if !m.Enabled {
		return false
	}
	ts := t.Unix()
	if m.ValidFrom != 0 && ts < m.ValidFrom {
		return false
	}
	if m.ValidUntil != 0 && ts >= m.ValidUntil {
		return false
	}
	return true
}

// TrustedMappingAudit 是追加式审计行。
type TrustedMappingAudit struct {
	ID           int64  `json:"id"`
	RegistryHost string `json:"registry_host"`
	SourceID     int64  `json:"source_id"`
	Operation    string `json:"operation"`
	OldSnapshot  string `json:"old_snapshot"`
	NewSnapshot  string `json:"new_snapshot"`
	Actor        string `json:"actor"`
	CreatedAt    int64  `json:"created_at"`
}

// normalizeMappingHost 复用一个规范化入口，避免 API 与迁移各自实现一份。
func normalizeMappingHost(raw string) (string, error) {
	h, err := registryhost.Normalize(raw)
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", fmt.Errorf("registry_host 不能为空")
	}
	return h, nil
}

// ListTrustedMappings 返回映射；host 为空时返回全部（按 Host、priority 排序）。
func (s *Store) ListTrustedMappings(host string) ([]TrustedMapping, error) {
	q := `SELECT registry_host, source_id, priority, valid_from, valid_until, enabled
		FROM trusted_source_mappings`
	args := []any{}
	if host != "" {
		h, err := normalizeMappingHost(host)
		if err != nil {
			return nil, err
		}
		q += ` WHERE registry_host = ?`
		args = append(args, h)
	}
	q += ` ORDER BY registry_host, priority, source_id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrustedMapping{}
	for rows.Next() {
		var m TrustedMapping
		var enabled int
		if err := rows.Scan(&m.RegistryHost, &m.SourceID, &m.Priority, &m.ValidFrom, &m.ValidUntil, &enabled); err != nil {
			return nil, err
		}
		m.Enabled = enabled == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// ActiveTrustedMappings 返回 t 时刻参与候选的映射（有效期与启用位都过滤）。
func (s *Store) ActiveTrustedMappings(host string, t time.Time) ([]TrustedMapping, error) {
	all, err := s.ListTrustedMappings(host)
	if err != nil {
		return nil, err
	}
	out := make([]TrustedMapping, 0, len(all))
	for _, m := range all {
		if m.ActiveAt(t) {
			out = append(out, m)
		}
	}
	return out, nil
}

// MappingGeneration 返回某 Host 的当前代次；没有行视为 0。
func (s *Store) MappingGeneration(host string) (uint64, error) {
	h, err := normalizeMappingHost(host)
	if err != nil {
		return 0, err
	}
	var g int64
	err = s.db.QueryRow(`SELECT generation FROM trusted_mapping_generation WHERE registry_host=?`, h).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return uint64(g), nil
}

// MappingGenerations 返回全部 Host 的当前代次快照（RequestScope 用）。
func (s *Store) MappingGenerations() (map[string]uint64, error) {
	rows, err := s.db.Query(`SELECT registry_host, generation FROM trusted_mapping_generation`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var h string
		var g int64
		if err := rows.Scan(&h, &g); err != nil {
			return nil, err
		}
		out[h] = uint64(g)
	}
	return out, rows.Err()
}

// mappingSnapshot 把一行序列化成审计快照。空行（不存在）用 ""。
func mappingSnapshot(m *TrustedMapping) string {
	if m == nil {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// bumpGeneration 在事务内递增并返回新代次。调用方持 tx。
func bumpGeneration(tx *sql.Tx, host string) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO trusted_mapping_generation(registry_host, generation)
		VALUES(?, 1) ON CONFLICT(registry_host) DO NOTHING`, host); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE trusted_mapping_generation SET generation = generation + 1
		WHERE registry_host = ?`, host); err != nil {
		return 0, err
	}
	var g int64
	if err := tx.QueryRow(`SELECT generation FROM trusted_mapping_generation WHERE registry_host=?`, host).Scan(&g); err != nil {
		return 0, err
	}
	return g, nil
}

func (s *Store) mappingInTx(tx *sql.Tx, host string, sourceID int64) (*TrustedMapping, error) {
	var m TrustedMapping
	var enabled int
	err := tx.QueryRow(`SELECT registry_host, source_id, priority, valid_from, valid_until, enabled
		FROM trusted_source_mappings WHERE registry_host=? AND source_id=?`, host, sourceID).
		Scan(&m.RegistryHost, &m.SourceID, &m.Priority, &m.ValidFrom, &m.ValidUntil, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.Enabled = enabled == 1
	return &m, nil
}

func auditMapping(tx *sql.Tx, op string, old, cur string, host, actor string, sourceID int64, now int64) error {
	_, err := tx.Exec(`INSERT INTO trusted_mapping_audit
		(registry_host, source_id, operation, old_snapshot, new_snapshot, actor, created_at)
		VALUES(?,?,?,?,?,?,?)`, host, sourceID, op, old, cur, actor, now)
	return err
}

// PutTrustedMapping 新建或修改一行映射。
//
// createOnly=true 时已存在返回 ErrMappingExists（POST 语义）；
// 否则按 PUT 语义覆盖。所有写入（含 Source 外键校验、代次、审计）
// 都在同一个事务里，失败不留半写。
func (s *Store) PutTrustedMapping(m TrustedMapping, actor string, createOnly bool) (int64, error) {
	host, err := normalizeMappingHost(m.RegistryHost)
	if err != nil {
		return 0, err
	}
	if m.SourceID <= 0 {
		return 0, fmt.Errorf("source_id 必须存在")
	}
	if m.Priority < 0 {
		return 0, fmt.Errorf("priority 不能为负")
	}
	if m.ValidUntil != 0 && m.ValidFrom != 0 && m.ValidUntil <= m.ValidFrom {
		return 0, fmt.Errorf("valid_until 必须大于 valid_from 或为 0")
	}
	m.RegistryHost = host
	now := time.Now().Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM sources WHERE id=?`, m.SourceID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, fmt.Errorf("source_id=%d 不存在", m.SourceID)
	}
	old, err := s.mappingInTx(tx, host, m.SourceID)
	if err != nil {
		return 0, err
	}
	if old != nil && createOnly {
		return 0, ErrMappingExists
	}
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	if _, err := tx.Exec(`INSERT INTO trusted_source_mappings
		(registry_host, source_id, priority, valid_from, valid_until, enabled)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(registry_host, source_id) DO UPDATE SET
			priority=excluded.priority, valid_from=excluded.valid_from,
			valid_until=excluded.valid_until, enabled=excluded.enabled`,
		host, m.SourceID, m.Priority, m.ValidFrom, m.ValidUntil, enabled); err != nil {
		return 0, err
	}
	gen, err := bumpGeneration(tx, host)
	if err != nil {
		return 0, err
	}
	op := "update"
	if old == nil {
		op = "create"
	}
	if err := auditMapping(tx, op, mappingSnapshot(old), mappingSnapshot(&m), host, actor, m.SourceID, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return gen, nil
}

// DeleteTrustedMapping 硬删除一行并写审计（删除前后快照都入库）。
func (s *Store) DeleteTrustedMapping(rawHost string, sourceID int64, actor string) (int64, error) {
	host, err := normalizeMappingHost(rawHost)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	old, err := s.mappingInTx(tx, host, sourceID)
	if err != nil {
		return 0, err
	}
	if old == nil {
		return 0, ErrMappingMissing
	}
	if _, err := tx.Exec(`DELETE FROM trusted_source_mappings WHERE registry_host=? AND source_id=?`, host, sourceID); err != nil {
		return 0, err
	}
	gen, err := bumpGeneration(tx, host)
	if err != nil {
		return 0, err
	}
	if err := auditMapping(tx, "delete", mappingSnapshot(old), "", host, actor, sourceID, time.Now().Unix()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return gen, nil
}

// ListTrustedMappingAudit 返回审计（按 Host 过滤可选，最新在前）。
func (s *Store) ListTrustedMappingAudit(rawHost string, limit int) ([]TrustedMappingAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, registry_host, source_id, operation, COALESCE(old_snapshot,''), COALESCE(new_snapshot,''), actor, created_at
		FROM trusted_mapping_audit`
	args := []any{}
	if rawHost != "" {
		h, err := normalizeMappingHost(rawHost)
		if err != nil {
			return nil, err
		}
		q += ` WHERE registry_host=?`
		args = append(args, h)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrustedMappingAudit{}
	for rows.Next() {
		var a TrustedMappingAudit
		if err := rows.Scan(&a.ID, &a.RegistryHost, &a.SourceID, &a.Operation, &a.OldSnapshot, &a.NewSnapshot, &a.Actor, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
