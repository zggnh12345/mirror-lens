package secret

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// AuthMode 是凭据在 API/UI 层的枚举，避免调用方拼非法字段组合。
//
// 三种语义必须分开实现、分开验收（1.6 D-15-1）：
//   - basic：Registry 直接 Basic，TokenMethod/TokenAuth 不参与；
//   - bearer_token_basic：Bearer Challenge，Token 端点使用 Basic；
//   - bearer_token_none：Bearer Challenge，Token 端点不带 Basic。
type AuthMode string

const (
	ModeBasic            AuthMode = "basic"
	ModeBearerTokenBasic AuthMode = "bearer_token_basic"
	ModeBearerTokenNone  AuthMode = "bearer_token_none"
)

// Valid 报告模式是否是已知枚举值。
func (m AuthMode) Valid() bool {
	switch m {
	case ModeBasic, ModeBearerTokenBasic, ModeBearerTokenNone:
		return true
	}
	return false
}

// CredentialMode 返回凭据实际生效的模式。
// 历史记录没有 Mode 字段：有用户名密码按 bearer_token_basic（Token 端点
// 默认带 Basic，与 3.5.14 行为一致），空凭据只在显式 none 时合法。
func (c Credential) CredentialMode() AuthMode {
	if c.Mode != "" {
		return AuthMode(c.Mode)
	}
	if c.Username == "" && c.Password == "" {
		if strings.EqualFold(strings.TrimSpace(c.TokenAuth), "none") {
			return ModeBearerTokenNone
		}
		return ""
	}
	return ModeBearerTokenBasic
}

// ErrInvalidCredential 表示凭据字段组合不合法；API 必须翻译成 400。
var ErrInvalidCredential = errors.New("凭据字段不合法")

// normalize 校验并规范化凭据。写入与比较都走这一条路径，禁止两套规则。
func (c Credential) normalize() (Credential, error) {
	// 先查控制字符再 TrimSpace：否则 "u\n" 会被 trim 成合法用户名，
	// 掩盖调用方的注入意图。
	if strings.ContainsAny(c.Username, "\r\n\x00") || strings.ContainsAny(c.Password, "\r\n\x00") {
		return Credential{}, fmt.Errorf("%w: 用户名/密码不能包含控制字符", ErrInvalidCredential)
	}
	c.Username = strings.TrimSpace(c.Username)
	// 密码不做 TrimSpace：首尾空格可能是密码的一部分。
	c.TokenMethod = strings.ToUpper(strings.TrimSpace(c.TokenMethod))
	switch c.TokenMethod {
	case "", "GET":
	case "POST":
		return Credential{}, fmt.Errorf("%w: TokenMethod=POST 明确不支持", ErrInvalidCredential)
	default:
		return Credential{}, fmt.Errorf("%w: TokenMethod=%q", ErrInvalidCredential, c.TokenMethod)
	}
	c.TokenAuth = strings.ToLower(strings.TrimSpace(c.TokenAuth))
	switch c.TokenAuth {
	case "", "none", "basic":
	default:
		return Credential{}, fmt.Errorf("%w: TokenAuth=%q", ErrInvalidCredential, c.TokenAuth)
	}

	mode := c.CredentialMode()
	if !mode.Valid() {
		return Credential{}, fmt.Errorf("%w: 认证模式缺失或不合法", ErrInvalidCredential)
	}
	switch mode {
	case ModeBasic:
		if c.Username == "" || c.Password == "" {
			return Credential{}, fmt.Errorf("%w: basic 必须有非空用户名和密码", ErrInvalidCredential)
		}
		// 直接 Basic 不使用 Token 端点参数，写了也不生效，直接拒绝免得误解。
		if c.TokenMethod != "" || c.TokenAuth != "" {
			return Credential{}, fmt.Errorf("%w: basic 不接受 TokenMethod/TokenAuth", ErrInvalidCredential)
		}
	case ModeBearerTokenBasic:
		if c.Username == "" || c.Password == "" {
			return Credential{}, fmt.Errorf("%w: bearer_token_basic 必须有非空用户名和密码", ErrInvalidCredential)
		}
		if c.TokenAuth == "none" {
			return Credential{}, fmt.Errorf("%w: bearer_token_basic 与 TokenAuth=none 矛盾", ErrInvalidCredential)
		}
		c.TokenAuth = "basic"
	case ModeBearerTokenNone:
		if c.TokenAuth != "none" {
			return Credential{}, fmt.Errorf("%w: bearer_token_none 必须显式标记 TokenAuth=none", ErrInvalidCredential)
		}
		// 空凭据条目合法：有些 Token 端点只需要 service/scope。
	}
	c.Mode = string(mode)
	return c, nil
}

// same 逐字段比较两份已规范化凭据。
func (c Credential) same(o Credential) bool {
	return c.Mode == o.Mode &&
		c.Username == o.Username &&
		c.Password == o.Password &&
		c.TokenMethod == o.TokenMethod &&
		c.TokenAuth == o.TokenAuth
}

// State 是单个 Host 的凭据一致性状态（1.6 15-A.2）。
type State string

const (
	// StateAbsent：该 Host 没有凭据，数据库摘要为空。
	StateAbsent State = "Absent"
	// StateReady：文件记录与数据库 gen/cred_digest 一致。
	StateReady State = "Ready"
	// StatePending：文件已更新、数据库未提交（或删除只完成一半）。
	StatePending State = "Pending"
	// StateQuarantined：该 Host 记录损坏、AEAD 失败或摘要无法对齐。
	StateQuarantined State = "Quarantined"
	// StateError：写文件/磁盘/锁失败；旧 Ready 快照仍可用。
	StateError State = "Error"
)

// Status 是管理 API/UI 的一行 Host 状态。不含密码/Token/密文。
type Status struct {
	Host      string `json:"host"`
	State     State  `json:"state"`
	Reason    string `json:"reason,omitempty"`
	Gen       int64  `json:"gen"`
	Username  string `json:"username,omitempty"`
	Mode      string `json:"mode,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// PutResult 是一次写入/删除的结果。
type PutResult struct {
	Host    string
	Gen     int64
	Changed bool
	State   State
}

// WriteActor 是凭据写入审计的操作者信息。API 层传入当前管理员与客户端 IP；
// 兼容包装/内部调用可以省略，但生产管理 API 必须带上。
type WriteActor struct {
	User string
	IP   string
}

func credentialAudit(action, target, detail string, actors []WriteActor) *store.AuditEvent {
	ev := &store.AuditEvent{Action: action, Target: target, Detail: detail, OK: true}
	if len(actors) > 0 {
		ev.User = actors[0].User
		ev.IP = actors[0].IP
	}
	return ev
}

// PutCredential 是凭据写入的唯一实现。expectedGen 非 nil 时做乐观并发。
//
// 写入顺序（1.6 15-A.2 唯一允许的顺序）：
//  1. 校验 Host 与 Credential，读取现有记录并判断幂等；
//  2. Store.mu + flock 内写临时文件、fsync、rename、父目录 fsync；
//  3. 同一临界区内执行数据库事务（gen 递增、摘要、审计）；
//  4. 事务失败 → Pending，文件保留，后台按 Host 重试。
func (s *Store) PutCredential(rawHost string, c Credential, expectedGen *int64, actors ...WriteActor) (PutResult, error) {
	host, err := credentialWriteHost(rawHost)
	if err != nil {
		return PutResult{}, err
	}
	c, err = c.normalize()
	if err != nil {
		return PutResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return PutResult{Host: host, State: StateError}, err
	}
	defer unlock()

	doc, err := s.readDoc()
	if err != nil {
		return PutResult{Host: host, State: StateQuarantined}, err
	}
	// 乐观并发预检：冲突时不得先改文件。最终仍由 ApplyCredWrite 的 CAS 兜底
	// （跨进程写入可能落在预检之后，那时文件已换新、DB 拒绝 → Pending）。
	if expectedGen != nil {
		gen, _, gerr := s.db.HostGenState(host)
		if gerr != nil {
			return PutResult{Host: host, State: StateError}, gerr
		}
		if gen != *expectedGen {
			return PutResult{Host: host, Gen: gen, State: StateReady},
				fmt.Errorf("%w: %s 期望 %d，当前 %d", store.ErrCredGenConflict, host, *expectedGen, gen)
		}
	}
	// 幂等：现有记录能解密且逐字段相同 ⇒ 不写文件、不 bump、不失效 Token。
	if rec, ok := doc.Hosts[host]; ok {
		if cur, oerr := s.open(host, rec); oerr == nil {
			if cur.CredentialMode() == ModeBasic || cur.Mode != "" {
				cur.Mode = string(cur.CredentialMode())
			}
			if cur.same(c) {
				gen, _, gerr := s.db.HostGenState(host)
				if gerr != nil {
					return PutResult{Host: host, State: StateError}, gerr
				}
				if expectedGen != nil && *expectedGen != gen {
					return PutResult{Host: host, Gen: gen, State: StatePending},
						fmt.Errorf("%w: %s 期望 %d，当前 %d", store.ErrCredGenConflict, host, *expectedGen, gen)
				}
				return PutResult{Host: host, Gen: gen, Changed: false, State: StateReady}, nil
			}
		}
		// 解密失败：该 Host 已被隔离，不能用新写入覆盖证据？管理员显式
		// Put 是修复动作，允许覆盖该 Host 的损坏记录（只影响本 Host）。
	}

	rec, err := s.seal(host, c)
	if err != nil {
		return PutResult{Host: host, State: StateError}, err
	}
	doc.Hosts[host] = rec
	delete(doc.Revoked, host)
	if err := s.writeAtomic(doc); err != nil {
		// 临时文件已清理，原文件不变，数据库不变：保留旧 Ready 快照。
		return PutResult{Host: host, State: StateError}, err
	}
	digest := recordDigest(host, rec)
	res, err := s.db.ApplyCredWrite(store.CredWrite{
		Host:        host,
		Digest:      digest,
		ExpectedGen: expectedGen,
		Audit:       credentialAudit("credential_put", host, fmt.Sprintf("mode=%s gen_bump=1", c.Mode), actors),
	})
	if err != nil {
		// 文件已 durable、DB 未提交：Pending，后台 ReconcileHost 补写。
		return PutResult{Host: host, Gen: res.Gen, Changed: true, State: StatePending},
			fmt.Errorf("凭据已写入 %s，但数据库版本未提交（后台对账重试）：%w", s.path, err)
	}
	return PutResult{Host: host, Gen: res.Gen, Changed: res.Changed, State: StateReady}, nil
}

// Put 是 3.5.14 的兼容包装：内部走同一 PutCredential，不存在第二套写入实现。
func (s *Store) Put(rawHost, username, password string) (string, error) {
	_, err := s.PutCredential(rawHost, Credential{Username: username, Password: password}, nil)
	if err != nil {
		return "", err
	}
	snap, gerr := s.Snapshot(rawHost)
	if gerr != nil {
		return "", gerr
	}
	return snap.Digest, nil
}

// DeleteCredential 显式删除：文件移除记录并留 Revoked 标记，数据库写空摘要并 bump。
func (s *Store) DeleteCredential(rawHost string, expectedGen *int64, actors ...WriteActor) (PutResult, error) {
	host, err := credentialWriteHost(rawHost)
	if err != nil {
		return PutResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return PutResult{Host: host, State: StateError}, err
	}
	defer unlock()

	doc, err := s.readDoc()
	if err != nil {
		return PutResult{Host: host, State: StateQuarantined}, err
	}
	gen, cur, err := s.db.HostGenState(host)
	if err != nil {
		return PutResult{Host: host, State: StateError}, err
	}
	if expectedGen != nil && *expectedGen != gen {
		return PutResult{Host: host, Gen: gen, State: StatePending},
			fmt.Errorf("%w: %s 期望 %d，当前 %d", store.ErrCredGenConflict, host, *expectedGen, gen)
	}
	if _, ok := doc.Hosts[host]; !ok {
		// 文件里没有：数据库也必须已经是「无凭据」才算幂等成功。
		if cur == "" || cur == emptyDigest(host) {
			if cur == "" {
				// 从未配置：不 bump，保持 Absent。
				return PutResult{Host: host, Gen: gen, Changed: false, State: StateAbsent}, nil
			}
			return PutResult{Host: host, Gen: gen, Changed: false, State: StateAbsent}, nil
		}
		return PutResult{Host: host, Gen: gen, State: StateQuarantined},
			fmt.Errorf("%w: %s", ErrRecordMissing, host)
	}
	delete(doc.Hosts, host)
	if doc.Revoked == nil {
		doc.Revoked = map[string]bool{}
	}
	doc.Revoked[host] = true
	if err := s.writeAtomic(doc); err != nil {
		return PutResult{Host: host, State: StateError}, err
	}
	digest := emptyDigest(host)
	res, err := s.db.ApplyCredWrite(store.CredWrite{
		Host:        host,
		Digest:      digest,
		ExpectedGen: expectedGen,
		Audit:       credentialAudit("credential_delete", host, "gen_bump=1", actors),
	})
	if err != nil {
		return PutResult{Host: host, Gen: res.Gen, Changed: true, State: StatePending},
			fmt.Errorf("凭据已删除但数据库版本未提交（后台对账重试）：%w", err)
	}
	return PutResult{Host: host, Gen: res.Gen, Changed: true, State: StateAbsent}, nil
}

// Delete 是兼容包装。
func (s *Store) Delete(rawHost string) error {
	_, err := s.DeleteCredential(rawHost, nil)
	return err
}

// Status 返回单 Host 状态。它不返回密码，也不会因为别的 Host 损坏而失败。
func (s *Store) Status(rawHost string) (Status, error) {
	host, err := credentialStatusHost(rawHost)
	if err != nil {
		return Status{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked(host)
}

func (s *Store) statusLocked(host string) (Status, error) {
	gen, cur, err := s.db.HostGenState(host)
	if err != nil {
		return Status{Host: host, State: StateError, Reason: "db_error"}, err
	}
	st := Status{Host: host, Gen: gen}
	doc, derr := s.readDoc()
	if derr != nil {
		// 文件级损坏：所有 Host fail closed，状态可解释。
		st.State = StateQuarantined
		st.Reason = "file_corrupt"
		return st, nil
	}
	rec, ok := doc.Hosts[host]
	if !ok {
		switch {
		case doc.Revoked[host]:
			st.State = StateAbsent
		case cur == "" || cur == emptyDigest(host):
			st.State = StateAbsent
		default:
			// 数据库有摘要、文件里没有：R02，只隔离该 Host。
			st.State = StateQuarantined
			st.Reason = "record_missing"
		}
		return st, nil
	}
	c, oerr := s.open(host, rec)
	if oerr != nil {
		st.State = StateQuarantined
		st.Reason = "record_corrupt"
		return st, nil
	}
	st.Username = c.Username
	st.Mode = string(c.CredentialMode())
	if cur != recordDigest(host, rec) {
		st.State = StatePending
		st.Reason = "digest_mismatch"
		return st, nil
	}
	st.State = StateReady
	return st, nil
}

// ListStatus 返回所有已知 Host 的状态：凭据文件、数据库代次、可配凭据 Host 的并集。
func (s *Store) ListStatus() ([]Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var hosts []string
	add := func(h string) {
		if h == "" || seen[h] {
			return
		}
		if _, err := credentialStatusHost(h); err != nil {
			return
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	for _, h := range CredentialCapableHosts() {
		add(h)
	}
	if doc, err := s.readDoc(); err == nil {
		for h := range doc.Hosts {
			add(h)
		}
		for h := range doc.Revoked {
			add(h)
		}
	}
	states, err := s.db.HostCredStates()
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		add(st.Host)
	}
	sort.Strings(hosts)
	out := make([]Status, 0, len(hosts))
	for _, h := range hosts {
		st, _ := s.statusLocked(h)
		out = append(out, st)
	}
	return out, nil
}

// ReconcileHost 按 Host 对账：以文件为事实来源补写摘要并递增 gen。
// 单 Host 失败不影响其他 Host；返回该 Host 的新状态。
func (s *Store) ReconcileHost(rawHost string) (Status, error) {
	return s.reconcileHost(rawHost, true)
}

func (s *Store) reconcileHost(rawHost string, requireAllowed bool) (Status, error) {
	host, err := credentialHost(rawHost)
	if err != nil {
		return Status{}, err
	}
	if requireAllowed && !CredentialHostAllowed(host) {
		return Status{}, errors.New("该 Host 不在 3.5.15 私有凭据 allowlist 内")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return Status{Host: host, State: StateError, Reason: "lock_failed"}, err
	}
	defer unlock()

	doc, err := s.readDoc()
	if err != nil {
		return Status{Host: host, State: StateQuarantined, Reason: "file_corrupt"}, err
	}
	gen, cur, err := s.db.HostGenState(host)
	if err != nil {
		return Status{Host: host, State: StateError, Reason: "db_error"}, err
	}
	rec, ok := doc.Hosts[host]
	if ok {
		if _, oerr := s.open(host, rec); oerr != nil {
			return Status{Host: host, Gen: gen, State: StateQuarantined, Reason: "record_corrupt"}, nil
		}
		digest := recordDigest(host, rec)
		if cur == digest {
			s.reconciled = true
			return Status{Host: host, Gen: gen, State: StateReady}, nil
		}
		res, werr := s.db.ApplyCredWrite(store.CredWrite{
			Host: host, Digest: digest,
			Audit: &store.AuditEvent{Action: "credential_reconcile", Target: host,
				Detail: "adopt_file", OK: true},
		})
		if werr != nil {
			return Status{Host: host, Gen: gen, State: StatePending, Reason: "db_error"}, werr
		}
		return Status{Host: host, Gen: res.Gen, State: StateReady}, nil
	}
	// 文件里没有这条记录。
	if cur == "" || cur == emptyDigest(host) || doc.Revoked[host] {
		if cur != "" && cur != emptyDigest(host) && doc.Revoked[host] {
			// 删除只写了文件：补提交空摘要。
			res, werr := s.db.ApplyCredWrite(store.CredWrite{
				Host: host, Digest: emptyDigest(host),
				Audit: &store.AuditEvent{Action: "credential_reconcile", Target: host,
					Detail: "adopt_delete", OK: true},
			})
			if werr != nil {
				return Status{Host: host, Gen: gen, State: StatePending}, werr
			}
			return Status{Host: host, Gen: res.Gen, State: StateAbsent}, nil
		}
		return Status{Host: host, Gen: gen, State: StateAbsent}, nil
	}
	return Status{Host: host, Gen: gen, State: StateQuarantined, Reason: "record_missing"},
		fmt.Errorf("%w: %s", ErrRecordMissing, host)
}

// ReconcileAll 按 Host 对账并返回：变更条数、被隔离的 Host、致命错误。
//
// 文件级损坏是致命错误（全局 fail closed）；单条记录损坏只把该 Host 加入
// quarantined，其余 Host 继续 Ready——启动流程不得因为一个 Host 失败把
// 整个 hostCreds 置 nil。
func (s *Store) ReconcileAll() (int, []Status, error) {
	before, err := s.db.HostCredStates()
	if err != nil {
		return 0, nil, err
	}
	prev := make(map[string]string, len(before))
	hostSet := map[string]bool{}
	for _, st := range before {
		prev[st.Host] = st.Digest
		hostSet[st.Host] = true
	}
	s.mu.Lock()
	doc, derr := s.readDoc()
	s.mu.Unlock()
	if derr != nil {
		return 0, nil, derr
	}
	for h := range doc.Hosts {
		hostSet[h] = true
	}
	for h := range doc.Revoked {
		hostSet[h] = true
	}
	hosts := make([]string, 0, len(hostSet))
	for h := range hostSet {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	var quarantined []Status
	for _, h := range hosts {
		if !CredentialHostAllowed(h) {
			// 历史文件/DB 里可能有非 allowlist Host；读路径能识别，但
			// 不把它们纳入 3.5.15 的对账范围。
			continue
		}
		st, err := s.reconcileHost(h, false)
		if st.State == StateQuarantined {
			quarantined = append(quarantined, st)
			continue
		}
		if err != nil {
			return 0, quarantined, err
		}
	}
	after, err := s.db.HostCredStates()
	if err != nil {
		return 0, quarantined, err
	}
	changes := 0
	now := make(map[string]string, len(after))
	for _, st := range after {
		now[st.Host] = st.Digest
	}
	for h, d := range now {
		if prev[h] != d {
			changes++
		}
	}
	for h := range prev {
		if _, ok := now[h]; !ok {
			changes++
		}
	}
	s.markReconciled()
	return changes, quarantined, nil
}

// markReconciled 由 Reconcile/ReconcileAll 在成功路径调用。
func (s *Store) markReconciled() {
	s.mu.Lock()
	s.reconciled = true
	s.mu.Unlock()
}

// nowUnix 便于测试注入时间（当前只用于状态展示）。
func nowUnix() int64 { return time.Now().Unix() }
