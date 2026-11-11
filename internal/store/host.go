package store

import (
	"database/sql"
	"errors"
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

// 路径格式。native = /v2/<repo>/...；host_prefix = /v2/<host>/<repo>/...。
const (
	PathNative     = "native"
	PathHostPrefix = "host_prefix"
)

// ErrSourceNotFound 表示按 id 找不到源。
var ErrSourceNotFound = errors.New("source not found")

// backfillRepoHost 把历史上只打了 ghcr_cap 的源标成 ghcr.io 归属。
//
// 只回填「纯 ghcr」源：docker_cap=1 的源同时服务 Docker Hub（实测 8 条 ghcr
// 源里有 6 条 docker_cap 也是 1），把它们改成 ghcr.io 会让 Hub 候选池少 6 条，
// 违反「3.4.6 行为不变」。它们的 ghcr 能力继续由 ghcr_cap/ghcr_prefix 权威。
//
// 幂等：第二次执行时 repo_host 已经是 ghcr.io，不再命中。
// 不触碰 docker_cap / ghcr_cap / ghcr_prefix。
func (s *Store) backfillRepoHost() error {
	_, err := s.db.Exec(`
UPDATE sources
SET repo_host='ghcr.io',
    registry_cap=1,
    registry_path_mode=CASE WHEN ghcr_prefix=1 THEN 'host_prefix' ELSE 'native' END
WHERE repo_host=''
  AND ghcr_cap=1
  AND docker_cap<>1
  AND status<>'archived'`)
	return err
}

// SetRepoHost 修改源归属的 Registry Host。
// 归属变了，旧 Host 的探测结论对新 Host 没有意义，必须一起清空，
// 否则新 Host 会顶着旧能力被调度。
func (s *Store) SetRepoHost(id int64, rawHost string) error {
	host, err := registryhost.Check(rawHost)
	if err != nil {
		return err
	}
	return s.withSourceLive(id, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sources
SET repo_host=?, registry_cap=?, registry_path_mode='', registry_probe_at=0, registry_probe_error=''
WHERE id=?`, host, RegistryCapUnknown, id)
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
// 三个条件都放在 SQL 里，不靠 Go 侧提前检查：
//   - repo_host 必须仍是探测时的 Host（用户可能中途改了归属）；
//   - registry_host_gen 必须仍是探测开始时的代次（凭据可能中途换过）；
//   - status 不能是 archived（用户可能中途归档）。
//
// 返回 false 表示这次结果已过期，调用方应静默丢弃。
func (s *Store) SetHostCaps(id int64, rawHost string, cap int, pathMode string, at int64, errText string, gen int64, credDigest string) (bool, error) {
	host, err := registryhost.Check(rawHost)
	if err != nil {
		return false, err
	}
	if host == "" || host == "ghcr.io" {
		return false, errors.New("Hub/ghcr 的能力仍由 docker_cap/ghcr_cap 记录，不走 SetHostCaps")
	}
	if !validRegistryCap(cap) {
		return false, errors.New("registry_cap 只允许 -1/0/1/2")
	}
	if pathMode != "" && pathMode != PathNative && pathMode != PathHostPrefix {
		return false, errors.New("registry_path_mode 只允许 native/host_prefix")
	}
	res, err := s.db.Exec(`UPDATE sources
SET registry_cap=?, registry_path_mode=?, registry_probe_at=?, registry_probe_error=?
WHERE id=?
  AND repo_host=?
  AND status<>'archived'
  AND COALESCE((SELECT gen FROM registry_host_gen WHERE host=?),0)=?
  AND COALESCE((SELECT cred_digest FROM registry_host_gen WHERE host=?),'')=?`,
		cap, pathMode, at, clipErr(errText), id, host, host, gen, host, credDigest)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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
		if host == "" || host == "ghcr.io" {
			return errors.New("Hub/ghcr 不使用 Host 代次")
		}
		if c.Digest == nil {
			if _, err := tx.Exec(`INSERT INTO registry_host_gen(host,gen,updated_at) VALUES(?,1,?)
ON CONFLICT(host) DO UPDATE SET gen=gen+1, updated_at=excluded.updated_at`, host, now); err != nil {
				return err
			}
		} else if _, err := tx.Exec(`INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES(?,1,?,?)
ON CONFLICT(host) DO UPDATE SET gen=gen+1, cred_digest=excluded.cred_digest, updated_at=excluded.updated_at`,
			host, *c.Digest, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE sources
SET registry_cap=?, registry_path_mode='', registry_probe_at=0, registry_probe_error=''
WHERE repo_host=? AND status<>'archived'`, RegistryCapUnknown, host); err != nil {
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

// ListHostCaps 返回所有「新 Host」的源（含已探测的），供面板展示。
// Docker Hub（空串）与 ghcr.io 的能力以旧列为权威，混进来就会在面板上
// 出现两套可能矛盾的状态，所以一律排除。
func (s *Store) ListHostCaps() ([]HostCap, error) {
	return s.hostCaps("")
}

// ListPendingHostCaps 只返回尚未验证成功的源，供 Runner 入队。
// 已确认不支持（0）的也在其中：上游策略会变，仍需按周期重探。
func (s *Store) ListPendingHostCaps() ([]HostCap, error) {
	return s.hostCaps("AND s.registry_cap<>1")
}

func (s *Store) hostCaps(extra string) ([]HostCap, error) {
	q := `
SELECT s.id, s.name, s.base_url, s.repo_host, s.registry_cap, s.registry_path_mode,
       s.registry_probe_at, s.registry_probe_error, COALESCE(g.gen,0)
FROM sources s
LEFT JOIN registry_host_gen g ON g.host = s.repo_host
WHERE s.status<>'archived'
  AND s.repo_host<>''
  AND s.repo_host<>'ghcr.io' ` + extra + `
ORDER BY s.repo_host, s.name`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HostCap
	for rows.Next() {
		var x HostCap
		if err := rows.Scan(&x.SourceID, &x.Name, &x.BaseURL, &x.RepoHost, &x.Cap,
			&x.PathMode, &x.ProbeAt, &x.ProbeError, &x.Gen); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// clipErr 限制错误文本长度并按 rune 截断：上游可以把整页 HTML 塞进
// 错误信息，直接落库会让面板和备份一起膨胀。
func clipErr(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}
