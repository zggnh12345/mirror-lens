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
	SourceID     int64
	Host         string
	PrimaryRealm string
	ExtraRealms  []string
	PolicyGen    int64
	UpdatedAt    int64
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
		return v2auth.OfficialPolicy{}.Resolve(ctx, 0, host)
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
	if err := tx.QueryRow(`SELECT 1 FROM sources WHERE id=?`, sourceID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceNotFound
		}
		return err
	}
	if err := setRealmPolicyTx(tx, sourceID, host, primary, extra, true); err != nil {
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
