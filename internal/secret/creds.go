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
type Credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
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

// Put 写入（或覆盖）某 Host 的凭据，返回该 Host 的新摘要。
//
// 返回值错误里若带「数据库版本未提交」，说明文件已换新但 DB 事务失败：
// 此时 Probe 会因摘要不符拒绝启动，重启进程跑 Reconcile 即可补齐。
func (s *Store) Put(rawHost, username, password string) (string, error) {
	host, err := credentialHost(rawHost)
	if err != nil {
		return "", err
	}
	if username == "" {
		return "", errors.New("用户名不能为空")
	}
	if password == "" {
		return "", errors.New("密码不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return "", err
	}
	defer unlock()

	doc, err := s.readValidated()
	if err != nil {
		return "", err
	}
	rec, err := s.seal(host, Credential{Username: username, Password: password})
	if err != nil {
		return "", err
	}
	doc.Hosts[host] = rec
	delete(doc.Revoked, host)
	if err := s.writeAtomic(doc); err != nil {
		return "", err
	}
	digest := recordDigest(host, rec)
	if err := s.db.ApplyCredChanges([]store.CredChange{{Host: host, Digest: &digest}}); err != nil {
		return "", fmt.Errorf("凭据已写入 %s，但数据库版本未提交（重启后自动对账）：%w", s.path, err)
	}
	return digest, nil
}

// Delete 显式删除某 Host 的凭据：文件里移除记录，数据库记为「无凭据摘要」。
func (s *Store) Delete(rawHost string) error {
	host, err := credentialHost(rawHost)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	doc, err := s.readValidated()
	if err != nil {
		return err
	}
	if _, ok := doc.Hosts[host]; !ok {
		// 文件里本来就没有：只有数据库已经是「无凭据」才算幂等成功；
		// 否则就是记录丢了，不能顺手把它当作删除。
		_, cur, err := s.db.HostGenState(host)
		if err != nil {
			return err
		}
		if cur == "" || cur == emptyDigest(host) {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrRecordMissing, host)
	}
	delete(doc.Hosts, host)
	if doc.Revoked == nil {
		doc.Revoked = map[string]bool{}
	}
	doc.Revoked[host] = true
	if err := s.writeAtomic(doc); err != nil {
		return err
	}
	digest := emptyDigest(host)
	if err := s.db.ApplyCredChanges([]store.CredChange{{Host: host, Digest: &digest}}); err != nil {
		return fmt.Errorf("凭据已删除但数据库版本未提交（重启后自动对账）：%w", err)
	}
	return nil
}

// Get 返回某 Host 的凭据。未对账、文件与数据库摘要不一致时拒绝返回。
func (s *Store) Get(rawHost string) (Credential, bool, error) {
	host, err := credentialHost(rawHost)
	if err != nil {
		return Credential{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reconciled {
		return Credential{}, false, ErrNotReconciled
	}
	doc, err := s.readValidated()
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
	_, cur, err := s.db.HostGenState(host)
	if err != nil {
		return Credential{}, false, err
	}
	if cur != recordDigest(host, rec) {
		return Credential{}, false, fmt.Errorf("%w: %s", ErrCredentialPending, host)
	}
	c, err := s.open(host, rec)
	if err != nil {
		return Credential{}, false, err
	}
	return c, true, nil
}

// Has 只报告是否配置了凭据，不返回密码。
func (s *Store) Has(rawHost string) (bool, string, error) {
	c, ok, err := s.Get(rawHost)
	if err != nil || !ok {
		return ok, "", err
	}
	return true, c.Username, nil
}

// Reconcile 在启动时把文件与 registry_host_gen 对齐。
//
// 只有文件结构合法、每条记录的 AEAD 都能通过时才允许提交；任何校验失败
// 都直接返回错误，绝不在「可能损坏」的基础上更新 cred_digest。
func (s *Store) Reconcile() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return 0, err
	}
	defer unlock()

	doc, err := s.readValidated()
	if err != nil {
		return 0, err
	}
	states, err := s.db.HostCredStates()
	if err != nil {
		return 0, err
	}
	known := make(map[string]store.CredState, len(states))
	for _, st := range states {
		known[st.Host] = st
	}

	var changes []store.CredChange
	for host := range doc.Revoked {
		st, ok := known[host]
		if ok && st.Digest != "" && st.Digest != emptyDigest(host) {
			d := emptyDigest(host)
			changes = append(changes, store.CredChange{Host: host, Digest: &d})
		}
	}
	for host, rec := range doc.Hosts {
		digest := recordDigest(host, rec)
		if st, ok := known[host]; ok && st.Digest == digest {
			continue
		}
		d := digest
		changes = append(changes, store.CredChange{Host: host, Digest: &d})
	}
	// 文件里没有、数据库里却有摘要：只有「显式删除」（Revoked 或已提交的
	// 空摘要）可以接受，其余一律视为记录丢失。
	for host, st := range known {
		if _, ok := doc.Hosts[host]; ok {
			continue
		}
		if doc.Revoked[host] || st.Digest == "" || st.Digest == emptyDigest(host) {
			continue
		}
		return 0, fmt.Errorf("%w: %s", ErrRecordMissing, host)
	}
	if err := s.db.ApplyCredChanges(changes); err != nil {
		return 0, err
	}
	s.reconciled = true
	return len(changes), nil
}

// credentialHost 只接受规范化后的受支持 Host；空串（Docker Hub）与
// 非法字符串都不接受。
func credentialHost(raw string) (string, error) {
	host, err := registryhost.Check(raw)
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", errors.New("Docker Hub 不支持配置仓库凭据")
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

// readValidated 读取并完整校验文件：结构、Host 合法性、每条记录的 AEAD。
// 文件不存在时返回空文档（全新安装）。
func (s *Store) readValidated() (*credsDoc, error) {
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
	// 无论取哪个都可能误删或误用凭据，直接判文件损坏。
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
		// 同一 Host 不能既是有效记录又是删除标记：两种解释同时成立时，
		// 无论按哪个处理都可能误删或误用凭据。
		if _, ok := doc.Hosts[host]; ok {
			return nil, fmt.Errorf("%w: %s 同时存在密文记录与删除标记", ErrCorrupt, host)
		}
	}
	for host, rec := range doc.Hosts {
		norm, err := credentialHost(host)
		if err != nil || norm != host {
			return nil, fmt.Errorf("%w: Host %q 不合法", ErrCorrupt, host)
		}
		if rec.Nonce == "" || rec.CT == "" {
			return nil, fmt.Errorf("%w: %s 的记录不完整", ErrCorrupt, host)
		}
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

// writeField 写「十进制长度:内容」，是摘要编码里唯一的字段边界来源。
func writeField(b *bytes.Buffer, s string) {
	fmt.Fprintf(b, "%d:", len(s))
	b.WriteString(s)
}
