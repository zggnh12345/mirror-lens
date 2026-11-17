package secret

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 仓库凭据的落盘格式。
//
// 文件里只有密文，版本号（gen）与摘要（cred_digest）唯一权威在
// SQLite 的 registry_host_gen 表：跨文件与数据库没有事务，把 gen 同时
// 写两处只会带来双写不一致。写入顺序固定为「文件先、数据库后」，
// 中间崩溃由 Reconcile 在启动时补齐。
const (
	credsVersion   = 1
	credsAADPrefix = "mirror-lens/registry-creds/v1|"
	digestPrefix   = "MLCRED1|"
)

var (
	// ErrNotReconciled 表示进程启动后还没跑对账，此时读凭据一律拒绝。
	ErrNotReconciled = errors.New("仓库凭据尚未与数据库对账")
	// ErrCredentialPending 表示文件已换新但数据库版本还没提交。
	ErrCredentialPending = errors.New("凭据文件已更新但版本未提交")
	// ErrRecordMissing 表示数据库有摘要、文件里却找不到该 Host。
	// 这可能是文件被截断或从旧备份恢复，不能当成「用户删除了凭据」。
	ErrRecordMissing = errors.New("凭据文件缺少数据库已记录的 Host")
	// ErrCorrupt 表示文件结构错误、AEAD 校验失败或记录不合法。
	ErrCorrupt = errors.New("凭据文件损坏")
)

// Credential 是解密后的凭据内容。
//
// TokenMethod / TokenAuth 与用户名密码一起封在同一个 AEAD 里，因此
// 「凭据 + 认证方式」天然来自同一次一致性读取，不会出现拼装错版本。
// Mode 是 3.5.15 的显式认证模式；历史记录没有它，按 CredentialMode() 派生。
type Credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Mode: ""（历史记录）| basic | bearer_token_basic | bearer_token_none
	Mode string `json:"mode,omitempty"`
	// TokenMethod: "" | "GET"（POST 明确拒绝）。
	TokenMethod string `json:"token_method,omitempty"`
	// TokenAuth: ""（自动）| "none"（永不带 Basic）| "basic"（必须带 Basic）。
	TokenAuth string `json:"token_auth,omitempty"`
}

type cipherRecord struct {
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

type credsDoc struct {
	Version int                     `json:"version"`
	Hosts   map[string]cipherRecord `json:"hosts"`
	// Revoked 记录「显式删除」过的 Host。没有它，就无法区分「用户删了凭据」
	// 和「文件被截断导致记录丢失」——后者绝不能自动当成删除。
	Revoked map[string]bool `json:"revoked,omitempty"`
}

// DB 是凭据存储需要的版本接口；由 store.Store 实现。
type DB interface {
	HostGenState(host string) (int64, string, error)
	HostCredStates() ([]store.CredState, error)
	ApplyCredChanges([]store.CredChange) error
	// ApplyCredWrite 是 3.5.15 的唯一提交路径：CAS + 同事务审计。
	ApplyCredWrite(store.CredWrite) (store.CredWriteResult, error)
}

// Store 管理 <data>/registry_creds.enc。
//
// 并发：同一进程内用 mu 串行化「读 → 改 → 加密 → 原子替换 → 算摘要」整个
// 过程；另外用一个 .lock 文件做 flock，防止同机另一个进程（比如手工工具）
// 同时改同一份文件造成丢失更新。
type Store struct {
	path string
	key  []byte
	db   DB

	mu         sync.Mutex
	reconciled bool
}

// NewCredStore 打开（或准备创建）凭据文件。key 必须是 32 字节。
func NewCredStore(path string, key []byte, db DB) (*Store, error) {
	if path == "" {
		return nil, errors.New("凭据文件路径为空")
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("密钥长度应为 %d 字节", KeySize)
	}
	if db == nil {
		return nil, errors.New("缺少版本数据库")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &Store{path: abs, key: key, db: db}, nil
}

// Path 返回凭据文件路径。
func (s *Store) Path() string { return s.path }

// Reconciled 报告启动对账是否已经成功完成。
func (s *Store) Reconciled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconciled
}

// Put 是 3.5.14 的兼容包装，见 credstate.go 的 PutCredential。

// Get 返回某 Host 的凭据。未对账、文件与数据库摘要不一致时拒绝返回。
// 其他 Host 的记录损坏不影响本 Host（记录级隔离）。
func (s *Store) Get(rawHost string) (Credential, bool, error) {
	host, err := credentialStatusHost(rawHost)
	if err != nil {
		return Credential{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reconciled {
		return Credential{}, false, ErrNotReconciled
	}
	doc, err := s.readDoc()
	if err != nil {
		return Credential{}, false, err
	}
	rec, ok := doc.Hosts[host]
	if !ok {
		// 文件里没有、但数据库还记着旧摘要：删除已写文件但版本没提交，
		// 此刻不能对外表现为「没有凭据」。
		_, cur, err := s.db.HostGenState(host)
		if err != nil {
			return Credential{}, false, err
		}
		if cur != "" && cur != emptyDigest(host) {
			return Credential{}, false, fmt.Errorf("%w: %s", ErrCredentialPending, host)
		}
		return Credential{}, false, nil
	}
	// 先解密：记录级损坏必须报 ErrCorrupt（该 Host 隔离），
	// 不能因为摘要对不上就误报成「Pending」，把损坏掩盖成可恢复状态。
	c, err := s.open(host, rec)
	if err != nil {
		return Credential{}, false, err
	}
	_, cur, err := s.db.HostGenState(host)
	if err != nil {
		return Credential{}, false, err
	}
	if cur != recordDigest(host, rec) {
		return Credential{}, false, fmt.Errorf("%w: %s", ErrCredentialPending, host)
	}
	return c, true, nil
}

// Snapshot 是一次探测使用的完整凭据版本。
//
// Host、Generation、Digest 与凭据内容必须来自同一次加锁读取；调用方不能
// 分别读 gen、digest 和凭据再拼起来。digest 为 "" 表示数据库里还没有该
// Host 的代次记录（与 SetHostCaps 的 COALESCE 语义一致）。
type Snapshot struct {
	Host          string
	Generation    int64
	Digest        string
	Credential    Credential
	HasCredential bool
}

// Snapshot 一次性取回某 Host 的凭据版本。
func (s *Store) Snapshot(rawHost string) (Snapshot, error) {
	host, err := credentialStatusHost(rawHost)
	if err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reconciled {
		return Snapshot{}, ErrNotReconciled
	}
	doc, err := s.readDoc()
	if err != nil {
		return Snapshot{}, err
	}
	gen, cur, err := s.db.HostGenState(host)
	if err != nil {
		return Snapshot{}, err
	}
	rec, ok := doc.Hosts[host]
	if !ok {
		// 文件里没有：数据库必须也是「无凭据」，否则是删除只写了一半。
		if cur != "" && cur != emptyDigest(host) {
			return Snapshot{}, fmt.Errorf("%w: %s", ErrCredentialPending, host)
		}
		return Snapshot{Host: host, Generation: gen, Digest: cur}, nil
	}
	// 先解密，理由同 Get：记录级损坏不能被摘要比较掩盖。
	c, err := s.open(host, rec)
	if err != nil {
		return Snapshot{}, err
	}
	digest := recordDigest(host, rec)
	if cur != digest {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrCredentialPending, host)
	}
	return Snapshot{Host: host, Generation: gen, Digest: digest, Credential: c, HasCredential: true}, nil
}

// Has 只报告是否配置了凭据，不返回密码。
func (s *Store) Has(rawHost string) (bool, string, error) {
	c, ok, err := s.Get(rawHost)
	if err != nil || !ok {
		return ok, "", err
	}
	return true, c.Username, nil
}

// Reconcile 是 3.5.14 的兼容入口：按 Host 对账后返回聚合错误。
//
// 单条记录损坏只隔离该 Host（其余 Host 继续 Ready），但为了调用方能看见
// 问题，这里把隔离 Host 归类成 ErrCorrupt / ErrRecordMissing 返回。
// 新代码请用 ReconcileAll，它把「隔离」与「致命」分开。
func (s *Store) Reconcile() (int, error) {
	n, quarantined, err := s.ReconcileAll()
	if err != nil {
		return n, err
	}
	for _, st := range quarantined {
		if st.Reason == "record_missing" {
			return n, fmt.Errorf("%w: %s", ErrRecordMissing, st.Host)
		}
		return n, fmt.Errorf("%w: %s（%s）", ErrCorrupt, st.Host, st.Reason)
	}
	return n, nil
}

// credentialHost 只接受规范化后的受支持 Host；空串（Docker Hub）与
// 非法字符串都不接受。
//
// 读取/对账/删除走这一层：3.5.14 遗留文件里可能有非 allowlist 的 Host，
// 读路径必须能识别并清理它们，不能把整份文件判成损坏。
func credentialHost(raw string) (string, error) {
	host, err := NormalizeHost(raw)
	if err != nil {
		return "", err
	}
	if host == "" || host == "docker.io" || host == "ghcr.io" {
		return "", errors.New("Docker Hub/GHCR 不支持配置 Host 代次凭据")
	}
	// 1.6 D.1：*.pkg.dev 本版不支持凭据；localhost 也不得进入凭据 Host。
	// 写入门禁之外，状态/列表路径也必须用同一收敛规则，否则 DB 里的
	// 历史行仍会被当成可配置凭据的 Host 展示。
	if registryhost.IsArtifactRegistry(host) {
		return "", errors.New("*.pkg.dev 需要 GCP ADC，本版不支持配置凭据")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", errors.New("localhost 不允许作为凭据 Host")
	}
	return host, nil
}

// credentialStatusHost 是运行/API 读路径的门禁：除格式与安全规则外，
// 还必须在凭据 allowlist 内。文件解析仍用 credentialHost，避免历史
// 非 allowlist 记录把整份文件判成损坏。
func credentialStatusHost(raw string) (string, error) {
	host, err := credentialHost(raw)
	if err != nil {
		return "", err
	}
	if !CredentialHostAllowed(host) {
		return "", errors.New("该 Host 不在 3.5.15 私有凭据 allowlist 内")
	}
	return host, nil
}

// credentialWriteHost 是 3.5.15 的写入门禁：除格式合法外，还必须在
// 3.5.15-D.1 的代码 allowlist 内。*.pkg.dev / registry.gitlab.com 明确排除：
// 前者需要 GCP ADC，后者属于 3.5.15a。
func credentialWriteHost(raw string) (string, error) {
	host, err := credentialHost(raw)
	if err != nil {
		return "", err
	}
	if host == "registry.gitlab.com" {
		return "", errors.New("GitLab 专用凭据属于 3.5.15a，本版不开放")
	}
	if registryhost.IsArtifactRegistry(host) {
		return "", errors.New("*.pkg.dev 需要 GCP ADC，本版不支持配置凭据")
	}
	if !CredentialHostAllowed(host) {
		return "", errors.New("该 Host 不在 3.5.15 私有凭据 allowlist 内（仅 quay.io / gcr.io）")
	}
	return host, nil
}

func (s *Store) seal(host string, c Credential) (cipherRecord, error) {
	gcm, err := aead(s.key)
	if err != nil {
		return cipherRecord{}, err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return cipherRecord{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return cipherRecord{}, fmt.Errorf("生成随机数: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plain, []byte(credsAADPrefix+host))
	return cipherRecord{
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(ct),
	}, nil
}

func (s *Store) open(host string, r cipherRecord) (Credential, error) {
	gcm, err := aead(s.key)
	if err != nil {
		return Credential{}, err
	}
	nonce, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Nonce))
	if err != nil || len(nonce) != gcm.NonceSize() {
		return Credential{}, fmt.Errorf("%w: %s 的 nonce 不合法", ErrCorrupt, host)
	}
	ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.CT))
	if err != nil || len(ct) == 0 {
		return Credential{}, fmt.Errorf("%w: %s 的密文不合法", ErrCorrupt, host)
	}
	plain, err := gcm.Open(nil, nonce, ct, []byte(credsAADPrefix+host))
	if err != nil {
		// AAD 绑定 Host：把 A 的密文挪到 B 名下同样会失败。
		return Credential{}, fmt.Errorf("%w: %s 解密失败（密钥不匹配、密文被改动或 Host 被调换）", ErrCorrupt, host)
	}
	var c Credential
	if err := json.Unmarshal(plain, &c); err != nil {
		return Credential{}, fmt.Errorf("%w: %s 的明文不是合法 JSON", ErrCorrupt, host)
	}
	return c, nil
}

// readDoc 只做结构校验：JSON 可解析、版本已知、Host 合法、记录字段非空。
// 不做 AEAD：单条记录解密失败属于「记录级损坏」，只能隔离该 Host，
// 不能让其他 Host 一起 fail closed。
func (s *Store) readDoc() (*credsDoc, error) {
	doc := &credsDoc{Version: credsVersion, Hosts: map[string]cipherRecord{}, Revoked: map[string]bool{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读凭据文件: %w", err)
	}
	if err := json.Unmarshal(b, doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if doc.Version != credsVersion {
		return nil, fmt.Errorf("%w: 未知版本 %d", ErrCorrupt, doc.Version)
	}
	if doc.Hosts == nil {
		doc.Hosts = map[string]cipherRecord{}
	}
	if doc.Revoked == nil {
		doc.Revoked = map[string]bool{}
	}
	// 同一 Host 不能既是有效记录又是删除标记：两种解释同时成立时，
	// 无论取哪个都可能误删或误用凭据，直接判文件损坏（这是文件级矛盾，
	// 不属于单条记录损坏）。
	for host := range doc.Revoked {
		if _, ok := doc.Hosts[host]; ok {
			return nil, fmt.Errorf("%w: %s 同时存在密文记录与删除标记", ErrCorrupt, host)
		}
	}
	for host := range doc.Revoked {
		norm, err := credentialHost(host)
		if err != nil || norm != host {
			return nil, fmt.Errorf("%w: 已删除记录里的 Host %q 不合法", ErrCorrupt, host)
		}
	}
	for host, rec := range doc.Hosts {
		norm, err := credentialHost(host)
		if err != nil || norm != host {
			return nil, fmt.Errorf("%w: Host %q 不合法", ErrCorrupt, host)
		}
		if rec.Nonce == "" || rec.CT == "" {
			// 字段缺失是记录级问题，交给 open() 在按 Host 读取时报错。
			continue
		}
	}
	return doc, nil
}

// readValidated 读取并完整校验文件：结构 + 每条记录的 AEAD。
// 仅用于需要「整文件可信」的运维检查；运行路径按 Host 隔离读取。
func (s *Store) readValidated() (*credsDoc, error) {
	doc, err := s.readDoc()
	if err != nil {
		return nil, err
	}
	for host, rec := range doc.Hosts {
		if _, err := s.open(host, rec); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

func (s *Store) writeAtomic(doc *credsDoc) error {
	doc.Version = credsVersion
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建凭据目录: %w", err)
	}
	f, err := os.CreateTemp(dir, ".registry_creds-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// rename 只保证目录项换了，断电后仍可能丢；父目录再 fsync 一次。
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) lockFile() (func(), error) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开凭据锁: %w", err)
	}
	if err := flockExclusive(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("锁定凭据文件: %w", err)
	}
	return func() {
		_ = funlock(f)
		_ = f.Close()
	}, nil
}

// recordDigest 是「该 Host 当前凭据记录」的摘要：包含 Host 名，逐 Host 独立。
// 只改 B 的凭据不会影响 A 的摘要。
//
// 摘要输入用「长度前缀 + 字段」编码，不依赖 JSON 的序列化结果，也不靠
// 分隔符去切分：字段长度先写进去，任何字段内容（哪怕含分隔符）都不会
// 让两条不同记录拼出同一段字节。
func recordDigest(host string, r cipherRecord) string {
	var b bytes.Buffer
	b.WriteString(digestPrefix)
	writeField(&b, "record")
	writeField(&b, host)
	writeField(&b, r.Nonce)
	writeField(&b, r.CT)
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

// emptyDigest 是「显式删除」的固定摘要。对账看到它，就知道文件里缺这条
// 记录是用户删除的结果，而不是文件损坏或丢失。标签与 recordDigest 不同，
// 两者不可能撞到同一段输入。
func emptyDigest(host string) string {
	var b bytes.Buffer
	b.WriteString(digestPrefix)
	writeField(&b, "empty")
	writeField(&b, host)
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

// HasCredentialDigest 判断数据库摘要是否代表真实凭据。
// 空摘要（从未配置）与 emptyDigest（显式删除）都不是凭据，不能把
// 「已删除」误当成「有凭据」传给候选探测快照。
func HasCredentialDigest(host, digest string) bool {
	return digest != "" && digest != emptyDigest(host)
}

// writeField 写「十进制长度:内容」，是摘要编码里唯一的字段边界来源。
func writeField(b *bytes.Buffer, s string) {
	fmt.Fprintf(b, "%d:", len(s))
	b.WriteString(s)
}
