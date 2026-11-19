package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// 3.5.17 跨 Registry 副本映射。
//
// 与 F08 的 trusted_source_mappings 分层：
//   - trusted_source_mappings：某逻辑 Host 可由哪些 Source 提供（Host→Source）；
//   - trusted_registry_replicas：某权威仓库在其他 Registry 上的副本（仓库→仓库）。
//
// 规则（冻结）：
//   - 必须精确配置，不做同名隐式映射；
//   - v1 不支持 Repository 通配符；
//   - 同一副本 Registry 在同一权威 Repository 下只能有一行；
//   - 按 priority 固定排序，不按完成先后；
//   - 任何变更同事务递增权威 Registry Generation 并写耐久审计；
//   - 映射只授权尝试，不证明内容一致，最终仍以 Digest 验证为准。

var (
	// ErrReplicaExists 表示同一权威仓库下该副本 Registry 已登记。
	ErrReplicaExists = errors.New("registry replica already exists")
	// ErrReplicaMissing 表示副本映射不存在。
	ErrReplicaMissing = errors.New("registry replica not found")
)

// RegistryReplica 是一行跨 Registry 副本映射。
type RegistryReplica struct {
	AuthorityRegistry   string `json:"authority_registry"`
	AuthorityRepository string `json:"authority_repository"`
	ReplicaRegistry     string `json:"replica_registry"`
	ReplicaRepository   string `json:"replica_repository"`
	Priority            int    `json:"priority"`
	ValidFrom           int64  `json:"valid_from"`
	ValidUntil          int64  `json:"valid_until"`
	Enabled             bool   `json:"enabled"`
}

// ActiveAt 判断该映射在 t 时刻是否参与副本选择。
func (r RegistryReplica) ActiveAt(t time.Time) bool {
	if !r.Enabled {
		return false
	}
	ts := t.Unix()
	if r.ValidFrom != 0 && ts < r.ValidFrom {
		return false
	}
	if r.ValidUntil != 0 && ts >= r.ValidUntil {
		return false
	}
	return true
}

// RegistryReplicaAudit 是追加式审计行。
type RegistryReplicaAudit struct {
	ID                  int64  `json:"id"`
	AuthorityRegistry   string `json:"authority_registry"`
	AuthorityRepository string `json:"authority_repository"`
	ReplicaRegistry     string `json:"replica_registry"`
	Operation           string `json:"operation"`
	OldSnapshot         string `json:"old_snapshot"`
	NewSnapshot         string `json:"new_snapshot"`
	Actor               string `json:"actor"`
	CreatedAt           int64  `json:"created_at"`
}

func normalizeReplicaRegistry(raw string) (string, error) {
	if strings.Contains(raw, "*") {
		return "", fmt.Errorf("不支持通配符 Registry: %q", raw)
	}
	h, err := registryhost.Normalize(raw)
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", fmt.Errorf("registry 不能为空")
	}
	return h, nil
}

// normalizeRepository 做 v1 的精确仓库校验：不允许通配符与路径穿越。
func normalizeRepository(raw string) (string, error) {
	r := strings.TrimSpace(raw)
	if r == "" {
		return "", fmt.Errorf("repository 不能为空")
	}
	if strings.Contains(r, "*") {
		return "", fmt.Errorf("v1 不支持 Repository 通配符: %q", r)
	}
	if strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") || strings.Contains(r, "//") {
		return "", fmt.Errorf("repository 路径不合法: %q", r)
	}
	if strings.Contains(r, "..") {
		return "", fmt.Errorf("repository 不允许 ..: %q", r)
	}
	for _, ch := range r {
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '.' || ch == '_' || ch == '-' || ch == '/':
		default:
			return "", fmt.Errorf("repository 含非法字符 %q: %q", string(ch), r)
		}
	}
	return r, nil
}

// ValidateRegistryReplica 归一化并校验一行映射（导出给 API/迁移共用）。
func ValidateRegistryReplica(r RegistryReplica) (RegistryReplica, error) {
	auth, err := normalizeReplicaRegistry(r.AuthorityRegistry)
	if err != nil {
		return r, fmt.Errorf("authority_registry: %w", err)
	}
	rep, err := normalizeReplicaRegistry(r.ReplicaRegistry)
	if err != nil {
		return r, fmt.Errorf("replica_registry: %w", err)
	}
	if auth == rep {
		return r, fmt.Errorf("副本 Registry 不能等于权威 Registry")
	}
	arepo, err := normalizeRepository(r.AuthorityRepository)
	if err != nil {
		return r, fmt.Errorf("authority_repository: %w", err)
	}
	rrepo, err := normalizeRepository(r.ReplicaRepository)
	if err != nil {
		return r, fmt.Errorf("replica_repository: %w", err)
	}
	if r.Priority < 0 {
		return r, fmt.Errorf("priority 不能为负")
	}
	if r.ValidUntil != 0 && r.ValidFrom != 0 && r.ValidUntil <= r.ValidFrom {
		return r, fmt.Errorf("valid_until 必须大于 valid_from 或为 0")
	}
	r.AuthorityRegistry = auth
	r.ReplicaRegistry = rep
	r.AuthorityRepository = arepo
	r.ReplicaRepository = rrepo
	return r, nil
}

// ListRegistryReplicas 返回某权威 Registry 下的全部副本映射；authority 为空
// 返回全部。排序为 (authority_registry, authority_repository, priority, replica_registry)。
func (s *Store) ListRegistryReplicas(authority string) ([]RegistryReplica, error) {
	q := `SELECT authority_registry, authority_repository, replica_registry, replica_repository,
		priority, valid_from, valid_until, enabled FROM trusted_registry_replicas`
	args := []any{}
	if strings.TrimSpace(authority) != "" {
		h, err := normalizeReplicaRegistry(authority)
		if err != nil {
			return nil, err
		}
		q += ` WHERE authority_registry=?`
		args = append(args, h)
	}
	q += ` ORDER BY authority_registry, authority_repository, priority, replica_registry`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RegistryReplica{}
	for rows.Next() {
		var r RegistryReplica
		var enabled int
		if err := rows.Scan(&r.AuthorityRegistry, &r.AuthorityRepository, &r.ReplicaRegistry,
			&r.ReplicaRepository, &r.Priority, &r.ValidFrom, &r.ValidUntil, &enabled); err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveRegistryReplicas 返回 t 时刻某权威仓库的可用副本，按 priority 固定
// 排序（同 priority 用 replica_registry 稳定排序）。
func (s *Store) ActiveRegistryReplicas(authority, repository string, t time.Time) ([]RegistryReplica, error) {
	h, err := normalizeReplicaRegistry(authority)
	if err != nil {
		return nil, err
	}
	repo, err := normalizeRepository(repository)
	if err != nil {
		return nil, err
	}
	all, err := s.ListRegistryReplicas(h)
	if err != nil {
		return nil, err
	}
	out := make([]RegistryReplica, 0, len(all))
	for _, r := range all {
		if r.AuthorityRepository != repo {
			continue
		}
		if r.ActiveAt(t) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ReplicaGeneration 返回权威 Registry 的当前代次；没有行视为 0。
func (s *Store) ReplicaGeneration(authority string) (uint64, error) {
	h, err := normalizeReplicaRegistry(authority)
	if err != nil {
		return 0, err
	}
	var g int64
	err = s.db.QueryRow(`SELECT generation FROM trusted_replica_generation WHERE authority_registry=?`, h).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return uint64(g), nil
}

func bumpReplicaGeneration(tx *sql.Tx, authority string) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO trusted_replica_generation(authority_registry, generation)
		VALUES(?, 1) ON CONFLICT(authority_registry) DO NOTHING`, authority); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE trusted_replica_generation SET generation = generation + 1
		WHERE authority_registry = ?`, authority); err != nil {
		return 0, err
	}
	var g int64
	if err := tx.QueryRow(`SELECT generation FROM trusted_replica_generation WHERE authority_registry=?`, authority).Scan(&g); err != nil {
		return 0, err
	}
	return g, nil
}

func replicaSnapshot(r *RegistryReplica) string {
	if r == nil {
		return ""
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func (s *Store) replicaInTx(tx *sql.Tx, r RegistryReplica) (*RegistryReplica, error) {
	var out RegistryReplica
	var enabled int
	err := tx.QueryRow(`SELECT authority_registry, authority_repository, replica_registry,
		replica_repository, priority, valid_from, valid_until, enabled
		FROM trusted_registry_replicas
		WHERE authority_registry=? AND authority_repository=? AND replica_registry=?`,
		r.AuthorityRegistry, r.AuthorityRepository, r.ReplicaRegistry).
		Scan(&out.AuthorityRegistry, &out.AuthorityRepository, &out.ReplicaRegistry,
			&out.ReplicaRepository, &out.Priority, &out.ValidFrom, &out.ValidUntil, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.Enabled = enabled == 1
	return &out, nil
}

func auditReplica(tx *sql.Tx, op string, old, cur string, r RegistryReplica, actor string, now int64) error {
	_, err := tx.Exec(`INSERT INTO trusted_replica_audit
		(authority_registry, authority_repository, replica_registry, operation,
		 old_snapshot, new_snapshot, actor, created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		r.AuthorityRegistry, r.AuthorityRepository, r.ReplicaRegistry, op, old, cur, actor, now)
	return err
}

// PutRegistryReplica 新建或修改一行副本映射。
//
// createOnly=true 时已存在返回 ErrReplicaExists（POST 语义）；否则按 PUT 覆盖。
// 写入 + Generation 递增 + 审计快照在同一事务提交。
func (s *Store) PutRegistryReplica(r RegistryReplica, actor string, createOnly bool) (int64, error) {
	r, err := ValidateRegistryReplica(r)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	old, err := s.replicaInTx(tx, r)
	if err != nil {
		return 0, err
	}
	if old != nil && createOnly {
		return 0, ErrReplicaExists
	}
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	if _, err := tx.Exec(`INSERT INTO trusted_registry_replicas
		(authority_registry, authority_repository, replica_registry, replica_repository,
		 priority, valid_from, valid_until, enabled)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(authority_registry, authority_repository, replica_registry) DO UPDATE SET
			replica_repository=excluded.replica_repository, priority=excluded.priority,
			valid_from=excluded.valid_from, valid_until=excluded.valid_until,
			enabled=excluded.enabled`,
		r.AuthorityRegistry, r.AuthorityRepository, r.ReplicaRegistry, r.ReplicaRepository,
		r.Priority, r.ValidFrom, r.ValidUntil, enabled); err != nil {
		return 0, err
	}
	gen, err := bumpReplicaGeneration(tx, r.AuthorityRegistry)
	if err != nil {
		return 0, err
	}
	op := "update"
	if old == nil {
		op = "create"
	}
	if err := auditReplica(tx, op, replicaSnapshot(old), replicaSnapshot(&r), r, actor, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return gen, nil
}

// DeleteRegistryReplica 硬删除一行并写审计（删除前后快照都入库）。
func (s *Store) DeleteRegistryReplica(authority, repository, replica, actor string) (int64, error) {
	key := RegistryReplica{
		AuthorityRegistry:   authority,
		AuthorityRepository: repository,
		ReplicaRegistry:     replica,
		ReplicaRepository:   repository,
	}
	norm, err := ValidateRegistryReplica(key)
	if err != nil {
		return 0, err
	}
	key = norm
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	old, err := s.replicaInTx(tx, key)
	if err != nil {
		return 0, err
	}
	if old == nil {
		return 0, ErrReplicaMissing
	}
	if _, err := tx.Exec(`DELETE FROM trusted_registry_replicas
		WHERE authority_registry=? AND authority_repository=? AND replica_registry=?`,
		key.AuthorityRegistry, key.AuthorityRepository, key.ReplicaRegistry); err != nil {
		return 0, err
	}
	gen, err := bumpReplicaGeneration(tx, key.AuthorityRegistry)
	if err != nil {
		return 0, err
	}
	if err := auditReplica(tx, "delete", replicaSnapshot(old), "", *old, actor, time.Now().Unix()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return gen, nil
}

// ListRegistryReplicaAudit 返回审计（按权威 Registry 过滤可选，最新在前）。
func (s *Store) ListRegistryReplicaAudit(authority string, limit int) ([]RegistryReplicaAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, authority_registry, authority_repository, replica_registry, operation,
		COALESCE(old_snapshot,''), COALESCE(new_snapshot,''), actor, created_at
		FROM trusted_replica_audit`
	args := []any{}
	if strings.TrimSpace(authority) != "" {
		h, err := normalizeReplicaRegistry(authority)
		if err != nil {
			return nil, err
		}
		q += ` WHERE authority_registry=?`
		args = append(args, h)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RegistryReplicaAudit{}
	for rows.Next() {
		var a RegistryReplicaAudit
		if err := rows.Scan(&a.ID, &a.AuthorityRegistry, &a.AuthorityRepository, &a.ReplicaRegistry,
			&a.Operation, &a.OldSnapshot, &a.NewSnapshot, &a.Actor, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
