package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// 六态常量，见 design §1.2。
const (
	RecoveryInitial      = "initial"
	RecoveryOK           = "ok"
	RecoveryExpired      = "expired"
	RecoveryFailed       = "failed"
	RecoveryUnsupported  = "unsupported"
	RecoveryHardExcluded = "hard-excluded"
)

// RecoveryView 是 registry_host_recovery 的一行读模型。
// Exists=false 表示当前 (source,family,scope) 没有恢复行——调用方必须按
// 「无证据」处理，不能当成 initial 之外的任何状态。
type RecoveryView struct {
	SourceID      int64
	Family        string
	Scope         string
	Gen           int64
	State         string
	LastProbeAt   int64
	LastSuccessAt int64
	NextProbeAt   int64
	RecFailures   int
	LastError     string
	UpdatedAt     int64
	Exists        bool
}

// RecoveryState 按复合键读一行恢复状态。
//
// scope 统一走 registryhost.Check 规范化，并且查询里带 s.repo_host=scope：
// 调用方传了别的源/别的 Host 的 scope 时拿不到行，而不是读到一条不属于
// 这个源的恢复状态。recovery 的主键是 (source_id,family,scope)，不是 host。
func (s *Store) RecoveryState(sourceID int64, family, scope string) (RecoveryView, error) {
	norm := scope
	if family == "registry" {
		h, err := registryhost.Check(scope)
		if err != nil {
			return RecoveryView{}, err
		}
		norm = h
	}

	var v RecoveryView
	err := s.db.QueryRow(`
		SELECT r.source_id, r.family, r.scope, r.gen, r.state,
		       r.last_probe_at, r.last_success_at, r.next_probe_at,
		       r.rec_failures, r.last_error, r.updated_at
		FROM registry_host_recovery r
		JOIN sources s ON s.id = r.source_id
		WHERE r.source_id=? AND r.family=? AND r.scope=?
		  AND s.repo_host=?`, sourceID, family, norm, norm).
		Scan(&v.SourceID, &v.Family, &v.Scope, &v.Gen, &v.State,
			&v.LastProbeAt, &v.LastSuccessAt, &v.NextProbeAt,
			&v.RecFailures, &v.LastError, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RecoveryView{}, nil
	}
	if err != nil {
		return RecoveryView{}, err
	}
	v.Exists = true
	return v, nil
}

// DeriveInput 是六态派生的全部输入。时间戳只负责新鲜度，代次只做整数相等比较。
type DeriveInput struct {
	Cap int
	Now int64
	TTL time.Duration

	// CurrentGen 是 registry_host_gen 里该 Host 的代次；缺行为 0。
	CurrentGen int64

	HasRecovery   bool
	RecoveryGen   int64
	LastProbeAt   int64
	LastSuccessAt int64
	RecFailures   int
	LastError     string

	HasScoped   bool
	ScopedGen   int64
	ScopedFails int
}

// DeriveState 按 design §1.2 的优先级派生状态，第一个命中即返回。
//
//	1 cap=2          -> hard-excluded
//	2 cap=0          -> unsupported
//	3 当前代次有失败 -> failed（优先于过期/新鲜成功）
//	4 cap=1 且成功证据新鲜 -> ok
//	5 cap=1 且成功证据超时且无失败 -> expired
//	6 其余           -> initial
func DeriveState(in DeriveInput) string {
	if in.Cap == RegistryCapMismatch {
		return RecoveryHardExcluded
	}
	if in.Cap == RegistryCapNo {
		return RecoveryUnsupported
	}

	ttlSec := int64(in.TTL / time.Second)
	if ttlSec < 0 {
		ttlSec = 0
	}

	// 「当前代次的有效探测」：两边的 gen 都必须等于 currentGen，
	// 且 recovery 确实发生过探测。旧代次证据一律不参与派生。
	recCurrent := in.HasRecovery && in.RecoveryGen == in.CurrentGen && in.LastProbeAt > 0
	scopedCurrent := in.HasScoped && in.ScopedGen == in.CurrentGen

	hasFailure := recCurrent && scopedCurrent &&
		(in.ScopedFails >= 1 || in.LastError != "" || in.RecFailures >= 1)
	if hasFailure {
		return RecoveryFailed
	}

	if in.Cap == RegistryCapYes && recCurrent && scopedCurrent &&
		in.ScopedFails == 0 && in.LastSuccessAt > 0 &&
		in.Now-in.LastSuccessAt <= ttlSec {
		return RecoveryOK
	}

	// 边界：now-lastSuccess == TTL 仍算 ok（与 §5 的 >= now-TTL 闸门一致）。
	if in.Cap == RegistryCapYes && recCurrent && scopedCurrent &&
		in.LastSuccessAt > 0 && in.Now-in.LastSuccessAt > ttlSec {
		return RecoveryExpired
	}

	return RecoveryInitial
}
