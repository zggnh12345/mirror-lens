// Package store persists sources, probes, settings and sessions.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
	"github.com/zggnh12345/mirror-lens/internal/store/schema"
)

// 能力标记：-1 还没测过，0 不支持，1 支持
const (
	CapUnknown = -1
	CapNo      = 0
	CapYes     = 1
)

type Source struct {
	ID      int64  `json:"id"`
	BaseURL string `json:"base_url"`
	Name    string `json:"name"`
	// SiteName 站点展示名（品牌名），来源链见 discover.SiteNameFor：
	// 状态页 data-name → 状态 JSON name → 内置种子手写映射 → 未知。
	// 与 Name 分开：Name 是用户可改的备注名，SiteName 只由来源链写入。
	SiteName string `json:"site_name"`
	Origin   string `json:"origin"` // seed | discovered | manual
	Enabled  bool   `json:"enabled"`
	Status   string `json:"status"` // pending | usable | degraded | dead | blocked | archived
	Note     string `json:"note"`
	// StatusAt 进入当前状态的时间，StatusReason 原因。
	// 只在状态变化时更新，归档/清理靠它判断「坏了多久」。
	StatusAt     int64  `json:"status_at"`
	StatusReason string `json:"status_reason"`
	// ProbeAt 最近一次探测尝试的时间；死源/封禁源靠它做低频恢复探测。
	ProbeAt int64 `json:"probe_at"`

	// 信任标记：勾上后私有请求（带 PAT）会发给这个源，多站竞速。
	Trusted bool `json:"trusted"`

	DockerCap int `json:"docker_cap"`
	GithubCap int `json:"github_cap"`
	// GhcrCap 单独记：ghcr.io 的镜像源（ghcr.nju.edu.cn 等）只代 ghcr，
	// 不做 docker hub 也不做 github 前缀代理。
	GhcrCap int `json:"ghcr_cap"`
	// GhcrPrefix 标记该源代 ghcr 时的路径格式：true = 带 ghcr.io 前缀
	// （mirror-lens 同款 /v2/ghcr.io/<repo>，如 gp.zzgg.eu.org），
	// false = 裸格式 /v2/<repo>（ghcr.nju.edu.cn 等官方换域名站）。
	// 探测时两种都试，据此写回；调度回源时按它拼路径。
	GhcrPrefix bool `json:"ghcr_prefix"`

	// RepoHost 是该源归属的 Registry Host（"": 无 Registry 归属）。
	// Docker Hub 必须显式落 "docker.io"；3.5.0 起一个源只服务一个 Registry。
	RepoHost string `json:"repo_host"`
	// RegistryCap 是该源对 RepoHost 的能力三态（0..2 见 store 常量），
	// 与 docker_cap/ghcr_cap 并行存在：Hub 与 ghcr 仍以旧列权威，
	// 新 Host 只认这两列。
	RegistryCap  int    `json:"registry_cap"`
	RegistryPath string `json:"registry_path_mode"` // native | host_prefix
	// RegistryProbeAt/RegistryProbeError 只由独立 Host Probe 写入，
	// 不得混进 probe_at（后者是旧协议族探测的字段）。
	RegistryProbeAt    int64  `json:"registry_probe_at"`
	RegistryProbeError string `json:"registry_probe_error"`
	// RegistryIdentity 是派生值，不落库：repo_host 非空即 identified；
	// 空 Host + GitHub 能力为 not_applicable；其余空 Host 为 unclassified。
	// 保留 JSON 字段是为了 API/UI 兼容，但唯一持久化事实是 repo_host。
	RegistryIdentity string `json:"registry_identity_state"`
	// GitCap 单独记：前缀代理能代 raw/release，不代表支持 git smart HTTP。
	// 默认 0（不确定），实测通过后置 1。
	GitCap int `json:"git_cap"`

	// Preferred 用户手动指定的优先源，调度时排在同等可用性的源前面。
	Preferred bool `json:"preferred"`

	// GroupID 所属分组（0=未分组）。GroupRank 是分组的优先级，越小越先试；
	// 两者都参与调度排序，见 scheduler.Candidate。
	GroupID   int64  `json:"group_id"`
	GroupName string `json:"group_name,omitempty"`
	GroupRank int    `json:"group_rank,omitempty"`

	CreatedAt int64 `json:"created_at"`

	// 每种类型最近一次检测结果
	Last map[string]*Check `json:"last,omitempty"`
	// 每种类型 24h 可用率，-1 表示无数据
	Uptime map[string]float64 `json:"uptime,omitempty"`
	// 每种类型 24h 延迟 P95（毫秒，只算成功样本）；0 表示样本不足
	P95 map[string]int64 `json:"p95_ms,omitempty"`
	// 每种类型 24h 上游 429 占比（0~1）。分母只算 code 100–599 的真实响应，
	// DNS/连接失败/超时无响应不进分母。样本不足时给 null 而非 0。
	Rate429 map[string]*float64 `json:"rate429,omitempty"`
	// 每种类型的可信度及证据（近 7 天）
	Trust map[string]Trust `json:"trust,omitempty"`

	// Recent 最近 N 次探测摘要（时间升序，最多 RecentN 条），由 /api/sources 批量填充。
	// 单独放字段而不是让前端逐源拉 /api/series/{id}：100+ 个源会变成 100+ 个请求。
	Recent []RecentPoint `json:"recent,omitempty"`
}

// Kinds 返回该源被确认支持的加速类型。
func (s Source) Kinds() []string {
	var out []string
	if s.DockerCap == CapYes {
		out = append(out, "docker")
	}
	if s.GithubCap == CapYes {
		out = append(out, "github")
	}
	return out
}

// Supports 该类型是否已被确认可用。
func (s Source) Supports(kind string) bool {
	switch kind {
	case "docker":
		return s.DockerCap == CapYes
	case "ghcr":
		return s.GhcrCap == CapYes
	case "github":
		return s.GithubCap == CapYes
	}
	return false
}

type Check struct {
	ID        int64  `json:"id"`
	SourceID  int64  `json:"source_id"`
	Kind      string `json:"kind"`
	TS        int64  `json:"ts"`
	Tier      int    `json:"tier"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	SpeedKBps int64  `json:"speed_kbps"`
	Digest    string `json:"digest"`
	DigestOK  int    `json:"digest_ok"` // -1 未知 0 不一致 1 一致
	BlobOK    int    `json:"blob_ok"`   // layer 字节：-1 未校验 0 不符 1 一致
	TTFBMS    int64  `json:"ttfb_ms"`   // 首字节耗时；0 表示这次没测
	TLSOK     bool   `json:"tls_ok"`
	CertDays  int    `json:"cert_days"`
	HTTPCode  int    `json:"http_code"`
	Error     string `json:"error"`
}

type Store struct {
	db *sql.DB

	// guard 是路由层的 fail-closed 屏蔽集（见 guard.go）。只在进程装配点
	// 注入一次；生产路径为 nil 必须在启动时失败，不能静默不过滤。
	guard       *RecoveryGuard
	recoveryTTL time.Duration
	clock       func() time.Time

	// 审计走单条后台通道：登录爆破时每个失败都同步写库会把写锁打满，
	// 所以宁可丢几条事件，也不让审计拖慢认证。
	//
	// 启动由 Open 显式触发，不挂在「第一次写事件」上：否则进程启动后
	// 若一直没人动面板，过期清理要等到第一条事件才跑。
	auditMu      sync.Mutex
	auditCh      chan AuditEvent
	auditStop    chan struct{}
	auditDone    chan struct{}
	auditStarted bool
	auditStopped bool
}

// Ready 做一次「真能写」的轻量检查：抢写锁、空更新、回滚。
// 不用 PingContext：只读挂载、磁盘满、WAL 损坏下 Ping 照样通，写缓存才失败。
func (s *Store) Ready(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	// 回滚用 Background，避免被 ctx 取消导致锁不放。
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	_, err = conn.ExecContext(ctx, "UPDATE settings SET value = value WHERE key = 'schema_version'")
	return err
}

func Open(path string) (*Store, error) {
	// foreign_keys 必须显式打开，否则 checks 的 ON DELETE CASCADE 失效，
	// 删源后检测记录会留成孤儿。
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc/sqlite: 单写者最稳
	s := &Store{db: db, recoveryTTL: RecoveryDefaultTTL, clock: time.Now}
	// 运行时不迁移、不补列、不回填。空库按 Canonical DDL 新建；非空库必须
	// 已由离线工具升级并写入 schema_meta，否则拒绝启动。旧二进制没有
	// schema_meta 检查，因此发布流程禁止用旧二进制打开新库；回滚时恢复
	// pre-3.5.6 数据库副本和原 secret.key，而不是让旧二进制直接读新库。
	empty, err := schema.IsEmpty(db)
	if err != nil {
		return nil, err
	}
	if empty {
		if err := schema.EnsureFresh(db, "fresh"); err != nil {
			return nil, err
		}
	} else if _, err := schema.Verify(db); err != nil {
		return nil, err
	}
	// 启动时验证可写：bind mount 权限不对时，SQLite 能读能建表，
	// 却在首次写检测记录时才失败，会静默几分钟。测试表必须用完即删，
	// 不能把 _writable 留在 Canonical 库里。
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _writable(x INTEGER);
		INSERT INTO _writable(x) VALUES(1); DELETE FROM _writable;
		DROP TABLE IF EXISTS _writable;`); err != nil {
		return nil, fmt.Errorf("数据目录不可写（%v）：容器内以 uid 10001 运行，请先 chown -R 10001:10001 挂载目录，或改用 named volume", err)
	}
	s.startAuditor()
	return s, nil
}

// Close 先停审计再关库：反过来的话，队列里剩下的事件会往一个已关闭的
// *sql.DB 上写，既丢记录又刷一堆错误日志。
func (s *Store) Close() error {
	s.stopAuditor()
	return s.db.Close()
}

const srcCols = `id,base_url,name,origin,enabled,status,note,docker_cap,github_cap,ghcr_cap,ghcr_prefix,git_cap,preferred,status_at,status_reason,probe_at,created_at,site_name,group_id,trusted,repo_host,registry_cap,registry_path_mode,registry_probe_at,registry_probe_error`

const checkCols = `id,source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,blob_ok,ttfb_ms,tls_ok,cert_days,http_code,error`

func scanSource(sc interface{ Scan(...any) error }) (Source, error) {
	var x Source
	err := sc.Scan(&x.ID, &x.BaseURL, &x.Name, &x.Origin, &x.Enabled, &x.Status, &x.Note,
		&x.DockerCap, &x.GithubCap, &x.GhcrCap, &x.GhcrPrefix, &x.GitCap, &x.Preferred, &x.StatusAt, &x.StatusReason, &x.ProbeAt, &x.CreatedAt, &x.SiteName, &x.GroupID, &x.Trusted,
		&x.RepoHost, &x.RegistryCap, &x.RegistryPath, &x.RegistryProbeAt, &x.RegistryProbeError)
	x.RegistryIdentity = deriveRegistryIdentity(x)
	return x, err
}

// deriveRegistryIdentity 只从持久化事实推导身份，不读取旧的身份列。
// 空 repo_host 永远表示没有 Registry 归属；只有“纯 GitHub”源才标 not_applicable：
// Docker/GHCR 能力未知或为 yes 时不能冒充 GitHub-only。
func deriveRegistryIdentity(x Source) string {
	if x.RepoHost != "" {
		return IdentityIdentified
	}
	if x.GithubCap == CapYes && x.DockerCap == CapNo && x.GhcrCap != RegistryCapYes {
		return IdentityNotApplicable
	}
	return IdentityUnclassified
}

func (s *Store) ListSources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT ` + srcCols + ` FROM sources ORDER BY CASE status WHEN 'usable' THEN 0 WHEN 'degraded' THEN 1 WHEN 'pending' THEN 2 WHEN 'dead' THEN 3 WHEN 'blocked' THEN 4 WHEN 'archived' THEN 5 ELSE 6 END, preferred DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		x, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	since := time.Now().Add(-24 * time.Hour).Unix()
	// 每个源两种类型各查一次 LastCheck + uptime 就是 372 条 SQL（93 源 × 2 × 2），
	// 单连接串行时整轮接近 100ms，而面板每 15 秒就要拉一次。
	// 这里把「最近一条」prepare 成两条语句复用，uptime 合成一条聚合。
	type key struct {
		id   int64
		kind string
	}
	lastT2, err := s.db.Prepare(`SELECT ` + checkCols + ` FROM checks
		WHERE source_id=? AND kind=? AND tier>=2 ORDER BY ts DESC, id DESC LIMIT 1`)
	if err != nil {
		return nil, err
	}
	defer lastT2.Close()
	lastAny, err := s.db.Prepare(`SELECT ` + checkCols + ` FROM checks
		WHERE source_id=? AND kind=? ORDER BY ts DESC, id DESC LIMIT 1`)
	if err != nil {
		return nil, err
	}
	defer lastAny.Close()

	uptimes := map[key]float64{}
	urows, err := s.db.Query(`SELECT source_id,kind,COUNT(*),COALESCE(SUM(ok),0)
		FROM checks WHERE ts>=? AND tier>=2 GROUP BY source_id,kind`, since)
	if err != nil {
		return nil, err
	}
	for urows.Next() {
		var k key
		var total, ok int64
		if err := urows.Scan(&k.id, &k.kind, &total, &ok); err == nil && total > 0 {
			uptimes[k] = float64(ok) / float64(total)
		}
	}
	urows.Close()

	for i := range out {
		out[i].Last = map[string]*Check{}
		out[i].Uptime = map[string]float64{}
		for _, k := range []string{"docker", "github"} {
			// 优先取真实校验（tier>=2）的一条，没有才回退到存活探测，
			// 与原先 ORDER BY (tier>=2) DESC 的语义一致。
			c, err := scanCheckRow(lastT2.QueryRow(out[i].ID, k))
			if err == nil && c == nil {
				c, err = scanCheckRow(lastAny.QueryRow(out[i].ID, k))
			}
			if err == nil && c != nil {
				out[i].Last[k] = c
			}
			if v, ok := uptimes[key{out[i].ID, k}]; ok {
				out[i].Uptime[k] = v
			} else {
				out[i].Uptime[k] = -1
			}
		}
	}

	// P95 一次查完再分桶，逐源查会变成 N 条 SQL。
	// 只算成功样本：失败样本的「延迟」是超时时间，混进去量的是耐心不是速度。
	samples := map[key][]int64{}
	prows, err := s.db.Query(`SELECT source_id,kind,latency_ms FROM checks
		WHERE ts>=? AND tier>=2 AND ok=1 AND latency_ms>0`, since)
	if err == nil {
		for prows.Next() {
			var k key
			var ms int64
			if err := prows.Scan(&k.id, &k.kind, &ms); err == nil {
				samples[k] = append(samples[k], ms)
			}
		}
		prows.Close()
	}
	for i := range out {
		out[i].P95 = map[string]int64{}
		for _, k := range []string{"docker", "github"} {
			out[i].P95[k] = p95(samples[key{out[i].ID, k}])
		}
	}

	// 限流比例：同一窗口内该源的 429 占比。分母只数真实响应，且排除中性结果：
	// code=0（DNS/连接/超时）不算，404/410（没这个文件）、401/403（没资格问）也不算——
	// 它们不是限流，算进分母只会把比率稀释成一个好看的数字。
	type rate struct{ all, c429 int64 }
	rates := map[key]*rate{}
	erows, err := s.db.Query(`SELECT source_id,kind,COUNT(*),SUM(code=429) FROM source_events
		WHERE ts>=? AND code BETWEEN 100 AND 599
		  AND code NOT IN (401,403,404,410) GROUP BY source_id,kind`, since)
	if err == nil {
		for erows.Next() {
			var k key
			var r rate
			if err := erows.Scan(&k.id, &k.kind, &r.all, &r.c429); err == nil {
				rates[k] = &r
			}
		}
		erows.Close()
	}
	for i := range out {
		out[i].Rate429 = map[string]*float64{}
		for _, k := range []string{"docker", "github"} {
			// 两种类型都给出键：无有效样本时是 null，不是缺字段。
			out[i].Rate429[k] = nil
			if r := rates[key{out[i].ID, k}]; r != nil && r.all >= min429Samples {
				v := float64(r.c429) / float64(r.all)
				out[i].Rate429[k] = &v
			}
		}
	}

	// 可信度按类型算，窗口口径与延迟、限流一致。
	for _, k := range []string{"docker", "github"} {
		tm, err := s.trustMap(k, time.Now())
		if err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].Trust == nil {
				out[i].Trust = map[string]Trust{}
			}
			t := tm[out[i].ID]
			t.Level = trustLevel(t)
			out[i].Trust[k] = t
		}
	}
	if err := s.attachGroups(out); err != nil {
		return nil, err
	}
	return out, nil
}

// min429Samples 报限流比例的最小样本数：十次里两次 429 说明不了什么，
// 写成 20% 会让人立刻停掉一台好源。
const min429Samples = 20

// minP95Samples 出 P95 的最小样本数：三个样本的 95 分位就是最大值，
// 标成 P95 会让人误以为有统计意义。
const minP95Samples = 5

// p95 用 nearest-rank 算第 95 百分位；样本不足返回 0。
func p95(v []int64) int64 {
	if len(v) < minP95Samples {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	idx := (len(v)*95 + 99) / 100 // ceil(0.95*n)
	if idx < 1 {
		idx = 1
	}
	return v[idx-1]
}

func (s *Store) GetSource(id int64) (*Source, error) {
	x, err := scanSource(s.db.QueryRow(`SELECT `+srcCols+` FROM sources WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) ByURL(baseURL string) (*Source, error) {
	x, err := scanSource(s.db.QueryRow(`SELECT `+srcCols+` FROM sources WHERE base_url=?`, baseURL))
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) AddSource(x Source) (int64, error) {
	if x.Origin == "" {
		x.Origin = "manual"
	}
	if x.Status == "" {
		x.Status = "pending"
	}
	if x.CreatedAt == 0 {
		x.CreatedAt = time.Now().Unix()
	}
	if x.DockerCap == 0 && x.GithubCap == 0 {
		x.DockerCap, x.GithubCap = CapUnknown, CapUnknown
	}
	// ghcr 维度同样：未显式设置（零值）当未知，交给探测识别。
	if x.GhcrCap == 0 {
		x.GhcrCap = CapUnknown
	}
	// 3.5.6 Canonical 语义：
	//   - 显式 Host：按 Host 归属；
	//   - 纯 GHCR：ghcr.io；
	//   - 仅 GitHub：Host 保持空（not_applicable）；
	//   - 能力未知且没有显式 Host：Host 保持空（unclassified），不能把未知类型
	//     无条件当成 Docker Hub；手工新增入口如需 Hub 默认，应由调用方显式写入
	//     docker.io（api 层已经这样做）。
	if x.RepoHost == "" {
		if x.GhcrCap == RegistryCapYes && x.DockerCap != RegistryCapYes {
			x.RepoHost = "ghcr.io"
		} else if x.DockerCap == RegistryCapYes || x.GithubCap == CapNo {
			// 有 Docker 能力，或明确不是 GitHub 源：默认 Hub。
			x.RepoHost = "docker.io"
		}
	}
	host, err := registryhost.Check(x.RepoHost)
	if err != nil {
		return 0, err
	}
	x.RepoHost = host
	if x.RegistryCap == 0 {
		x.RegistryCap = CapUnknown
	}
	if x.StatusAt == 0 {
		x.StatusAt = x.CreatedAt
	}
	// 源与 recovery 行必须同一事务落库：只建源不建 recovery，会让新源在
	// 「cap 未知但 recovery 缺失」状态下被 §6 的到期查询反复漏掉。
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT OR IGNORE INTO sources(base_url,name,site_name,origin,enabled,status,note,docker_cap,github_cap,ghcr_cap,git_cap,preferred,status_at,created_at,repo_host,registry_cap,registry_path_mode)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		x.BaseURL, x.Name, x.SiteName, x.Origin, x.Enabled, x.Status, x.Note, x.DockerCap, x.GithubCap, x.GhcrCap, x.GitCap, x.Preferred, x.StatusAt, x.CreatedAt, x.RepoHost, x.RegistryCap, x.RegistryPath)
	if err != nil {
		return 0, err
	}
	// INSERT OR IGNORE 时 LastInsertId 返回上次的 rowid，
	// 得用 RowsAffected 判断是否真的写入。
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrExists
	}
	id, _ := res.LastInsertId()
	// 归属 Host 的源立即进入待探测集合：gen 取该 Host 当前代次，
	// 没有代次行时按 0（与 CommitHostProbe 的缺行规则一致）。
	// 空 Host（历史 Docker Hub）、docker.io 与 ghcr.io 不参与 Host 探测，不建行。
	if x.RepoHost != "" && x.RepoHost != "ghcr.io" && x.RepoHost != "docker.io" {
		now := time.Now().Unix()
		if _, err := tx.Exec(`INSERT INTO registry_host_recovery
(source_id, family, scope, state, gen, last_probe_at, last_success_at, next_probe_at, rec_failures, retry_failures, last_error, updated_at)
VALUES(?, 'registry', ?, 'initial', COALESCE((SELECT gen FROM registry_host_gen WHERE host=?), 0), 0, 0, ?, 0, 0, '', ?)
ON CONFLICT(source_id, family, scope) DO UPDATE SET
  state='initial', gen=excluded.gen, last_probe_at=0,
  next_probe_at=excluded.next_probe_at, rec_failures=0, retry_failures=0, last_error='',
  updated_at=excluded.updated_at`,
			id, x.RepoHost, x.RepoHost, now, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

var ErrExists = errors.New("source already exists")

// ErrArchived 已归档的源不能直接启用，只能走恢复流程。
var ErrArchived = errors.New("已归档的源不能直接启用，请用恢复流程重新验证")

func (s *Store) UpdateSource(id int64, enabled *bool, name, note *string, preferred *bool, groupID *int64) error {
	if enabled != nil {
		// 启用状态与 recovery 的重置必须同生共死：只改一半会留下
		// 「已启用但仍背着旧退避」的状态。
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()

		var wasEnabled int
		var status string
		if err := tx.QueryRow(`SELECT enabled, status FROM sources WHERE id=?`, id).Scan(&wasEnabled, &status); err != nil {
			return err
		}
		if *enabled && status == "archived" {
			// 归档是已经做过的判断，不能被「启用」开关顺手撤销；
			// 恢复必须走显式流程（status=pending → 重新探测验证）。
			return ErrArchived
		}
		if _, err := tx.Exec(`UPDATE sources SET enabled=? WHERE id=?`, *enabled, id); err != nil {
			return err
		}
		if *enabled && wasEnabled == 0 {
			// 真实的 0→1 转换：退避次数无条件清零，与调度时间无关。
			// 重复执行 enabled=1 不触发，避免用户点两下就把阶梯重置。
			if _, err := tx.Exec(`UPDATE registry_host_recovery SET retry_failures=0
WHERE source_id=? AND family='registry'`, id); err != nil {
				return err
			}
			// 原拉近逻辑保持不变：只把停在未来的排期拉回现在；
			// 已到期的行保持原值，不清零也不推后。
			now := time.Now().Unix()
			if _, err := tx.Exec(`UPDATE registry_host_recovery SET next_probe_at=?
WHERE source_id=? AND family='registry' AND next_probe_at>?`, now, id, now); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if name != nil && *name != "" {
		if _, err := s.db.Exec(`UPDATE sources SET name=? WHERE id=?`, *name, id); err != nil {
			return err
		}
	}
	if note != nil {
		if _, err := s.db.Exec(`UPDATE sources SET note=? WHERE id=?`, *note, id); err != nil {
			return err
		}
	}
	if preferred != nil {
		if _, err := s.db.Exec(`UPDATE sources SET preferred=? WHERE id=?`, *preferred, id); err != nil {
			return err
		}
	}
	if groupID != nil {
		if _, err := s.db.Exec(`UPDATE sources SET group_id=? WHERE id=?`, *groupID, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetCaps(id int64, docker, github, ghcr int) error {
	_, err := s.db.Exec(`UPDATE sources SET docker_cap=?, github_cap=?, ghcr_cap=? WHERE id=?`, docker, github, ghcr, id)
	return err
}

func (s *Store) SetStatus(id int64, status string) error {
	return s.SetStatusWhy(id, status, "")
}

// SetCapsLive / SetStatusLive 探测写回专用的条件更新：只改没被归档的行，
// 返回是否真的写入。
// 探测发起到写回有几十秒，用户可能刚归档该源；把「状态仍是活的」并进
// UPDATE，由数据库保证不覆盖人工决定。
func (s *Store) SetCapsLive(id int64, docker, github, ghcr int, ghcrPrefix bool) (bool, error) {
	res, err := s.db.Exec(`UPDATE sources SET docker_cap=?, github_cap=?, ghcr_cap=?, ghcr_prefix=?
		WHERE id=? AND status<>'archived'`, docker, github, ghcr, ghcrPrefix, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetRangeCap 记一次 Range 预检结论：1 支持分片，0 不支持（整层吐）。
func (s *Store) SetRangeCap(id int64, cap int) error {
	_, err := s.db.Exec(`UPDATE sources SET range_cap=?, range_at=? WHERE id=?`,
		cap, time.Now().Unix(), id)
	return err
}

func (s *Store) SetStatusLive(id int64, status, reason string) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`UPDATE sources SET
		status_at = CASE WHEN status<>? THEN ? ELSE status_at END,
		status_reason = CASE WHEN status<>? THEN ? ELSE status_reason END,
		range_at = CASE WHEN status<>? AND ?='usable' THEN 0 ELSE range_at END,
		status = ?
		WHERE id=? AND status<>'archived'`, status, now, status, reason, status, status, status, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetStatusWhy 改状态并记时间和原因。状态没变不动 status_at，
// 否则常年 dead 的源每次探测都把「坏了多久」清零，永远归档不了。
func (s *Store) SetStatusWhy(id int64, status, reason string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE sources SET
		status_at = CASE WHEN status<>? THEN ? ELSE status_at END,
		status_reason = CASE WHEN status<>? THEN ? ELSE status_reason END,
		status = ?
		WHERE id=?`, status, now, status, reason, status, id)
	return err
}

// TouchProbe0 清掉探测时间，让恢复的源不受低频门限阻挡，立刻可探。
func (s *Store) TouchProbe0(id int64) error {
	_, err := s.db.Exec(`UPDATE sources SET probe_at=0 WHERE id=?`, id)
	return err
}

// TouchProbe 记一次探测尝试（不论成败）。
func (s *Store) TouchProbe(id int64) error {
	_, err := s.db.Exec(`UPDATE sources SET probe_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

// ConsecutiveClean 数最近连续多少次 tier>=2 探测通过且无篡改。
// 封禁源靠它放回：一次干净不够，要连续两次。
func (s *Store) ConsecutiveClean(sourceID int64, kind string) int {
	rows, err := s.db.Query(`SELECT ok, digest_ok FROM checks
		WHERE source_id=? AND kind=? AND tier>=2 ORDER BY ts DESC LIMIT 5`, sourceID, kind)
	if err != nil {
		return 0
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ok bool
		var digestOK int
		if err := rows.Scan(&ok, &digestOK); err != nil {
			return n
		}
		if ok && digestOK == 1 {
			n++
			continue
		}
		break
	}
	return n
}

// ArchiveStale 归档「坏了/被封超过 before 秒」的源：不再探测、不参与调度，
// 记录保留可恢复。返回归档条数。
func (s *Store) ArchiveStale(before int64) (int64, error) {
	res, err := s.db.Exec(`UPDATE sources SET status='archived', status_at=?, status_reason=?
		WHERE enabled=1 AND status IN ('dead','blocked') AND status_at>0 AND status_at<=?`,
		time.Now().Unix(), "长时间不可用，自动归档", before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupArchived 删除归档超过 before 秒的源及检测记录、调度事件。
// 只删 archived：别的状态再久也留着，删数据是单向操作。
func (s *Store) CleanupArchived(before int64) (int64, error) {
	rows, err := s.db.Query(`SELECT id FROM sources WHERE status='archived' AND status_at>0 AND status_at<=?`, before)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if err := s.DeleteSource(id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) DeleteSource(id int64) error {
	// 三条语句必须一起成功或一起失败：删一半会留下指向不存在源的检查记录，
	// 面板上出现「幽灵源」，再删一次也清不掉。
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// source_events 没建外键要显式清；checks 有 CASCADE，但老库可能没开
	// foreign_keys，也一并清掉，不留孤儿。
	for _, q := range []string{
		`DELETE FROM source_events WHERE source_id=?`,
		`DELETE FROM breaker_state WHERE source_id=?`,
		`DELETE FROM checks WHERE source_id=?`,
		`DELETE FROM source_health WHERE source_id=?`,
		`DELETE FROM sources WHERE id=?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AddCheck(c Check) error {
	if c.TS == 0 {
		c.TS = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO checks(source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,blob_ok,ttfb_ms,tls_ok,cert_days,http_code,error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.SourceID, c.Kind, c.TS, c.Tier, c.OK, c.LatencyMS, c.SpeedKBps, c.Digest, c.DigestOK, c.BlobOK, c.TTFBMS, c.TLSOK, c.CertDays, c.HTTPCode, c.Error)
	return err
}

// LastCheck 返回某源某类型最近一次检测，优先 tier>=2 的真实探测。
func (s *Store) LastCheck(sourceID int64, kind string) (*Check, error) {
	return scanCheckRow(s.db.QueryRow(`SELECT `+checkCols+` FROM checks
		WHERE source_id=? AND kind=? ORDER BY (tier>=2) DESC, ts DESC, id DESC LIMIT 1`, sourceID, kind))
}

// scanCheckRow 把一行检测记录扫进 Check；没有记录时返回 (nil, nil)。
func scanCheckRow(row *sql.Row) (*Check, error) {
	var c Check
	err := row.Scan(&c.ID, &c.SourceID, &c.Kind, &c.TS, &c.Tier, &c.OK, &c.LatencyMS, &c.SpeedKBps,
		&c.Digest, &c.DigestOK, &c.BlobOK, &c.TTFBMS, &c.TLSOK, &c.CertDays, &c.HTTPCode, &c.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ConsecutiveFailures 返回某类型最近连续失败次数（按时间倒序，遇到成功即停）。
func (s *Store) ConsecutiveFailures(sourceID int64, kind string) int {
	rows, err := s.db.Query(`SELECT ok FROM checks WHERE source_id=? AND kind=? AND tier>=2 ORDER BY ts DESC LIMIT 5`, sourceID, kind)
	if err != nil {
		return 0
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ok bool
		if err := rows.Scan(&ok); err != nil {
			return n
		}
		if ok {
			break
		}
		n++
	}
	return n
}

// ObserveStreak 返回观察池口径的连续结果：最近一次 tier>=2 探测之后，
// 该类型 L1 连续通过和连续失败的次数。L2 一旦落库，计数自然清零。
func (s *Store) ObserveStreak(sourceID int64, kind string) (pass, fail int) {
	// 按自增 id 而不是 ts 切分：同一秒内的多次探测 ts 相同，分不开。
	var sinceID int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM checks WHERE source_id=? AND kind=? AND tier>=2`,
		sourceID, kind).Scan(&sinceID); err != nil {
		return 0, 0
	}
	rows, err := s.db.Query(`SELECT ok FROM checks WHERE source_id=? AND kind=? AND tier=1 AND id>?
		ORDER BY id DESC LIMIT 8`, sourceID, kind, sinceID)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var ok bool
		if err := rows.Scan(&ok); err != nil {
			return pass, fail
		}
		if ok {
			if fail > 0 {
				break
			}
			pass++
			continue
		}
		if pass > 0 {
			break
		}
		fail++
	}
	return pass, fail
}

// Series 返回某源最近 limit 条 tier>=2 的检测（两种类型混合），按时间正序。
func (s *Store) Series(sourceID int64, limit int) ([]Check, error) {
	rows, err := s.db.Query(`SELECT id,source_id,kind,ts,tier,ok,latency_ms,speed_kbps,digest,digest_ok,blob_ok,ttfb_ms,tls_ok,cert_days,http_code,error
		FROM (SELECT * FROM checks WHERE source_id=? AND tier>=2 ORDER BY ts DESC LIMIT ?) ORDER BY ts ASC`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		if err := rows.Scan(&c.ID, &c.SourceID, &c.Kind, &c.TS, &c.Tier, &c.OK, &c.LatencyMS, &c.SpeedKBps, &c.Digest, &c.DigestOK, &c.BlobOK, &c.TTFBMS, &c.TLSOK, &c.CertDays, &c.HTTPCode, &c.Error); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnabledSources 返回所有启用的源（runner 自己决定测哪几种类型）。
func (s *Store) EnabledSources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT ` + srcCols + ` FROM sources WHERE enabled=1 AND status<>'archived'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		x, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n)
	return n, err
}

// Prune 删除 retention 之前的检测记录。
func (s *Store) Prune(before int64) error {
	_, err := s.db.Exec(`DELETE FROM checks WHERE ts < ?`, before)
	return err
}

// ---- settings ----

func (s *Store) GetSetting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// Tx 在一个事务里执行 fn，出错整体回滚。
// 「先判断再写」的场景用它：单条 SQL 的原子性管不了跨语句状态。
func (s *Store) Tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) AllSettings() map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT key,value FROM settings`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// ---- sessions ----

func (s *Store) CreateSession(token string, ttl time.Duration) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO sessions(token,created_at,expires_at) VALUES(?,?,?)`,
		token, now, now+int64(ttl.Seconds()))
	return err
}

func (s *Store) ValidSession(token string) bool {
	if token == "" {
		return false
	}
	var exp int64
	if err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token=?`, token).Scan(&exp); err != nil {
		return false
	}
	if exp < time.Now().Unix() {
		s.DeleteSession(token)
		return false
	}
	return true
}

func (s *Store) DeleteSession(token string) {
	s.db.Exec(`DELETE FROM sessions WHERE token=?`, token)
}

func (s *Store) DeleteAllSessions() {
	s.db.Exec(`DELETE FROM sessions`)
}

func (s *Store) PruneSessions() {
	s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
}

func (s *Store) String() string { return fmt.Sprintf("store(%p)", s.db) }

// SetRecoveryGuard 注入路由层屏蔽集。nil 视为装配错误：生产路径不允许
// 「没有 Guard 就静默放行」。
func (s *Store) SetRecoveryGuard(g *RecoveryGuard) error {
	if g == nil {
		return errors.New("store: recovery guard 不能为 nil")
	}
	s.guard = g
	return nil
}

// RecoveryGuard 返回当前注入的屏蔽集（未注入时为 nil，仅测试可达）。
func (s *Store) RecoveryGuard() *RecoveryGuard { return s.guard }

// SetRecoveryTTL 设置成功结论的有效期；<=0 回到默认 24h。
func (s *Store) SetRecoveryTTL(d time.Duration) {
	if d <= 0 {
		d = RecoveryDefaultTTL
	}
	s.recoveryTTL = d
}

// RecoveryTTL 是候选准入与缓存实时检查共用的有效期。
func (s *Store) RecoveryTTL() time.Duration {
	if s.recoveryTTL <= 0 {
		return RecoveryDefaultTTL
	}
	return s.recoveryTTL
}

// SetClock 仅供测试注入时间源；生产使用 time.Now。
func (s *Store) SetClock(f func() time.Time) {
	if f == nil {
		f = time.Now
	}
	s.clock = f
}

func (s *Store) nowTime() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock()
}
