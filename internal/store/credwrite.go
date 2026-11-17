package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// ErrCredGenConflict 表示调用方给的 expected_gen 与当前代次不一致。
// 管理 API 必须把它翻译成 409，不能后写覆盖先写。
var ErrCredGenConflict = errors.New("凭据代次冲突")

// credExec 是事务内执行器：*sql.Tx 与 *sql.Conn 都满足。
type credExec interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// CredWrite 是一次凭据版本提交。
//
// 与 ApplyCredChanges 的区别：
//   - ExpectedGen 做乐观并发（nil 表示不校验）；
//   - Audit 在同一事务内同步写入，不走「队列满即丢」的异步旁路；
//   - Digest 与现值相同时 Changed=false，不递增代次、不写审计。
type CredWrite struct {
	Host        string
	Digest      string
	ExpectedGen *int64
	Audit       *AuditEvent
}

// CredWriteResult 是一次提交的结果。
type CredWriteResult struct {
	Host    string
	Gen     int64
	Changed bool
}

// ApplyCredWrite 在单个 BEGIN IMMEDIATE 事务内完成：CAS 校验、代次递增、
// 摘要写入、能力结论清理、审计落库。任一失败整体回滚。
func (s *Store) ApplyCredWrite(w CredWrite) (CredWriteResult, error) {
	host, err := credentialGenHost(w.Host)
	if err != nil {
		return CredWriteResult{}, err
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return CredWriteResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return CredWriteResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	res, err := s.applyCredWriteConn(ctx, conn, host, w)
	if err != nil {
		return CredWriteResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return CredWriteResult{}, err
	}
	committed = true
	return res, nil
}

func (s *Store) applyCredWriteConn(ctx context.Context, ex credExec, host string, w CredWrite) (CredWriteResult, error) {
	var curGen int64
	var curDigest string
	err := ex.QueryRowContext(ctx,
		`SELECT gen, COALESCE(cred_digest,'') FROM registry_host_gen WHERE host=?`, host).
		Scan(&curGen, &curDigest)
	if errors.Is(err, sql.ErrNoRows) {
		curGen, curDigest = 0, ""
	} else if err != nil {
		return CredWriteResult{}, err
	}
	if w.ExpectedGen != nil && *w.ExpectedGen != curGen {
		return CredWriteResult{Host: host, Gen: curGen}, fmt.Errorf("%w: %s 期望 %d，当前 %d",
			ErrCredGenConflict, host, *w.ExpectedGen, curGen)
	}
	if w.Digest == curDigest {
		// 幂等：内容没变，不写文件、不递增、不失效 Token、不写审计。
		return CredWriteResult{Host: host, Gen: curGen, Changed: false}, nil
	}
	digest := w.Digest
	gen, err := applyCredChangeSQL(ctx, ex, host, &digest, time.Now().Unix())
	if err != nil {
		return CredWriteResult{}, err
	}
	if w.Audit != nil {
		e := *w.Audit
		if e.TS == 0 {
			e.TS = time.Now().Unix()
		}
		if e.Target == "" {
			e.Target = host
		}
		ok := 0
		if e.OK {
			ok = 1
		}
		if _, err := ex.ExecContext(ctx,
			`INSERT INTO audit(ts,ip,user,action,target,detail,ok) VALUES(?,?,?,?,?,?,?)`,
			e.TS, e.IP, e.User, e.Action, e.Target, e.Detail, ok); err != nil {
			return CredWriteResult{}, fmt.Errorf("写凭据审计: %w", err)
		}
	}
	return CredWriteResult{Host: host, Gen: gen, Changed: true}, nil
}

// applyCredChangeSQL 是「代次 +1、写摘要、清能力结论、重置 recovery」的唯一实现。
// ApplyCredChanges（旧调用方）与 ApplyCredWrite（CAS + 审计）共用它，避免两套语义。
func applyCredChangeSQL(ctx context.Context, ex credExec, host string, digest *string, now int64) (int64, error) {
	var newGen int64
	if digest == nil {
		if err := ex.QueryRowContext(ctx, `INSERT INTO registry_host_gen(host,gen,updated_at) VALUES(?,1,?)
ON CONFLICT(host) DO UPDATE SET gen=gen+1, updated_at=excluded.updated_at
RETURNING gen`, host, now).Scan(&newGen); err != nil {
			return 0, err
		}
	} else if err := ex.QueryRowContext(ctx, `INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES(?,1,?,?)
ON CONFLICT(host) DO UPDATE SET gen=gen+1, cred_digest=excluded.cred_digest, updated_at=excluded.updated_at
RETURNING gen`,
		host, *digest, now).Scan(&newGen); err != nil {
		return 0, err
	}
	// 只清结论，不抹诊断：诊断是上一次有效 Probe 留下的历史事实，
	// 凭据变了只意味着它不能再当结论用，不意味着它没发生过。
	if _, err := ex.ExecContext(ctx, `UPDATE source_host_caps
SET effective_status=?, registry_path_mode=''
WHERE host=? AND EXISTS (SELECT 1 FROM sources s WHERE s.id=source_host_caps.source_id AND s.status<>'archived')`,
		HostStatusUnknown, host); err != nil {
		return 0, err
	}
	// 代次绑定：recovery 行在同一事务内切到新 gen 并重置为待重验。
	if _, err := ex.ExecContext(ctx, `INSERT INTO registry_host_recovery
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
		return 0, err
	}
	return newGen, nil
}

// credentialGenHost 只接受 Host 级代次的合法 Host；Hub/GHCR 由协议槽位管理。
func credentialGenHost(raw string) (string, error) {
	host, err := registryhost.Check(raw)
	if err != nil {
		return "", err
	}
	if host == "" || host == "docker.io" || host == "ghcr.io" {
		return "", errors.New("Docker Hub/GHCR 不使用 Host 代次")
	}
	return host, nil
}
