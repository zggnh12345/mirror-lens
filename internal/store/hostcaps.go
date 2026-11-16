package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// 3.5.14a 能力状态字符串。数据库 CHECK 与这里必须一致。
const (
	HostVerdictSupported    = "Supported"
	HostVerdictUnsupported  = "Unsupported"
	HostVerdictInconclusive = "Inconclusive"
	HostVerdictMismatch     = "Mismatch"

	HostStatusUnknown     = "Unknown"
	HostStatusSupported   = "Supported"
	HostStatusUnsupported = "Unsupported"
	HostStatusMismatch    = "Mismatch"
)

// SourceHost 是一个 Source 对一个逻辑 Registry Host 的能力记录。
type SourceHost struct {
	SourceID         int64  `json:"source_id"`
	Host             string `json:"host"`
	Enabled          bool   `json:"enabled"`
	LastVerdict      string `json:"last_verdict"`
	EffectiveStatus  string `json:"effective_status"`
	RegistryPathMode string `json:"registry_path_mode"`
	RegistryPrefix   bool   `json:"registry_prefix"`
	ProbeAt          int64  `json:"probe_at,omitempty"`
	PolicyGen        int64  `json:"policy_gen,omitempty"`
	Diagnostics      string `json:"diagnostics,omitempty"`
}

// verdictForCap 把旧三态/四态能力值映射为新状态。
// VerdictForCap 供离线迁移工具复用同一套 cap→状态映射。
func VerdictForCap(cap int) (last, effective string) { return verdictForCap(cap) }

func verdictForCap(cap int) (last, effective string) {
	switch cap {
	case RegistryCapYes:
		return HostVerdictSupported, HostStatusSupported
	case RegistryCapNo:
		return HostVerdictUnsupported, HostStatusUnsupported
	case RegistryCapMismatch:
		return HostVerdictMismatch, HostStatusMismatch
	default:
		return HostVerdictInconclusive, HostStatusUnknown
	}
}

// CapForStatusForTest 仅供测试把四态状态映射回旧 cap。
func CapForStatusForTest(status string) int { return capForStatus(status) }

func capForStatus(status string) int {
	switch status {
	case HostStatusSupported:
		return RegistryCapYes
	case HostStatusUnsupported:
		return RegistryCapNo
	case HostStatusMismatch:
		return RegistryCapMismatch
	default:
		return RegistryCapUnknown
	}
}

func validHostVerdict(s string) bool {
	switch s {
	case HostVerdictSupported, HostVerdictUnsupported, HostVerdictInconclusive, HostVerdictMismatch:
		return true
	}
	return false
}

func validHostStatus(s string) bool {
	switch s {
	case HostStatusUnknown, HostStatusSupported, HostStatusUnsupported, HostStatusMismatch:
		return true
	}
	return false
}

func normalizeHost(raw string) (string, error) {
	h, err := registryhost.Check(raw)
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", errors.New("Host 不能为空")
	}
	return h, nil
}

// hostCapsTx 读取事务内一行能力记录；不存在时返回 nil, nil。
func hostCapsTx(tx *sql.Tx, sourceID int64, host string) (*SourceHost, error) {
	var x SourceHost
	var enabled, prefix int
	err := tx.QueryRow(`SELECT source_id,host,enabled,last_verdict,effective_status,
		registry_path_mode,registry_prefix,diagnostics FROM source_host_caps WHERE source_id=? AND host=?`,
		sourceID, host).Scan(&x.SourceID, &x.Host, &enabled, &x.LastVerdict,
		&x.EffectiveStatus, &x.RegistryPathMode, &prefix, &x.Diagnostics)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	x.Enabled = enabled != 0
	x.RegistryPrefix = prefix != 0
	return &x, nil
}

// upsertHostCapsTx 写一行能力记录，默认不改变已有 enabled。
func upsertHostCapsTx(tx *sql.Tx, sourceID int64, host, last, effective, pathMode string, prefix, enabled int) error {
	_, err := tx.Exec(`INSERT INTO source_host_caps
(source_id,host,enabled,last_verdict,effective_status,registry_path_mode,registry_prefix)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(source_id,host) DO UPDATE SET
  last_verdict=excluded.last_verdict,
  effective_status=excluded.effective_status,
  registry_path_mode=excluded.registry_path_mode,
  registry_prefix=excluded.registry_prefix`,
		sourceID, host, enabled, last, effective, pathMode, prefix)
	return err
}

// ErrHostCapsNotFound 表示 Source 没有该 Host 的能力行。
var ErrHostCapsNotFound = errors.New("source host caps 不存在")

func hostCapsConn(ctx context.Context, conn *sql.Conn, sourceID int64, host string) (*SourceHost, error) {
	var x SourceHost
	var enabled, prefix int
	err := conn.QueryRowContext(ctx, `SELECT source_id,host,enabled,last_verdict,effective_status,
		registry_path_mode,registry_prefix,diagnostics FROM source_host_caps WHERE source_id=? AND host=?`,
		sourceID, host).Scan(&x.SourceID, &x.Host, &enabled, &x.LastVerdict,
		&x.EffectiveStatus, &x.RegistryPathMode, &prefix, &x.Diagnostics)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	x.Enabled = enabled != 0
	x.RegistryPrefix = prefix != 0
	return &x, nil
}

func beginImmediate(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func rollbackConn(ctx context.Context, conn *sql.Conn) {
	_, _ = conn.ExecContext(ctx, "ROLLBACK")
}

// attachSourceCaps 从 caps 派生 Source 的旧字段，仅供现有 UI/JSON 兼容；
// 数据库不再持久化这些字段，业务读写必须走 source_host_caps。
func derivedCap(h SourceHost) int {
	if !h.Enabled {
		return RegistryCapNo
	}
	return capForStatus(h.EffectiveStatus)
}

func (s *Store) attachSourceCaps(list []Source) error {
	if len(list) == 0 {
		return nil
	}
	rows, err := s.db.Query(`SELECT c.source_id,c.host,c.enabled,c.last_verdict,c.effective_status,
		c.registry_path_mode,c.registry_prefix,c.diagnostics,COALESCE(r.last_probe_at,0),COALESCE(p.policy_gen,0)
		FROM source_host_caps c
		LEFT JOIN registry_host_recovery r ON r.source_id=c.source_id AND r.family='registry' AND r.scope=c.host
		LEFT JOIN source_realm_policy p ON p.source_id=c.source_id AND p.host=c.host
		ORDER BY c.source_id,c.host`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[int64][]SourceHost{}
	for rows.Next() {
		var h SourceHost
		var enabled, prefix int
		if err := rows.Scan(&h.SourceID, &h.Host, &enabled, &h.LastVerdict,
			&h.EffectiveStatus, &h.RegistryPathMode, &prefix, &h.Diagnostics, &h.ProbeAt, &h.PolicyGen); err != nil {
			return err
		}
		h.Enabled = enabled != 0
		h.RegistryPrefix = prefix != 0
		byID[h.SourceID] = append(byID[h.SourceID], h)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range list {
		list[i].Hosts = byID[list[i].ID]
		list[i].DockerCap = RegistryCapUnknown
		list[i].GhcrCap = RegistryCapUnknown
		list[i].GhcrPrefix = false
		list[i].RepoHost = ""
		list[i].RegistryCap = RegistryCapUnknown
		list[i].RegistryPath = ""
		for _, h := range byID[list[i].ID] {
			switch h.Host {
			case "docker.io":
				list[i].DockerCap = derivedCap(h)
			case "ghcr.io":
				list[i].GhcrCap = derivedCap(h)
				list[i].GhcrPrefix = h.Enabled && h.RegistryPrefix
			default:
				if list[i].RepoHost == "" {
					list[i].RepoHost = h.Host
					list[i].RegistryCap = derivedCap(h)
					list[i].RegistryPath = h.RegistryPathMode
					list[i].RegistryProbeAt = h.ProbeAt
					list[i].RegistryProbeError = h.Diagnostics
				}
			}
		}
		// 旧 API 的“主 Host”兼容派生：没有 Foreign 时，按 caps 行显示
		// docker.io / ghcr.io（含 disabled，Host 关系仍在）；Unsupported 不算。
		if list[i].RepoHost == "" {
			for _, h := range byID[list[i].ID] {
				if h.Host == "docker.io" && h.EffectiveStatus == HostStatusSupported {
					list[i].RepoHost = "docker.io"
					break
				}
			}
			if list[i].RepoHost == "" {
				for _, h := range byID[list[i].ID] {
					if h.Host == "ghcr.io" && h.EffectiveStatus == HostStatusSupported {
						list[i].RepoHost = "ghcr.io"
						break
					}
				}
			}
		}
		list[i].RegistryIdentity = deriveRegistryIdentity(list[i])
	}
	return nil
}

// GetSourceHosts 返回一个 Source 的全部 Host 能力行。
func (s *Store) GetSourceHosts(sourceID int64) ([]SourceHost, error) {
	rows, err := s.db.Query(`SELECT c.source_id,c.host,c.enabled,c.last_verdict,c.effective_status,
		c.registry_path_mode,c.registry_prefix,c.diagnostics,COALESCE(r.last_probe_at,0),COALESCE(p.policy_gen,0)
		FROM source_host_caps c
		LEFT JOIN registry_host_recovery r ON r.source_id=c.source_id AND r.family='registry' AND r.scope=c.host
		LEFT JOIN source_realm_policy p ON p.source_id=c.source_id AND p.host=c.host
		WHERE c.source_id=? ORDER BY c.host`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceHost{}
	for rows.Next() {
		var x SourceHost
		var enabled, prefix int
		if err := rows.Scan(&x.SourceID, &x.Host, &enabled, &x.LastVerdict,
			&x.EffectiveStatus, &x.RegistryPathMode, &prefix, &x.Diagnostics, &x.ProbeAt, &x.PolicyGen); err != nil {
			return nil, err
		}
		x.Enabled = enabled != 0
		x.RegistryPrefix = prefix != 0
		out = append(out, x)
	}
	return out, rows.Err()
}

// GetSourceHost 读取一行；不存在返回 sql.ErrNoRows。
func (s *Store) GetSourceHost(sourceID int64, rawHost string) (*SourceHost, error) {
	host, err := normalizeHost(rawHost)
	if err != nil {
		return nil, err
	}
	var x SourceHost
	var enabled, prefix int
	err = s.db.QueryRow(`SELECT c.source_id,c.host,c.enabled,c.last_verdict,c.effective_status,
		c.registry_path_mode,c.registry_prefix,c.diagnostics,COALESCE(r.last_probe_at,0),COALESCE(p.policy_gen,0)
		FROM source_host_caps c
		LEFT JOIN registry_host_recovery r ON r.source_id=c.source_id AND r.family='registry' AND r.scope=c.host
		LEFT JOIN source_realm_policy p ON p.source_id=c.source_id AND p.host=c.host
		WHERE c.source_id=? AND c.host=?`,
		sourceID, host).Scan(&x.SourceID, &x.Host, &enabled, &x.LastVerdict,
		&x.EffectiveStatus, &x.RegistryPathMode, &prefix, &x.Diagnostics, &x.ProbeAt, &x.PolicyGen)
	if err != nil {
		return nil, err
	}
	x.Enabled = enabled != 0
	x.RegistryPrefix = prefix != 0
	return &x, nil
}

// SetSourceHostEnabled 是 14b 的 per-host 启停入口。
func (s *Store) SetSourceHostEnabled(sourceID int64, rawHost string, enabled bool) error {
	host, err := normalizeHost(rawHost)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := beginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer func() {
		if !committed {
			rollbackConn(ctx, conn)
		}
	}()
	var status string
	if err := conn.QueryRowContext(ctx, `SELECT status FROM sources WHERE id=?`, sourceID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceNotFound
		}
		return err
	}
	if status == "archived" {
		return ErrArchived
	}
	var last, effective, pathMode string
	var prefix, oldEnabled int
	err = conn.QueryRowContext(ctx, `SELECT enabled,last_verdict,effective_status,registry_path_mode,registry_prefix
		FROM source_host_caps WHERE source_id=? AND host=?`, sourceID, host).
		Scan(&oldEnabled, &last, &effective, &pathMode, &prefix)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrHostCapsNotFound
	}
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE source_host_caps SET enabled=? WHERE source_id=? AND host=?`,
		boolToInt(enabled), sourceID, host); err != nil {
		return err
	}
	if enabled {
		now := time.Now().Unix()
		if _, err := conn.ExecContext(ctx, `UPDATE source_realm_policy
			SET policy_gen=policy_gen+1, updated_at=? WHERE source_id=? AND host=?`, now, sourceID, host); err != nil {
			return err
		}
		if host != "docker.io" && host != "ghcr.io" {
			if _, err := conn.ExecContext(ctx, `DELETE FROM source_health_scope
				WHERE source_id=? AND family='registry' AND scope=?`, sourceID, host); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `UPDATE registry_host_recovery
				SET state='initial', next_probe_at=?, rec_failures=0, retry_failures=0, last_error='', updated_at=?
				WHERE source_id=? AND family='registry' AND scope=?`, now, now, sourceID, host); err != nil {
				return err
			}
		}
		if host == "docker.io" || host == "ghcr.io" {
			fam := "docker"
			if host == "ghcr.io" {
				fam = "ghcr"
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM source_health WHERE source_id=? AND family=?`, sourceID, fam); err != nil {
				return err
			}
		}
	}
	kind := "source_host_disabled"
	if enabled {
		kind = "source_host_enabled"
	}
	if err := recordSourceHostEventTx(ctx, conn, sourceID, host, kind, enabled); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// AddSourceHost 建立一条新的 Host 关系；policy 必须合法。
func (s *Store) AddSourceHost(sourceID int64, rawHost string, primary string, extra []string) error {
	host, err := normalizeHost(rawHost)
	if err != nil {
		return err
	}
	primary, extra, err = normalizePolicyRealms(primary, extra)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := beginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer func() {
		if !committed {
			rollbackConn(ctx, conn)
		}
	}()
	var status string
	if err := conn.QueryRowContext(ctx, `SELECT status FROM sources WHERE id=?`, sourceID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceNotFound
		}
		return err
	}
	if status == "archived" {
		return ErrArchived
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO source_host_caps
(source_id,host,enabled,last_verdict,effective_status,registry_path_mode,registry_prefix)
VALUES(?,?,1,?,?, '',0)
ON CONFLICT(source_id,host) DO UPDATE SET enabled=1`, sourceID, host, HostVerdictInconclusive, HostStatusUnknown); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO source_realm_policy
(source_id,host,primary_realm,extra_realms,policy_gen,updated_at)
VALUES(?,?,?,?,1,?)
ON CONFLICT(source_id,host) DO UPDATE SET
 primary_realm=excluded.primary_realm, extra_realms=excluded.extra_realms,
 policy_gen=source_realm_policy.policy_gen+1, updated_at=excluded.updated_at`,
		sourceID, host, primary, jsonString(extra), time.Now().Unix()); err != nil {
		return err
	}
	// 策略变更/首次建立都意味着该 Host 需要重新验证；已有旧结论不能继续用。
	now := time.Now().Unix()
	if _, err := conn.ExecContext(ctx, `UPDATE source_host_caps
		SET effective_status=?, registry_path_mode='' WHERE source_id=? AND host=?`,
		HostStatusUnknown, sourceID, host); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM source_health_scope
		WHERE source_id=? AND family='registry' AND scope=?`, sourceID, host); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE registry_host_recovery
		SET state='initial', next_probe_at=?, rec_failures=0, retry_failures=0, last_error='', updated_at=?
		WHERE source_id=? AND family='registry' AND scope=?`, now, now, sourceID, host); err != nil {
		return err
	}
	if host == "docker.io" || host == "ghcr.io" {
		fam := "docker"
		if host == "ghcr.io" {
			fam = "ghcr"
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM source_health WHERE source_id=? AND family=?`, sourceID, fam); err != nil {
			return err
		}
	}
	if host != "docker.io" && host != "ghcr.io" {
		if _, err := conn.ExecContext(ctx, `INSERT INTO registry_host_recovery
(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,retry_failures,last_error,updated_at)
VALUES(?, 'registry', ?, 'initial', COALESCE((SELECT gen FROM registry_host_gen WHERE host=?),0),0,0,?,0,0,'',?)
ON CONFLICT(source_id,family,scope) DO NOTHING`, sourceID, host, host, now, now); err != nil {
			return err
		}
	}
	if err := recordSourceHostEventTx(ctx, conn, sourceID, host, "source_host_added", true); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// RemoveSourceHost 删除一条 Host 关系。
func (s *Store) RemoveSourceHost(sourceID int64, rawHost string) error {
	host, err := normalizeHost(rawHost)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := beginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer func() {
		if !committed {
			rollbackConn(ctx, conn)
		}
	}()
	var status string
	if err := conn.QueryRowContext(ctx, `SELECT status FROM sources WHERE id=?`, sourceID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceNotFound
		}
		return err
	}
	if status == "archived" {
		return ErrArchived
	}
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM source_host_caps WHERE source_id=? AND host=?`, sourceID, host).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrHostCapsNotFound
		}
		return err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM source_host_caps WHERE source_id=? AND host=?`, sourceID, host); err != nil {
		return err
	}
	// Docker/GHCR 的 source_health 是源级协议族计数；移除该 Host 后旧失败
	// 不应在以后重新添加时继续挡路。
	if host == "docker.io" {
		if _, err := conn.ExecContext(ctx, `DELETE FROM source_health WHERE source_id=? AND family='docker'`, sourceID); err != nil {
			return err
		}
	}
	if host == "ghcr.io" {
		if _, err := conn.ExecContext(ctx, `DELETE FROM source_health WHERE source_id=? AND family='ghcr'`, sourceID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM source_realm_policy WHERE source_id=? AND host=?`, sourceID, host); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM source_health_scope WHERE source_id=? AND family='registry' AND scope=?`, sourceID, host); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM registry_host_recovery WHERE source_id=? AND family='registry' AND scope=?`, sourceID, host); err != nil {
		return err
	}
	if err := recordSourceHostEventTx(ctx, conn, sourceID, host, "source_host_removed", false); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// ResetSourceHostVerdict 管理员显式重置 Mismatch/Unknown。
func (s *Store) ResetSourceHostVerdict(sourceID int64, rawHost string) error {
	host, err := normalizeHost(rawHost)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := beginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer func() {
		if !committed {
			rollbackConn(ctx, conn)
		}
	}()
	var status string
	if err := conn.QueryRowContext(ctx, `SELECT status FROM sources WHERE id=?`, sourceID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceNotFound
		}
		return err
	}
	if status == "archived" {
		return ErrArchived
	}
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM source_host_caps WHERE source_id=? AND host=?`, sourceID, host).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrHostCapsNotFound
		}
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE source_host_caps
SET effective_status=?, registry_path_mode=''
WHERE source_id=? AND host=?`, HostStatusUnknown, sourceID, host); err != nil {
		return err
	}
	// policy_gen 同时充当探测 epoch：重置后，重置前启动的探测结果必须失效。
	if _, err := conn.ExecContext(ctx, `UPDATE source_realm_policy
SET policy_gen=policy_gen+1, updated_at=? WHERE source_id=? AND host=?`, time.Now().Unix(), sourceID, host); err != nil {
		return err
	}
	// 重置必须同时清掉当前代的连续失败，否则 candidate 仍会被 scoped 健康门禁挡住。
	if _, err := conn.ExecContext(ctx, `DELETE FROM source_health_scope
WHERE source_id=? AND family='registry' AND scope=?`, sourceID, host); err != nil {
		return err
	}
	if host == "docker.io" || host == "ghcr.io" {
		fam := "docker"
		if host == "ghcr.io" {
			fam = "ghcr"
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM source_health WHERE source_id=? AND family=?`, sourceID, fam); err != nil {
			return err
		}
	}
	now := time.Now().Unix()
	if _, err := conn.ExecContext(ctx, `UPDATE registry_host_recovery
SET state='initial', next_probe_at=?, rec_failures=0, retry_failures=0, last_error='', updated_at=?
WHERE source_id=? AND family='registry' AND scope=?`, now, now, sourceID, host); err != nil {
		return err
	}
	if err := recordSourceHostEventTx(ctx, conn, sourceID, host, "source_host_verdict_reset", true); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func recordSourceHostEventTx(ctx context.Context, conn *sql.Conn, sourceID int64, host, kind string, ok bool) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO source_events(source_id,kind,ts,ok,neutral,ms,code)
VALUES(?,?,?,?,1,0,0)`, sourceID, kind, time.Now().Unix(), boolToInt(ok))
	return err
}

func jsonString(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}
