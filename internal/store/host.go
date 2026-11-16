package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// Registry Host 能力判定，与 docker_cap/ghcr_cap 的 -1/0/1 语义对齐，
// 另加 2 = 「能拉到但内容与官方不一致」：这种源必须排除，但不能显示成
// 「不支持」，否则用户会去查路径而不是查内容安全。
const (
	RegistryCapUnknown  = -1 // 未探测 / 认证不足 / 限流 / 超时：不排除，回退官方直连
	RegistryCapNo       = 0  // 明确证据表明该路径格式不可用
	RegistryCapYes      = 1  // 路径 + 认证 + Manifest 内容一致性均已验证
	RegistryCapMismatch = 2  // Manifest 内容与官方不一致，硬排除
)

// Registry 身份状态：显式区分「已确认归属」「类型不适用」「尚未确认」。
// 空 Host 不再隐含 Docker Hub；unclassified 不得被写入猜测 Host。
const (
	IdentityIdentified    = "identified"
	IdentityUnclassified  = "unclassified"
	IdentityNotApplicable = "not_applicable"
)

// 路径格式。native = /v2/<repo>/...；host_prefix = /v2/<host>/<repo>/...。
const (
	PathNative     = "native"
	PathHostPrefix = "host_prefix"
)

// ErrSourceNotFound 表示按 id 找不到源。
var ErrSourceNotFound = errors.New("source not found")

// HostCapsCommitResult 说明一次能力写回为什么没成功。
//
// 不能用 bool：写回失败可能是凭据版本变了，也可能是源被归档、被移走或
// 被删了。前者要重排，后者重排只会变成死循环。
type HostCapsCommitResult int

const (
	// HostCapsCommitted：已写入。
	HostCapsCommitted HostCapsCommitResult = iota
	// HostCapsStaleSnapshot：探测期间的 gen 或 cred_digest 已过期。
	HostCapsStaleSnapshot
	// HostCapsSourceChanged：repo_host 已经改成别的 Host。
	HostCapsSourceChanged
	// HostCapsSourceUnavailable：源不存在或已归档。
	HostCapsSourceUnavailable
)

func (r HostCapsCommitResult) String() string {
	switch r {
	case HostCapsCommitted:
		return "committed"
	case HostCapsStaleSnapshot:
		return "stale_snapshot"
	case HostCapsSourceChanged:
		return "source_changed"
	case HostCapsSourceUnavailable:
		return "source_unavailable"
	default:
		return "unknown"
	}
}

// SetRepoHost 修改源归属的 Registry Host。
// 归属变了，旧 Host 的探测结论、健康证据和恢复状态对新 Host 都没有意义，
// 必须在同一事务里一起清掉，否则新 Host 会顶着旧归属的数据被调度。
func (s *Store) SetRepoHost(id int64, rawHost string) error {
	host, err := registryhost.Check(rawHost)
	if err != nil {
		return err
	}
	return s.withSourceLive(id, func(tx *sql.Tx) error {
		var baseURL string
		if err := tx.QueryRow(`SELECT base_url FROM sources WHERE id=?`, id).Scan(&baseURL); err != nil {
			return err
		}
		if host == "" {
			if _, err := tx.Exec(`DELETE FROM source_host_caps WHERE source_id=? AND host NOT IN ('docker.io','ghcr.io')`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM source_realm_policy WHERE source_id=? AND host NOT IN ('docker.io','ghcr.io')`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM source_health_scope WHERE source_id=? AND family='registry' AND scope NOT IN ('docker.io','ghcr.io')`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM registry_host_recovery WHERE source_id=? AND family='registry' AND scope NOT IN ('docker.io','ghcr.io')`, id); err != nil {
				return err
			}
			return recordPolicyChangeTx(tx, id)
		}
		if _, err := tx.Exec(`DELETE FROM source_host_caps WHERE source_id=? AND host NOT IN ('docker.io','ghcr.io') AND host<>?`, id, host); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM source_realm_policy WHERE source_id=? AND host NOT IN ('docker.io','ghcr.io') AND host<>?`, id, host); err != nil {
			return err
		}
		// 归属切换后，新 Host 也不能继承旧的 scoped 失败/恢复状态；
		// docker/ghcr 没有 Host 探测，保留它们的行。
		if _, err := tx.Exec(`DELETE FROM source_health_scope WHERE source_id=? AND family='registry' AND scope NOT IN ('docker.io','ghcr.io')`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM registry_host_recovery WHERE source_id=? AND family='registry' AND scope NOT IN ('docker.io','ghcr.io')`, id); err != nil {
			return err
		}
		if host == "docker.io" || host == "ghcr.io" {
			fam := "docker"
			if host == "ghcr.io" {
				fam = "ghcr"
			}
			if _, err := tx.Exec(`DELETE FROM source_health WHERE source_id=? AND family=?`, id, fam); err != nil {
				return err
			}
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM source_host_caps WHERE source_id=? AND host=?`, id, host).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				if err := upsertHostCapsTx(tx, id, host, HostVerdictInconclusive, HostStatusUnknown, "", 0, 1); err != nil {
					return err
				}
			}
		} else if err := upsertHostCapsTx(tx, id, host, HostVerdictInconclusive, HostStatusUnknown, "", 0, 1); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE source_host_caps SET enabled=1 WHERE source_id=? AND host=?`, id, host); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE source_realm_policy
SET policy_gen=policy_gen+1, updated_at=? WHERE source_id=? AND host=?`,
			time.Now().Unix(), id, host)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			if primary, extra, ok := policySeedForSource(baseURL, host, true); ok {
				if err := setRealmPolicyTx(tx, id, host, primary, extra, true); err != nil {
					return err
				}
			}
		}
		var policyN int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM source_realm_policy WHERE source_id=? AND host=?`, id, host).Scan(&policyN); err != nil {
			return err
		}
		if policyN == 0 {
			return errors.New("新 Host 没有可用 Realm 策略，拒绝留下不可调度的半成品")
		}
		if err := recordPolicyChangeTx(tx, id); err != nil {
			return err
		}
		if host == "docker.io" || host == "ghcr.io" {
			return nil
		}
		var gen int64
		err = tx.QueryRow(`SELECT gen FROM registry_host_gen WHERE host=?`, host).Scan(&gen)
		if errors.Is(err, sql.ErrNoRows) {
			gen = 0
		} else if err != nil {
			return err
		}
		now := time.Now().Unix()
		_, err = tx.Exec(`INSERT INTO registry_host_recovery
(source_id, family, scope, state, gen, last_probe_at, last_success_at, next_probe_at, rec_failures, retry_failures, last_error, updated_at)
VALUES(?, 'registry', ?, 'initial', ?, 0, 0, ?, 0, 0, '', ?)
ON CONFLICT(source_id, family, scope) DO UPDATE SET
  state='initial', gen=excluded.gen,
  next_probe_at=excluded.next_probe_at, rec_failures=0, retry_failures=0,
  last_error='', updated_at=excluded.updated_at`,
			id, host, gen, now, now)
		return err
	})
}

// withSourceLive 在事务里确认源存在且未归档，再执行修改。
// 归档是用户明确决定，任何后台写入都不能覆盖它。
func (s *Store) withSourceLive(id int64, fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRow(`SELECT status FROM sources WHERE id=?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSourceNotFound
	}
	if err != nil {
		return err
	}
	if status == "archived" {
		return ErrArchived
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SetHostCaps 写回一次 Host Probe 的结果。
//
// Deprecated: 生产路径已统一到 CommitHostProbe（cap + scoped + recovery 同一
// 事务）。此函数只保留给历史测试造数据，不得新增调用方；
// TestRegistryProbeWritePathsConverged 会拦住生产代码里的调用。
//
// 三个条件都放在 SQL 里，不靠 Go 侧提前检查：
//   - source_host_caps 行必须仍是探测时的 Host（用户可能中途改了归属）；
//   - registry_host_gen 必须仍是探测开始时的代次（凭据可能中途换过）；
//   - status 不能是 archived（用户可能中途归档）。
//
// 条件不满足时返回 HostCapsStaleSnapshot / HostCapsSourceChanged /
// HostCapsSourceUnavailable，调用方据此决定重排还是收尾。
func (s *Store) SetHostCaps(id int64, rawHost string, cap int, pathMode string, at int64, errText string, gen int64, credDigest string) (HostCapsCommitResult, error) {
	host, err := registryhost.Check(rawHost)
	if err != nil {
		return HostCapsCommitted, err
	}
	if host == "" || host == "ghcr.io" || host == "docker.io" {
		return HostCapsCommitted, errors.New("Hub/ghcr 的能力由 source_host_caps 的 docker.io/ghcr.io 行记录，不走 SetHostCaps")
	}
	if !validRegistryCap(cap) {
		return HostCapsCommitted, errors.New("registry_cap 只允许 -1/0/1/2")
	}
	if cap == RegistryCapYes {
		if pathMode != PathNative && pathMode != PathHostPrefix {
			return HostCapsCommitted, errors.New("registry_cap=1 必须指定 native/host_prefix")
		}
	} else if pathMode != "" {
		return HostCapsCommitted, errors.New("只有 registry_cap=1 可以带 registry_path_mode")
	}
	last, effective := verdictForCap(cap)
	res, err := s.db.Exec(`UPDATE source_host_caps
SET last_verdict=?, effective_status=?, registry_path_mode=?
WHERE source_id=? AND host=?
  AND EXISTS (SELECT 1 FROM sources s WHERE s.id=source_host_caps.source_id AND s.status<>'archived')
  AND COALESCE((SELECT gen FROM registry_host_gen WHERE host=?),0)=?
  AND COALESCE((SELECT cred_digest FROM registry_host_gen WHERE host=?),'')=?`,
		last, effective, pathMode, id, host, host, gen, host, credDigest)
	if err != nil {
		return HostCapsCommitted, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return HostCapsCommitted, err
	}
	if n > 0 {
		return HostCapsCommitted, nil
	}
	return s.classifyHostCapsMiss(id, host, gen, credDigest)
}

// classifyHostCapsMiss 在 UPDATE 未命中后读一次现场，判断是版本过期还是
// 源本身变了。这次读取只影响「接下来怎么调度」，不参与写回，所以和
// UPDATE 之间的竞态最多导致一次多余重排，不会写错数据。
func (s *Store) classifyHostCapsMiss(id int64, host string, gen int64, credDigest string) (HostCapsCommitResult, error) {
	var status string
	err := s.db.QueryRow(`SELECT status FROM sources WHERE id=?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return HostCapsSourceUnavailable, nil
	}
	if err != nil {
		return HostCapsCommitted, err
	}
	if status == "archived" {
		return HostCapsSourceUnavailable, nil
	}
	var one int
	if err := s.db.QueryRow(`SELECT 1 FROM source_host_caps WHERE source_id=? AND host=?`, id, host).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HostCapsSourceChanged, nil
		}
		return HostCapsCommitted, err
	}
	var g int64
	var d string
	err = s.db.QueryRow(`SELECT gen, cred_digest FROM registry_host_gen WHERE host=?`, host).Scan(&g, &d)
	if errors.Is(err, sql.ErrNoRows) {
		g, d = 0, ""
	} else if err != nil {
		return HostCapsCommitted, err
	}
	if g != gen || d != credDigest {
		return HostCapsStaleSnapshot, nil
	}
	// 条件都还成立却没更新到：期间被别的写入改回原值（例如归档后又恢复）。
	// 当成过期处理，重探一次即可，不会无限循环——源不可用会走上面的分支。
	return HostCapsStaleSnapshot, nil
}

func validRegistryCap(v int) bool {
	return v >= RegistryCapUnknown && v <= RegistryCapMismatch
}

// HostGen 返回某 Host 当前的凭据代次；没有记录时为 0。
func (s *Store) HostGen(rawHost string) (int64, error) {
	host, err := registryhost.Normalize(rawHost)
	if err != nil {
		return 0, err
	}
	var gen int64
	err = s.db.QueryRow(`SELECT gen FROM registry_host_gen WHERE host=?`, host).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return gen, err
}

// CredChange 是一次凭据版本提交：Digest 为 nil 表示只递增代次、保留摘要。
type CredChange struct {
	Host   string
	Digest *string
}

// CredState 是某 Host 当前的凭据版本，供对账比较。
type CredState struct {
	Host   string
	Gen    int64
	Digest string
}

// HostGenState 返回某 Host 的 (gen, cred_digest)；没有记录时是 (0, "")。
func (s *Store) HostGenState(rawHost string) (int64, string, error) {
	host, err := registryhost.Normalize(rawHost)
	if err != nil {
		return 0, "", err
	}
	var gen int64
	var digest string
	err = s.db.QueryRow(`SELECT gen, cred_digest FROM registry_host_gen WHERE host=?`, host).Scan(&gen, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	return gen, digest, nil
}

// HostCredStates 列出所有已有版本记录的 Host。
func (s *Store) HostCredStates() ([]CredState, error) {
	rows, err := s.db.Query(`SELECT host, gen, cred_digest FROM registry_host_gen ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CredState
	for rows.Next() {
		var c CredState
		if err := rows.Scan(&c.Host, &c.Gen, &c.Digest); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ApplyCredChanges 递增代次、写入摘要、清空能力结论，全部在一个事务里。
// 代次涨了但能力没清，会出现「新凭据 + 旧结论」。
func (s *Store) ApplyCredChanges(changes []CredChange) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, c := range changes {
		host, err := registryhost.Check(c.Host)
		if err != nil {
			return err
		}
		if host == "" || host == "ghcr.io" || host == "docker.io" {
			return errors.New("Hub/ghcr 不使用 Host 代次")
		}
		var newGen int64
		if c.Digest == nil {
			if err := tx.QueryRow(`INSERT INTO registry_host_gen(host,gen,updated_at) VALUES(?,1,?)
ON CONFLICT(host) DO UPDATE SET gen=gen+1, updated_at=excluded.updated_at
RETURNING gen`, host, now).Scan(&newGen); err != nil {
				return err
			}
		} else if err := tx.QueryRow(`INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES(?,1,?,?)
ON CONFLICT(host) DO UPDATE SET gen=gen+1, cred_digest=excluded.cred_digest, updated_at=excluded.updated_at
RETURNING gen`,
			host, *c.Digest, now).Scan(&newGen); err != nil {
			return err
		}
		// 只清结论，不抹诊断：诊断是上一次有效 Probe 留下的历史事实，
		// 凭据变了只意味着它不能再当结论用，不意味着它没发生过。
		if _, err := tx.Exec(`UPDATE source_host_caps
SET effective_status=?, registry_path_mode=''
WHERE host=? AND EXISTS (SELECT 1 FROM sources s WHERE s.id=source_host_caps.source_id AND s.status<>'archived')`,
			HostStatusUnknown, host); err != nil {
			return err
		}
		// 代次绑定：recovery 行在同一事务内切到新 gen 并重置为待重验。
		// last_success_at 保留为历史事实，但 gen 不匹配使它不再参与 ok 判定。
		if _, err := tx.Exec(`INSERT INTO registry_host_recovery
(source_id, family, scope, state, gen, last_probe_at, last_success_at, next_probe_at, rec_failures, retry_failures, last_error, updated_at)
SELECT c.source_id, 'registry', ?, 'initial', ?, 0, 0, ?, 0, 0, '', ?
FROM source_host_caps c
JOIN sources s ON s.id=c.source_id
WHERE c.host=? AND s.status<>'archived'
ON CONFLICT(source_id, family, scope) DO UPDATE SET
  state='initial', gen=excluded.gen,
  next_probe_at=excluded.next_probe_at, rec_failures=0, retry_failures=0,
  last_error='', updated_at=excluded.updated_at`,
			host, newGen, now, now, host); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BumpHostGen 递增代次并清空能力，保留已提交的凭据摘要。
func (s *Store) BumpHostGen(rawHost string) (int64, error) {
	return s.bump(rawHost, nil)
}

// BumpHostGenDigest 递增代次并把该 Host 的凭据摘要置为新值。
func (s *Store) BumpHostGenDigest(rawHost, digest string) (int64, error) {
	return s.bump(rawHost, &digest)
}

func (s *Store) bump(rawHost string, digest *string) (int64, error) {
	host, err := registryhost.Check(rawHost)
	if err != nil {
		return 0, err
	}
	if err := s.ApplyCredChanges([]CredChange{{Host: host, Digest: digest}}); err != nil {
		return 0, err
	}
	gen, _, err := s.HostGenState(host)
	return gen, err
}

// HostCap 是面板与调度读取的一行 Host 能力记录。
type HostCap struct {
	SourceID   int64  `json:"source_id"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	RepoHost   string `json:"repo_host"`
	Cap        int    `json:"registry_cap"`
	PathMode   string `json:"registry_path_mode"`
	ProbeAt    int64  `json:"registry_probe_at"`
	ProbeError string `json:"registry_probe_error"`
	Gen        int64  `json:"gen"`
}

// ListHostCaps 返回所有 Foreign Host 的源（含已探测的），供面板展示。
// Docker/GHCR 的能力在 source_host_caps 的 docker.io/ghcr.io 行，独立候选池
// 不混进 Foreign 列表。
func (s *Store) ListHostCaps() ([]HostCap, error) {
	return s.hostCaps("")
}

// ResetHostCaps 把一条源的能力结论清回「未知」，但保留最近一次诊断与
// 探测时间：诊断确实来自那次有效 Probe，只是它对应的代表性仓库已经不
// 是当前配置，不能再当结论用。
//
// 清理带条件：对账读到的是 (id, host, cap)，写回时必须仍然一致，
// 否则说明期间有人改过归属或能力，不能拿旧判断误伤新状态。
//
// 刻意不动 registry_host_gen：这是「探测输入变了」，不是凭据变更，也
// 不应该让在途结果被误判成 stale_snapshot。
func (s *Store) ResetHostCaps(id int64, host string, cap int) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE source_host_caps
SET effective_status=?, registry_path_mode=''
WHERE source_id=? AND host=? AND effective_status=?`,
		HostStatusUnknown, id, host, statusForCap(cap))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	// 代表性仓库变了，必须立刻重探；只清 cap 会让 recovery 的旧 next_probe_at
	// 继续把任务压到未来，期间该源一直不可调度。policy_gen 同时充当探测
	// epoch，重置前启动的旧探测结果会因 policy_gen 不匹配而 stale。
	now := time.Now().Unix()
	if _, err := tx.Exec(`UPDATE source_realm_policy
SET policy_gen=policy_gen+1, updated_at=? WHERE source_id=? AND host=?`, now, id, host); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE registry_host_recovery
SET state='initial', next_probe_at=?, rec_failures=0, retry_failures=0, last_error='', updated_at=?
WHERE source_id=? AND family='registry' AND scope=?`, now, now, id, host); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM source_health_scope
WHERE source_id=? AND family='registry' AND scope=?`, id, host); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
func statusForCap(cap int) string {
	switch cap {
	case RegistryCapYes:
		return HostStatusSupported
	case RegistryCapNo:
		return HostStatusUnsupported
	case RegistryCapMismatch:
		return HostStatusMismatch
	default:
		return HostStatusUnknown
	}
}

// DueHostProbe 是一条到期待探测的源×Host。
type DueHostProbe struct {
	SourceID  int64
	Name      string
	BaseURL   string
	RepoHost  string
	Gen       int64
	PolicyGen int64
}

// ListDueHostProbes 返回 §6 定义的待探测集合：recovery 行不存在（还没人
// 探测过）或 next_probe_at 已到期。持久化状态是唯一权威，Runner 重启后
// 直接从这里捞回任务。
//
// 边界与 §6.1 一致：
//   - archived / 空 Host / ghcr.io 不参与；
//   - disabled 源保留 recovery 行但不进周期探测；
//   - cap=2 与 state='hard-excluded' 双重排除（cap 未写回时靠 state，
//     state 未更新时靠 cap）；
//   - cap=1 但 recovery 行缺失仍会返回——缺行不等于健康，必须重探。
func (s *Store) ListDueHostProbes(now int64) ([]DueHostProbe, error) {
	rows, err := s.db.Query(`
SELECT s.id, s.name, s.base_url, c.host, COALESCE(g.gen,0), COALESCE(p.policy_gen,0)
FROM sources s
JOIN source_host_caps c ON c.source_id=s.id
LEFT JOIN registry_host_gen g ON g.host = c.host
LEFT JOIN source_realm_policy p ON p.source_id=s.id AND p.host=c.host
WHERE s.status<>'archived'
  AND c.host<>'ghcr.io'
  AND c.host<>'docker.io'
  AND s.enabled = 1
  AND c.enabled = 1
  AND c.effective_status <> 'Mismatch'
  AND NOT EXISTS (SELECT 1 FROM registry_host_recovery r
                  WHERE r.source_id=s.id AND r.family='registry'
                    AND r.scope=c.host AND r.state='hard-excluded')
  AND ( NOT EXISTS (SELECT 1 FROM registry_host_recovery r
                    WHERE r.source_id=s.id AND r.family='registry' AND r.scope=c.host)
     OR EXISTS (SELECT 1 FROM registry_host_recovery r
                WHERE r.source_id=s.id AND r.family='registry'
                  AND r.scope=c.host AND r.next_probe_at <= ?) )
ORDER BY c.host, s.name`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DueHostProbe
	for rows.Next() {
		var x DueHostProbe
		if err := rows.Scan(&x.SourceID, &x.Name, &x.BaseURL, &x.RepoHost, &x.Gen, &x.PolicyGen); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListPendingHostCaps 只返回尚未验证成功的源。
//
// Deprecated: Runner 已改用 ListDueHostProbes（recovery 驱动的到期查询）。
// 保留此方法仅供历史测试与只读诊断，不得作为调度依据。
func (s *Store) ListPendingHostCaps() ([]HostCap, error) {
	return s.hostCaps("AND c.effective_status<>'Supported'")
}

func (s *Store) hostCaps(extra string) ([]HostCap, error) {
	q := `
SELECT s.id, s.name, s.base_url, c.host, c.effective_status, c.registry_path_mode,
       COALESCE(r.last_probe_at,0), COALESCE(c.diagnostics,''), COALESCE(g.gen,0)
FROM source_host_caps c
JOIN sources s ON s.id=c.source_id
LEFT JOIN registry_host_recovery r
  ON r.source_id=c.source_id AND r.family='registry' AND r.scope=c.host
LEFT JOIN registry_host_gen g ON g.host = c.host
WHERE s.status<>'archived'
  AND c.host<>'ghcr.io'
  AND c.host<>'docker.io' ` + extra + `
ORDER BY c.host, s.name`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HostCap
	for rows.Next() {
		var x HostCap
		var status string
		if err := rows.Scan(&x.SourceID, &x.Name, &x.BaseURL, &x.RepoHost, &status,
			&x.PathMode, &x.ProbeAt, &x.ProbeError, &x.Gen); err != nil {
			return nil, err
		}
		x.Cap = capForStatus(status)
		out = append(out, x)
	}
	return out, rows.Err()
}

// registry_probe_error 的长度上限。
//
// 普通错误文本按 200 rune 截断；结构化诊断（version>=1 的 JSON）按 UTF-8
// 字节限制 4 KiB，且必须在序列化前裁字段——直接裁序列化结果会留下半个
// JSON，面板永远解析不出来。
const (
	probeNoteMaxRunes = 200
	probeNoteMaxBytes = 4096
)

func clipProbeNote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if isDiagnosticsJSON(s) {
		if len(s) <= probeNoteMaxBytes {
			return s
		}
		// Probe 层已按 4 KiB 裁剪；这里是兜底：宁可退化成一行文本，
		// 也不把残缺 JSON 写进库。
		return "诊断超限（" + strconv.Itoa(len(s)) + " 字节），已省略"
	}
	return clipRunes(s, probeNoteMaxRunes)
}

// isDiagnosticsJSON 先去掉首尾空白再解析，避免前缀/换行让判断失效。
func isDiagnosticsJSON(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return false
	}
	var v struct {
		Version int `json:"version"`
	}
	return json.Unmarshal([]byte(s), &v) == nil && v.Version >= 1
}

func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
