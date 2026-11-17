package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// SourceEventPolicyChange 是策略变更事件。它写进 source_events 以便和
// 策略行同一事务提交，但健康统计必须把它排除，否则一次管理操作会占用
// 最近样本并稀释源的可用率。
const SourceEventPolicyChange = "policy_change"

const maxPolicyExtraRealms = 3

// RealmPolicy 是 source_realm_policy 的一行。
type RealmPolicy struct {
	SourceID     int64    `json:"source_id"`
	Host         string   `json:"host"`
	PrimaryRealm string   `json:"primary_realm"`
	ExtraRealms  []string `json:"extra_realms"`
	PolicyGen    int64    `json:"policy_gen"`
	UpdatedAt    int64    `json:"updated_at"`
}

// StorePolicy 是运行时唯一允许的 sourceID>0 策略来源。
// 它不读旧 Host 白名单，也不做 Host 猜测。
type StorePolicy struct {
	s *Store
}

func NewStorePolicy(s *Store) *StorePolicy {
	return &StorePolicy{s: s}
}

func (p *StorePolicy) Resolve(ctx context.Context, sourceID int64, host string) (v2auth.PolicySnapshot, error) {
	if sourceID < 0 {
		return v2auth.PolicySnapshot{}, fmt.Errorf("%w: source_id=%d", v2auth.ErrPolicyMissing, sourceID)
	}
	if sourceID == 0 {
		// 官方直连策略必须带真实 generation：静态 OfficialPolicy 的
		// CredGen 恒为 0，会让轮换后的凭据继续通过门禁（P0-1）。
		return p.resolveOfficial(ctx, host)
	}
	logical, err := normalizePolicyHost(host)
	if err != nil {
		return v2auth.PolicySnapshot{}, fmt.Errorf("%w: %v", v2auth.ErrPolicyMissing, err)
	}

	var primary, extraJSON string
	var policyGen int64
	err = p.s.db.QueryRowContext(ctx, `SELECT primary_realm, extra_realms, policy_gen
		FROM source_realm_policy WHERE source_id=? AND host=?`, sourceID, logical).
		Scan(&primary, &extraJSON, &policyGen)
	if errors.Is(err, sql.ErrNoRows) {
		return v2auth.PolicySnapshot{}, fmt.Errorf("%w: source_id=%d host=%s", v2auth.ErrPolicyMissing, sourceID, logical)
	}
	if err != nil {
		return v2auth.PolicySnapshot{}, err
	}
	primary, extras, err := validatePolicyRealms(primary, extraJSON)
	if err != nil {
		return v2auth.PolicySnapshot{}, fmt.Errorf("source_id=%d host=%s 的策略非法: %w", sourceID, logical, err)
	}
	var credGen int64
	if err := p.s.db.QueryRowContext(ctx, `SELECT gen FROM registry_host_gen WHERE host=?`, logical).Scan(&credGen); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return v2auth.PolicySnapshot{}, err
	}
	return v2auth.PolicySnapshot{
		SourceID:      sourceID,
		LogicalHost:   logical,
		PolicyGen:     policyGen,
		CredGen:       credGen,
		Official:      false,
		AllowSameHost: false,
		Scheme:        "https",
		Realms:        append([]string{primary}, extras...),
	}, nil
}

// resolveOfficial 是官方直连策略的唯一生产实现。
//
// Host 来自已解析 plan，可能是测试/内网地址；这里只做基本收敛，不做
// registryhost 白名单校验。generation 读取失败（DB 错误）必须 fail closed；
// 行缺失时 CredGenKnown=false，由 AuthorizeCredential 拒绝凭据认证，
// 匿名路径不受影响。
func (p *StorePolicy) resolveOfficial(ctx context.Context, host string) (v2auth.PolicySnapshot, error) {
	logical := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "/"))
	if logical == "" {
		return v2auth.PolicySnapshot{}, fmt.Errorf("%w: 官方 Host 为空", v2auth.ErrPolicyMissing)
	}
	snap := v2auth.PolicySnapshot{
		SourceID:      0,
		LogicalHost:   logical,
		Official:      true,
		AllowSameHost: true,
		Scheme:        "https",
		Realms:        append([]string(nil), v2auth.DefaultRealmsFor(logical)...),
	}
	var gen int64
	err := p.s.db.QueryRowContext(ctx, `SELECT gen FROM registry_host_gen WHERE host=?`, logical).Scan(&gen)
	switch {
	case err == nil:
		snap.CredGen = gen
		snap.CredGenKnown = true
	case errors.Is(err, sql.ErrNoRows):
		// 没有代次行：匿名可用，凭据认证会被门禁拒绝。
	default:
		return v2auth.PolicySnapshot{}, fmt.Errorf("读取官方凭据代次: %w", err)
	}
	return snap, nil
}

// GetRealmPolicy 读取一行策略；没有行时返回 ErrPolicyMissing 语义的错误。
func (s *Store) GetRealmPolicy(sourceID int64, rawHost string) (*RealmPolicy, error) {
	host, err := normalizePolicyHost(rawHost)
	if err != nil {
		return nil, err
	}
	var primary, extraJSON string
	var gen, updated int64
	err = s.db.QueryRow(`SELECT primary_realm, extra_realms, policy_gen, updated_at
		FROM source_realm_policy WHERE source_id=? AND host=?`, sourceID, host).
		Scan(&primary, &extraJSON, &gen, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, v2auth.ErrPolicyMissing
	}
	if err != nil {
		return nil, err
	}
	primary, extras, err := validatePolicyRealms(primary, extraJSON)
	if err != nil {
		return nil, err
	}
	return &RealmPolicy{SourceID: sourceID, Host: host, PrimaryRealm: primary, ExtraRealms: extras, PolicyGen: gen, UpdatedAt: updated}, nil
}

// SetRealmPolicy 显式替换一个 (source_id, host) 的策略并把 policy_gen 加一。
// 策略行和事件在同一事务里；主键冲突也走更新，不会覆盖另一行。
func (s *Store) SetRealmPolicy(sourceID int64, rawHost, primary string, extra []string) error {
	host, err := normalizePolicyHost(rawHost)
	if err != nil {
		return err
	}
	primary, extra, err = normalizePolicyRealms(primary, extra)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	var status string
	if err := tx.QueryRow(`SELECT 1, status FROM sources WHERE id=?`, sourceID).Scan(&one, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceNotFound
		}
		return err
	}
	if status == "archived" {
		return ErrArchived
	}
	if err := tx.QueryRow(`SELECT 1 FROM source_host_caps WHERE source_id=? AND host=?`, sourceID, host).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrHostCapsNotFound
		}
		return err
	}
	if err := setRealmPolicyTx(tx, sourceID, host, primary, extra, true); err != nil {
		return err
	}
	// 认证 Realm 变了，旧能力结论不再可信：清结论、清当前代 scoped 失败，
	// 让下一轮探测重新验证；诊断保留为历史事实。
	now := time.Now().Unix()
	if _, err := tx.Exec(`UPDATE source_host_caps SET enabled=1, effective_status=?, registry_path_mode=''
		WHERE source_id=? AND host=?`, HostStatusUnknown, sourceID, host); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM source_health_scope
		WHERE source_id=? AND family='registry' AND scope=?`, sourceID, host); err != nil {
		return err
	}
	if host == "docker.io" || host == "ghcr.io" {
		fam := "docker"
		if host == "ghcr.io" {
			fam = "ghcr"
		}
		if _, err := tx.Exec(`DELETE FROM source_health WHERE source_id=? AND family=?`, sourceID, fam); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE registry_host_recovery
		SET state='initial', next_probe_at=?, rec_failures=0, retry_failures=0, last_error='', updated_at=?
		WHERE source_id=? AND family='registry' AND scope=?`, now, now, sourceID, host); err != nil {
		return err
	}
	if err := recordPolicyChangeTx(tx, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// setRealmPolicyTx 只接受事务，确保业务行、策略行和事件不会被拆开提交。
// bump=false 用于 AddSource 的初始行（policy_gen=1）。
func setRealmPolicyTx(tx *sql.Tx, sourceID int64, host, primary string, extra []string, bump bool) error {
	extraJSON, _ := json.Marshal(extra)
	if bump {
		_, err := tx.Exec(`INSERT INTO source_realm_policy
(source_id, host, primary_realm, extra_realms, policy_gen, updated_at)
VALUES(?,?,?,?,1,?)
ON CONFLICT(source_id, host) DO UPDATE SET
  primary_realm=excluded.primary_realm,
  extra_realms=excluded.extra_realms,
  policy_gen=source_realm_policy.policy_gen+1,
  updated_at=excluded.updated_at`,
			sourceID, host, primary, string(extraJSON), time.Now().Unix())
		return err
	}
	_, err := tx.Exec(`INSERT INTO source_realm_policy
(source_id, host, primary_realm, extra_realms, policy_gen, updated_at)
VALUES(?,?,?,?,1,?)`, sourceID, host, primary, string(extraJSON), time.Now().Unix())
	return err
}

func recordPolicyChangeTx(tx *sql.Tx, sourceID int64) error {
	_, err := tx.Exec(`INSERT INTO source_events(source_id, kind, ts, ok, neutral, ms, code)
		VALUES(?,?,?,?,?,0,0)`, sourceID, SourceEventPolicyChange, time.Now().Unix(), 1, 1)
	return err
}

// policySeedForSource 生成首次策略。规则：
//   - 已知 repo_host 且 BaseURL Host 相同：使用官方默认 Realm；
//   - 已知 repo_host 但 BaseURL Host 不同（如 m.daocloud.io -> public.ecr.aws，
//     或 ghcr.dockerproxy.com -> ghcr.io）：BaseURL origin 为主，官方 Realm
//     作为额外白名单；镜像自身的 origin 必须能用，官方 Realm 是已知安全来源；
//   - 未知 Host 在迁移时返回 ok=false，进入报告并 fail closed；新源 allowUnknown
//     时使用 BaseURL origin，便于手工新增自定义源。
func SeedPolicyForSource(baseURL, repoHost string, allowUnknown bool) (primary string, extra []string, ok bool) {
	return policySeedForSource(baseURL, repoHost, allowUnknown)
}

func policySeedForSource(baseURL, repoHost string, allowUnknown bool) (primary string, extra []string, ok bool) {
	repoHost, err := registryhost.Normalize(repoHost)
	if err != nil || repoHost == "" {
		return "", nil, false
	}
	baseHost := hostFromBaseURL(baseURL)
	baseNormalized, _ := registryhost.Normalize(baseHost)

	if p, e, found := knownPolicySeed(repoHost); found {
		if baseNormalized != "" && baseNormalized != repoHost {
			primary = "https://" + baseHost
			// 已知官方 Realm 不能因为 BaseURL 换了镜像域名就丢掉：
			// 例如 ghcr.dockerproxy.com 的 token 端点仍是 ghcr.io。
			extra = appendUniqueRealms(extra, p)
			extra = appendUniqueRealms(extra, e...)
			extra = removeRealm(extra, primary)
			if len(extra) > maxPolicyExtraRealms {
				extra = extra[:maxPolicyExtraRealms]
			}
			return primary, extra, true
		}
		if baseHost != "" && baseNormalized == "" {
			return "https://" + baseHost, nil, true
		}
		return p, e, true
	}
	if !allowUnknown {
		return "", nil, false
	}
	if baseHost != "" {
		return "https://" + baseHost, nil, true
	}
	return "https://" + repoHost, nil, true
}

func appendUniqueRealms(dst []string, src ...string) []string {
	seen := make(map[string]bool, len(dst)+len(src))
	for _, x := range dst {
		seen[x] = true
	}
	for _, x := range src {
		if x != "" && !seen[x] {
			dst = append(dst, x)
			seen[x] = true
		}
	}
	return dst
}

func removeRealm(list []string, drop string) []string {
	out := list[:0]
	for _, x := range list {
		if x != drop {
			out = append(out, x)
		}
	}
	return out
}

func knownPolicySeed(repoHost string) (string, []string, bool) {
	realms := v2auth.DefaultRealmsFor(repoHost)
	if len(realms) == 0 {
		if registryhost.IsArtifactRegistry(repoHost) {
			return "https://" + repoHost, nil, true
		}
		return "", nil, false
	}
	primary := realms[0]
	var extra []string
	if len(realms) > 1 {
		extra = append(extra, realms[1:]...)
	}
	return primary, extra, true
}

func hostFromBaseURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	h, err := registryhost.Normalize(u.Hostname())
	if err != nil {
		return ""
	}
	return h
}

func normalizePolicyHost(raw string) (string, error) {
	h, err := registryhost.Normalize(raw)
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", errors.New("策略 Host 不能为空")
	}
	return h, nil
}

func normalizePolicyRealms(primary string, extra []string) (string, []string, error) {
	primary, err := normalizeRealm(primary)
	if err != nil {
		return "", nil, err
	}
	if len(extra) > maxPolicyExtraRealms {
		return "", nil, fmt.Errorf("extra_realms 最多 %d 个，实际 %d", maxPolicyExtraRealms, len(extra))
	}
	seen := map[string]bool{primary: true}
	out := make([]string, 0, len(extra))
	for _, raw := range extra {
		r, err := normalizeRealm(raw)
		if err != nil {
			return "", nil, err
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return primary, out, nil
}

func validatePolicyRealms(primary, extraJSON string) (string, []string, error) {
	var extra []string
	if err := json.Unmarshal([]byte(extraJSON), &extra); err != nil {
		return "", nil, fmt.Errorf("extra_realms 不是 JSON 数组: %w", err)
	}
	return normalizePolicyRealms(primary, extra)
}

func normalizeRealm(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("Realm 无法解析: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("Realm 必须是 https origin，不能带 userinfo/query/fragment: %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("Realm 不能带 path: %q", raw)
	}
	host, err := registryhost.Normalize(u.Hostname())
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", fmt.Errorf("Realm 缺 Host: %q", raw)
	}
	return "https://" + host, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// PolicyListItem 是某个 Host 下的一条 Source 及其策略（策略可能缺失）。
type PolicyListItem struct {
	SourceID     int64        `json:"source_id"`
	Name         string       `json:"name"`
	BaseURL      string       `json:"base_url"`
	RepoHost     string       `json:"repo_host"`
	Enabled      bool         `json:"enabled"`
	Status       string       `json:"status"`
	RegistryCap  int          `json:"registry_cap"`
	RegistryPath string       `json:"registry_path_mode"`
	Policy       *RealmPolicy `json:"policy"`
}

// ListRealmPolicies 返回某个 Host 下所有未归档 Source 及策略。
// host 为空返回空列表，调用方应先取 Host 汇总。
func (s *Store) ListRealmPolicies(rawHost string) ([]PolicyListItem, error) {
	host, err := normalizePolicyHost(rawHost)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT s.id, s.name, s.base_url, c.host, s.enabled, s.status,
		c.effective_status, c.registry_path_mode,
		p.primary_realm, p.extra_realms, p.policy_gen, p.updated_at
		FROM source_host_caps c
		JOIN sources s ON s.id=c.source_id
		LEFT JOIN source_realm_policy p ON p.source_id=s.id AND p.host=c.host
		WHERE c.host=? AND s.status<>'archived'
		ORDER BY s.id`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PolicyListItem
	for rows.Next() {
		var it PolicyListItem
		var primary, extra sql.NullString
		var gen, updated sql.NullInt64
		var status string
		if err := rows.Scan(&it.SourceID, &it.Name, &it.BaseURL, &it.RepoHost, &it.Enabled, &it.Status,
			&status, &it.RegistryPath, &primary, &extra, &gen, &updated); err != nil {
			return nil, err
		}
		it.RegistryCap = capForStatus(status)
		if primary.Valid {
			extras, err := parseExtraRealms(extra.String)
			if err != nil {
				return nil, err
			}
			it.Policy = &RealmPolicy{
				SourceID: it.SourceID, Host: it.RepoHost, PrimaryRealm: primary.String,
				ExtraRealms: extras, PolicyGen: gen.Int64, UpdatedAt: updated.Int64,
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// PolicyHostSummary 是 Host 只读汇总的一行。
type PolicyHostSummary struct {
	Host       string `json:"host"`
	Sources    int    `json:"sources"`
	WithPolicy int    `json:"with_policy"`
}

// ListPolicyHosts 汇总当前有 Source 归属的 Host。
func (s *Store) ListPolicyHosts() ([]PolicyHostSummary, error) {
	rows, err := s.db.Query(`SELECT c.host,
		COUNT(*),
		SUM(CASE WHEN p.source_id IS NULL THEN 0 ELSE 1 END)
		FROM source_host_caps c
		JOIN sources s ON s.id=c.source_id
		LEFT JOIN source_realm_policy p ON p.source_id=c.source_id AND p.host=c.host
		WHERE s.status<>'archived'
		GROUP BY c.host ORDER BY c.host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PolicyHostSummary
	for rows.Next() {
		var x PolicyHostSummary
		if err := rows.Scan(&x.Host, &x.Sources, &x.WithPolicy); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DeleteRealmPolicy 删除一行策略并记录事件。删除后运行时必须 fail closed。
func (s *Store) DeleteRealmPolicy(sourceID int64, rawHost string) error {
	host, err := normalizePolicyHost(rawHost)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM source_realm_policy WHERE source_id=? AND host=?`, sourceID, host)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return v2auth.ErrPolicyMissing
	}
	if err := recordPolicyChangeTx(tx, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// PolicyEvent 是策略变更事件（source_events 的 policy_change 行）。
type PolicyEvent struct {
	ID       int64 `json:"id"`
	SourceID int64 `json:"source_id"`
	TS       int64 `json:"ts"`
	OK       bool  `json:"ok"`
}

// ListPolicyEvents 返回策略变更事件，sourceID=0 表示全部。
func (s *Store) ListPolicyEvents(sourceID int64, limit int) ([]PolicyEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, source_id, ts, ok FROM source_events WHERE kind=?`
	args := []any{SourceEventPolicyChange}
	if sourceID > 0 {
		q += ` AND source_id=?`
		args = append(args, sourceID)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PolicyEvent
	for rows.Next() {
		var e PolicyEvent
		if err := rows.Scan(&e.ID, &e.SourceID, &e.TS, &e.OK); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func parseExtraRealms(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}
