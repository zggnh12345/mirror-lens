package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// probeReasonTransportError 与 probe.ReasonTransportError 取值一致。
// store 不 import probe（探测层不能依赖数据库），用测试锁死两边一致。
const probeReasonTransportError = "transport_error"

const (
	// recoveryUnsupportedInterval 是 cap=0 的长周期重验间隔。
	recoveryUnsupportedInterval = 6 * time.Hour
	// RecoveryDefaultTTL 是成功结论的有效期默认值；A2 会接到配置上。
	RecoveryDefaultTTL = 24 * time.Hour

	recoveryBackoffBase = 15 * time.Second
	recoveryBackoffMax  = 10 * time.Minute
)

// recoveryBackoff 与 runner 的 maxHostProbeBackoff 保持同一档位：
// 15s 起步、按失败次数翻倍、10m 封顶。
func recoveryBackoff(failures int) time.Duration {
	d := recoveryBackoffBase
	for i := 0; i < failures; i++ {
		d *= 2
		if d >= recoveryBackoffMax {
			return recoveryBackoffMax
		}
	}
	return d
}

// HostProbeCommit 是一次 Host 探测结果的完整写回输入。
// 字段与 design §2.3 的 CommitHostProbe 参数一一对应；Gen/CredDigest/
// HasCredential 必须来自探测起点的 Creds.Snapshot，事务内不得重新推导。
type HostProbeCommit struct {
	SourceID int64
	Host     string
	Cap      int
	PathMode string
	Reason   string
	// Transport 是 transport_error 的细分（dns/timeout/...），只用于诊断。
	Transport string
	At        int64
	ErrText   string

	Gen           int64
	CredDigest    string
	HasCredential bool

	// TTL 是成功结论的有效期；<=0 时用 RecoveryDefaultTTL。
	TTL time.Duration
}

// CommitHostProbe 是 Registry 探测结果写回的唯一入口。
//
// 一个 BEGIN IMMEDIATE 事务内完成：源归属 / 归档 / 代次 / 摘要校验，
// 写 registry_cap+path_mode（瞬时传输错误跳过）、按探测起点 gen 写
// source_health_scope、派生并 UPSERT registry_host_recovery。
// 任一校验不符或任一语句失败即整体回滚，返回对应结果码且不留下任何
// 持久化副作用。
func (s *Store) CommitHostProbe(p HostProbeCommit) (HostCapsCommitResult, error) {
	host, err := registryhost.Check(p.Host)
	if err != nil {
		return HostCapsCommitted, err
	}
	if host == "" || host == "ghcr.io" {
		return HostCapsCommitted, errors.New("Hub/ghcr 的能力仍由 docker_cap/ghcr_cap 记录，不走 CommitHostProbe")
	}
	if !validRegistryCap(p.Cap) {
		return HostCapsCommitted, errors.New("registry_cap 只允许 -1/0/1/2")
	}
	if p.Cap == RegistryCapYes {
		if p.PathMode != PathNative && p.PathMode != PathHostPrefix {
			return HostCapsCommitted, errors.New("registry_cap=1 必须指定 native/host_prefix")
		}
	} else if p.PathMode != "" {
		return HostCapsCommitted, errors.New("只有 registry_cap=1 可以带 registry_path_mode")
	}
	ttl := p.TTL
	if ttl <= 0 {
		ttl = RecoveryDefaultTTL
	}

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return HostCapsCommitted, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return HostCapsCommitted, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	res, err := s.commitHostProbeTx(ctx, conn, host, p, ttl)
	if err != nil {
		return res, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return HostCapsCommitted, err
	}
	committed = true
	return res, nil
}

func (s *Store) commitHostProbeTx(ctx context.Context, conn *sql.Conn, host string, p HostProbeCommit, ttl time.Duration) (HostCapsCommitResult, error) {
	// 1. 源存在 / 未归档 / 归属仍是探测时的 Host。三项都在事务内读，
	//    与后续写入同处一个写锁区间。
	var repoHost, status string
	var capNow int
	err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(repo_host,''), status, registry_cap FROM sources WHERE id=?`, p.SourceID).
		Scan(&repoHost, &status, &capNow)
	if errors.Is(err, sql.ErrNoRows) {
		return HostCapsSourceUnavailable, nil
	}
	if err != nil {
		return HostCapsCommitted, err
	}
	if status == "archived" {
		return HostCapsSourceUnavailable, nil
	}
	if repoHost != host {
		return HostCapsSourceChanged, nil
	}

	// 2. 代次三态：缺行不得用 COALESCE 折算。
	var curGen int64
	var curDigest string
	err = conn.QueryRowContext(ctx,
		`SELECT gen, COALESCE(cred_digest,'') FROM registry_host_gen WHERE host=?`, host).
		Scan(&curGen, &curDigest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if p.HasCredential || p.Gen != 0 {
			// 有凭据却没有代次行 = 未对账；无凭据也只在 gen=0 时放行。
			return HostCapsStaleSnapshot, nil
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT OR IGNORE INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES(?,0,'',?)`,
			host, p.At); err != nil {
			return HostCapsCommitted, err
		}
	case err != nil:
		return HostCapsCommitted, err
	default:
		if curGen != p.Gen || curDigest != p.CredDigest {
			return HostCapsStaleSnapshot, nil
		}
	}

	// 3. registry_cap / registry_path_mode。
	//    瞬时传输错误只跳过 cap 写回，不跳过 scoped 与 recovery。
	writeCap := true
	effectiveCap := p.Cap
	if p.Cap == RegistryCapUnknown && p.Reason == probeReasonTransportError {
		writeCap = false
		effectiveCap = capNow
	}
	if writeCap {
		mode := p.PathMode
		if effectiveCap != RegistryCapYes {
			mode = ""
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE sources SET registry_cap=?, registry_path_mode=?, registry_probe_at=?, registry_probe_error=? WHERE id=?`,
			effectiveCap, mode, p.At, clipProbeNote(p.ErrText), p.SourceID); err != nil {
			return HostCapsCommitted, err
		}
	}

	// 4. source_health_scope：SQL 与既有 NoteProbeScoped 逐字等价，但
	//    目标行固定 (source_id,'registry',host,gen=p.Gen)，gen 不在 SQL 里推导。
	scopedWritten := p.Cap == RegistryCapYes || p.Cap == RegistryCapUnknown
	if scopedWritten {
		if p.Cap == RegistryCapYes {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO source_health_scope(source_id, family, scope, gen, fails, ok_at, last_at)
				 VALUES(?,?,?,?,0,?,?)
				 ON CONFLICT(source_id, family, scope, gen)
				 DO UPDATE SET fails=0, ok_at=excluded.ok_at, last_at=excluded.last_at`,
				p.SourceID, "registry", host, p.Gen, p.At, p.At); err != nil {
				return HostCapsCommitted, err
			}
		} else {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO source_health_scope(source_id, family, scope, gen, fails, last_at)
				 VALUES(?,?,?,?,1,?)
				 ON CONFLICT(source_id, family, scope, gen)
				 DO UPDATE SET fails=fails+1, last_at=excluded.last_at`,
				p.SourceID, "registry", host, p.Gen, p.At); err != nil {
				return HostCapsCommitted, err
			}
		}
	}

	// 5. 读回本代次的 scoped 证据（派生必须基于写入之后的值）。
	var scopedFails int
	var scopedOkAt, scopedLastAt int64
	hasScoped := true
	err = conn.QueryRowContext(ctx,
		`SELECT fails, ok_at, last_at FROM source_health_scope
		 WHERE source_id=? AND family='registry' AND scope=? AND gen=?`,
		p.SourceID, host, p.Gen).Scan(&scopedFails, &scopedOkAt, &scopedLastAt)
	if errors.Is(err, sql.ErrNoRows) {
		hasScoped = false
		scopedFails, scopedOkAt, scopedLastAt = 0, 0, 0
	} else if err != nil {
		return HostCapsCommitted, err
	}

	// 6. 读旧 recovery 行（可能不存在），在其上叠加本次结果。
	var oldState string
	var oldLastProbe, oldLastSuccess, oldNext int64
	var oldFailures int
	var oldErr string
	hasRecovery := true
	err = conn.QueryRowContext(ctx,
		`SELECT state, last_probe_at, last_success_at, next_probe_at, rec_failures, last_error
		 FROM registry_host_recovery WHERE source_id=? AND family='registry' AND scope=?`,
		p.SourceID, host).
		Scan(&oldState, &oldLastProbe, &oldLastSuccess, &oldNext, &oldFailures, &oldErr)
	if errors.Is(err, sql.ErrNoRows) {
		hasRecovery = false
		oldState, oldLastProbe, oldLastSuccess, oldNext, oldFailures, oldErr = "", 0, 0, 0, 0, ""
	} else if err != nil {
		return HostCapsCommitted, err
	}
	_ = hasRecovery

	lastSuccess := oldLastSuccess
	failures := oldFailures
	lastError := oldErr
	switch p.Cap {
	case RegistryCapYes:
		lastSuccess = p.At
		failures = 0
		lastError = ""
	case RegistryCapUnknown:
		failures++
		lastError = clipProbeNote(p.ErrText)
	}

	state := DeriveState(DeriveInput{
		Cap:           effectiveCap,
		Now:           p.At,
		TTL:           ttl,
		CurrentGen:    p.Gen,
		HasRecovery:   true,
		RecoveryGen:   p.Gen,
		LastProbeAt:   p.At,
		LastSuccessAt: lastSuccess,
		RecFailures:   failures,
		LastError:     lastError,
		HasScoped:     hasScoped,
		ScopedGen:     p.Gen,
		ScopedFails:   scopedFails,
	})

	var next int64
	switch state {
	case RecoveryOK:
		next = p.At + int64(ttl/time.Second)
	case RecoveryFailed:
		next = p.At + int64(recoveryBackoff(failures)/time.Second)
	case RecoveryUnsupported:
		next = p.At + int64(recoveryUnsupportedInterval/time.Second)
	case RecoveryHardExcluded:
		next = 0 // 不进普通恢复循环，只能由凭据/归属变更触发重验
	default:
		// initial：本次没有形成可判定的当前代次证据。不能把
		// next_probe_at 留在 now，否则 30s 扫描会形成探测循环。
		f := failures
		if f < 1 {
			f = 1
		}
		next = p.At + int64(recoveryBackoff(f)/time.Second)
	}

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO registry_host_recovery
		 (source_id, family, scope, state, gen, last_probe_at, last_success_at,
		  next_probe_at, rec_failures, last_error, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(source_id, family, scope) DO UPDATE SET
		   state=excluded.state, gen=excluded.gen, last_probe_at=excluded.last_probe_at,
		   last_success_at=excluded.last_success_at, next_probe_at=excluded.next_probe_at,
		   rec_failures=excluded.rec_failures, last_error=excluded.last_error,
		   updated_at=excluded.updated_at`,
		p.SourceID, "registry", host, state, p.Gen, p.At, lastSuccess,
		next, failures, lastError, p.At); err != nil {
		return HostCapsCommitted, err
	}

	return HostCapsCommitted, nil
}
