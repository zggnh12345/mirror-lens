package store

import (
	"context"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

func seedExpireExec(t *testing.T, st *Store, q string, args ...any) {
	t.Helper()
	if _, err := st.db.Exec(q, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// seedExpiredOK 造一条「cap=1、recovery=ok、代次对齐、scoped 无失败」的行，
// 成功时间由调用方指定。生产上这行由 CommitHostProbe 写入，last_probe_at>0。
func seedExpiredOK(t *testing.T, st *Store, host string, lastSuccess, gen int64) int64 {
	t.Helper()
	id := addRegSource(t, st, host, "native", 1, true, "usable", 0)
	seedExpireExec(t, st, `UPDATE registry_host_recovery
		SET gen=?, last_probe_at=?, last_success_at=?, next_probe_at=?, state='ok',
		    rec_failures=0, last_error='', updated_at=?
		WHERE source_id=? AND family='registry' AND scope=?`,
		gen, lastSuccess, lastSuccess, lastSuccess, lastSuccess, id, host)
	seedExpireExec(t, st, `INSERT INTO source_health_scope(source_id, family, scope, gen, fails, ok_at, last_at)
		VALUES(?, 'registry', ?, ?, 0, ?, ?)
		ON CONFLICT(source_id, family, scope, gen) DO UPDATE SET
		  fails=0, ok_at=excluded.ok_at, last_at=excluded.last_at`,
		id, host, gen, lastSuccess, lastSuccess)
	return id
}

func recoveryRow(t *testing.T, st *Store, id int64) (state string, gen, lastSuccess, nextProbe int64) {
	t.Helper()
	if err := st.db.QueryRow(`SELECT state, gen, last_success_at, next_probe_at
		FROM registry_host_recovery WHERE source_id=? AND family='registry'`,
		id).Scan(&state, &gen, &lastSuccess, &nextProbe); err != nil {
		t.Fatalf("read recovery: %v", err)
	}
	return
}

// 测试 #1：恰好到 TTL 边界仍是 ok，不物化。
func TestMaterializeExpiredTTLBoundary(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)

	id := seedExpiredOK(t, st, "quay.io", base.Unix()-int64((24*time.Hour)/time.Second), 0)

	n, err := st.MaterializeExpired(base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("恰好等于 TTL 边界不应物化，推进了 %d 行", n)
	}
	if state, _, _, _ := recoveryRow(t, st, id); state != "ok" {
		t.Fatalf("边界上状态应保持 ok，实际 %s", state)
	}
}

// 测试 #2：超过 TTL 一秒 → ok → expired，且 next_probe_at 拉到 now。
func TestMaterializeExpiredBeyondTTL(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)

	id := seedExpiredOK(t, st, "quay.io", base.Unix()-int64(24*time.Hour/time.Second)-1, 0)

	now := base.Add(time.Minute)
	n, err := st.MaterializeExpired(now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应物化 1 行，实际 %d", n)
	}
	state, _, _, next := recoveryRow(t, st, id)
	if state != "expired" {
		t.Fatalf("state 应为 expired，实际 %s", state)
	}
	if next != now.Unix() {
		t.Fatalf("next_probe_at 应为 %d，实际 %d", now.Unix(), next)
	}
}

// 测试 #3：重复执行不再产生变化。
func TestMaterializeExpiredIdempotent(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)

	seedExpiredOK(t, st, "quay.io", base.Unix()-int64(24*time.Hour/time.Second)-1, 0)

	if n, err := st.MaterializeExpired(base); err != nil || n != 1 {
		t.Fatalf("第一次应物化 1 行，n=%d err=%v", n, err)
	}
	if n, err := st.MaterializeExpired(base); err != nil || n != 0 {
		t.Fatalf("第二次应推进 0 行，n=%d err=%v", n, err)
	}
}

// 测试 #4（先行状态变化保护）：先有新的成功提交，再物化，旧事实不得覆盖新 ok。
// 这不是事务内竞态复现——BEGIN IMMEDIATE 已排除该窗口。
func TestMaterializeExpiredAfterFreshSuccess(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)

	id := seedExpiredOK(t, st, "quay.io", base.Unix()-int64(24*time.Hour/time.Second)-1, 0)
	fresh := base.Add(2 * time.Minute)
	if res, err := st.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Gen: 0, CredDigest: "",
		Cap: RegistryCapYes, PathMode: PathNative, At: fresh.Unix(), TTL: 24 * time.Hour,
	}); err != nil || res != HostCapsCommitted {
		t.Fatalf("fresh commit: res=%v err=%v", res, err)
	}

	n, err := st.MaterializeExpired(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("新成功记录不应被物化，推进了 %d 行", n)
	}
	state, _, lastSuccess, _ := recoveryRow(t, st, id)
	if state != "ok" || lastSuccess != fresh.Unix() {
		t.Fatalf("新成功事实被覆盖：state=%s last_success=%d", state, lastSuccess)
	}
}

// 测试 #5（先行状态变化保护）：代次推进后，旧代次结论不得被物化为当前代次 expired。
func TestMaterializeExpiredAfterGenBump(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)

	id := seedExpiredOK(t, st, "quay.io", base.Unix()-int64(24*time.Hour/time.Second)-1, 0)
	newGen, err := st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if newGen != 1 {
		t.Fatalf("代次应推进到 1，实际 %d", newGen)
	}
	// BumpHostGen 会把 cap 清成 Unknown；模拟重新具备 cap=1 的状态，
	// 确保候选 SQL 不会因为 cap 而提前过滤掉，真正走到代次判断。
	seedExpireExec(t, st, `UPDATE sources SET registry_cap=1, registry_path_mode='native' WHERE id=?`, id)

	n, err := st.MaterializeExpired(base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("旧代次行不应被物化，推进了 %d 行", n)
	}
	state, gen, _, _ := recoveryRow(t, st, id)
	if gen != newGen {
		t.Fatalf("recovery.gen 应跟随到 %d，实际 %d", newGen, gen)
	}
	if state != "initial" {
		t.Fatalf("代次推进后应为 initial（待重验），实际 %s", state)
	}
}

// 测试 #6（CAS 结构性防御验证）：state / last_success_at / gen 任一不匹配都不得更新。
// 这是条件正确性验证，不是跨连接并发竞争测试。
func TestMaterializeExpiredCASConditions(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	id := seedExpiredOK(t, st, "quay.io", base.Unix()-1000, 0)

	ctx := context.Background()
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, "ROLLBACK")

	c := expireCand{SourceID: id, Scope: "quay.io", Gen: 0, LastSuccessAt: base.Unix() - 1000}

	reset := func() {
		if _, err := conn.ExecContext(ctx,
			`UPDATE registry_host_recovery SET state='ok', gen=0, last_success_at=? WHERE source_id=?`,
			c.LastSuccessAt, id); err != nil {
			t.Fatal(err)
		}
	}

	reset()
	if n, err := updateExpiredCAS(ctx, conn, c, base); err != nil || n != 1 {
		t.Fatalf("三条件全匹配应更新 1 行，n=%d err=%v", n, err)
	}

	reset()
	if _, err := conn.ExecContext(ctx, `UPDATE registry_host_recovery SET state='failed' WHERE source_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := updateExpiredCAS(ctx, conn, c, base); err != nil || n != 0 {
		t.Fatalf("state 不匹配不得更新，n=%d err=%v", n, err)
	}

	reset()
	if _, err := conn.ExecContext(ctx, `UPDATE registry_host_recovery SET last_success_at=? WHERE source_id=?`, c.LastSuccessAt+1, id); err != nil {
		t.Fatal(err)
	}
	if n, err := updateExpiredCAS(ctx, conn, c, base); err != nil || n != 0 {
		t.Fatalf("last_success_at 不匹配不得更新，n=%d err=%v", n, err)
	}

	reset()
	if _, err := conn.ExecContext(ctx, `UPDATE registry_host_recovery SET gen=1 WHERE source_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := updateExpiredCAS(ctx, conn, c, base); err != nil || n != 0 {
		t.Fatalf("gen 不匹配不得更新，n=%d err=%v", n, err)
	}
}

// 物化只处理派生结果为 expired 的行：存在当前代次失败证据时应跳过，
// 留给探测路径按 failed 处理。
func TestMaterializeExpiredSkipsFailedDerivation(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)

	id := seedExpiredOK(t, st, "quay.io", base.Unix()-int64(24*time.Hour/time.Second)-1, 0)
	seedExpireExec(t, st, `UPDATE source_health_scope SET fails=1 WHERE source_id=? AND gen=0`, id)

	n, err := st.MaterializeExpired(base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("存在失败证据时不应物化为 expired，推进了 %d 行", n)
	}
	if state, _, _, _ := recoveryRow(t, st, id); state != "ok" {
		t.Fatalf("物化任务不应把行改成 %s", state)
	}
}

// cap=1 之外的源、归档源都不在物化范围。
func TestMaterializeExpiredSkipsNonCandidates(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)
	old := base.Unix() - int64(24*time.Hour/time.Second) - 1

	hard := seedExpiredOK(t, st, "hard.example", old, 0)
	seedExpireExec(t, st, `UPDATE sources SET registry_cap=2 WHERE id=?`, hard)

	arch := seedExpiredOK(t, st, "arch.example", old, 0)
	seedExpireExec(t, st, `UPDATE sources SET status='archived' WHERE id=?`, arch)

	n, err := st.MaterializeExpired(base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cap!=1 或 archived 的源不应物化，推进了 %d 行", n)
	}
}

// 指标：物化行数只统计 CAS 提交成功；零行与执行失败在 runs/failures 上可区分。
func TestMaterializeExpiredMetrics(t *testing.T) {
	st, _ := newTestStore(t)
	base := time.Unix(1_700_000_000, 0)
	st.SetClock(func() time.Time { return base })
	st.SetRecoveryTTL(24 * time.Hour)

	before := metrics.RecoveryCountersSnapshot()
	// 零行：没有过期记录，runs+1、rows 不动、failures 不动。
	if n, err := st.MaterializeExpired(base); err != nil || n != 0 {
		t.Fatalf("空跑应 0 行，n=%d err=%v", n, err)
	}
	mid := metrics.RecoveryCountersSnapshot()
	if mid.Runs != before.Runs+1 || mid.MaterializedRows != before.MaterializedRows || mid.Failures != before.Failures {
		t.Fatalf("零行语义错误: before=%+v mid=%+v", before, mid)
	}

	seedExpiredOK(t, st, "quay.io", base.Unix()-int64(24*time.Hour/time.Second)-1, 0)
	if n, err := st.MaterializeExpired(base); err != nil || n != 1 {
		t.Fatalf("应物化 1 行，n=%d err=%v", n, err)
	}
	after := metrics.RecoveryCountersSnapshot()
	if after.MaterializedRows != before.MaterializedRows+1 {
		t.Fatalf("物化行数增量错误: %+v", after)
	}
	if after.Runs != before.Runs+2 || after.Failures != before.Failures {
		t.Fatalf("runs/failures 增量错误: %+v", after)
	}

	// 执行失败：failures+1，rows 不动。
	st.Close()
	if _, err := st.MaterializeExpired(base); err == nil {
		t.Fatal("关闭后应返回错误")
	}
	failed := metrics.RecoveryCountersSnapshot()
	if failed.Failures != after.Failures+1 || failed.MaterializedRows != after.MaterializedRows {
		t.Fatalf("失败计数错误: %+v", failed)
	}
	if failed.Runs != after.Runs+1 {
		t.Fatalf("失败也应计入 runs: %+v", failed)
	}
}
