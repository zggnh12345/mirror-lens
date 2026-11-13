package store

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func setGen(t *testing.T, s *Store, host string, gen int64, digest string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES(?,?,?,1)
ON CONFLICT(host) DO UPDATE SET gen=excluded.gen, cred_digest=excluded.cred_digest`, host, gen, digest); err != nil {
		t.Fatalf("set gen: %v", err)
	}
}

// registrySnapshot 把一次探测可能触及（以及候选准入依赖）的全部行拍成字符串，
// 用于断言「整笔拒绝之后状态逐字段不变」。
func registrySnapshot(t *testing.T, s *Store, id int64, host string) string {
	t.Helper()
	var b strings.Builder
	var repoHost, status, mode string
	var capv, probeAt int64
	var probeErr string
	if err := s.db.QueryRow(`SELECT COALESCE(repo_host,''),status,registry_cap,registry_path_mode,
		registry_probe_at,COALESCE(registry_probe_error,'') FROM sources WHERE id=?`, id).
		Scan(&repoHost, &status, &capv, &mode, &probeAt, &probeErr); err != nil {
		t.Fatalf("snapshot source: %v", err)
	}
	fmt.Fprintf(&b, "src|%s|%s|%d|%s|%d|%s\n", repoHost, status, capv, mode, probeAt, probeErr)

	rows, err := s.db.Query(`SELECT family,scope,gen,fails,ok_at,last_at FROM source_health_scope
		WHERE source_id=? ORDER BY family,scope,gen`, id)
	if err != nil {
		t.Fatalf("snapshot scoped: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f, sc string
		var gen, fails, okAt, lastAt int64
		if err := rows.Scan(&f, &sc, &gen, &fails, &okAt, &lastAt); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "scoped|%s|%s|%d|%d|%d|%d\n", f, sc, gen, fails, okAt, lastAt)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	rrows, err := s.db.Query(`SELECT family,scope,state,gen,last_probe_at,last_success_at,
		next_probe_at,rec_failures,COALESCE(last_error,''),updated_at
		FROM registry_host_recovery WHERE source_id=? ORDER BY family,scope`, id)
	if err != nil {
		t.Fatalf("snapshot recovery: %v", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var f, sc, st, le string
		var gen, lp, ls, nx, rf, up int64
		if err := rrows.Scan(&f, &sc, &st, &gen, &lp, &ls, &nx, &rf, &le, &up); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "rec|%s|%s|%s|%d|%d|%d|%d|%d|%s|%d\n", f, sc, st, gen, lp, ls, nx, rf, le, up)
	}
	if err := rrows.Err(); err != nil {
		t.Fatal(err)
	}

	var gen int64
	var digest string
	err = s.db.QueryRow(`SELECT gen,COALESCE(cred_digest,'') FROM registry_host_gen WHERE host=?`, host).
		Scan(&gen, &digest)
	if err == nil {
		fmt.Fprintf(&b, "gen|%d|%s\n", gen, digest)
	}
	return b.String()
}

func mustCommit(t *testing.T, s *Store, p HostProbeCommit) HostCapsCommitResult {
	t.Helper()
	res, err := s.CommitHostProbe(p)
	if err != nil {
		t.Fatalf("CommitHostProbe: %v", err)
	}
	return res
}

func TestCommitHostProbeSuccess(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 5, "d5")

	res := mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 1000, ErrText: "", Gen: 5, CredDigest: "d5", TTL: time.Hour,
	})
	if res != HostCapsCommitted {
		t.Fatalf("res=%v", res)
	}

	var capv int64
	var mode, probeErr string
	var probeAt int64
	if err := s.db.QueryRow(`SELECT registry_cap,registry_path_mode,registry_probe_at,
		COALESCE(registry_probe_error,'') FROM sources WHERE id=?`, id).
		Scan(&capv, &mode, &probeAt, &probeErr); err != nil {
		t.Fatal(err)
	}
	if capv != 1 || mode != PathNative || probeAt != 1000 || probeErr != "" {
		t.Fatalf("cap=%d mode=%s at=%d err=%s", capv, mode, probeAt, probeErr)
	}

	var fails, okAt, lastAt, gen int64
	if err := s.db.QueryRow(`SELECT fails,ok_at,last_at,gen FROM source_health_scope
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, id).
		Scan(&fails, &okAt, &lastAt, &gen); err != nil {
		t.Fatal(err)
	}
	if fails != 0 || okAt != 1000 || lastAt != 1000 || gen != 5 {
		t.Fatalf("scoped fails=%d ok=%d last=%d gen=%d", fails, okAt, lastAt, gen)
	}

	v, err := s.RecoveryState(id, "registry", "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Exists || v.State != RecoveryOK || v.Gen != 5 || v.LastSuccessAt != 1000 ||
		v.LastProbeAt != 1000 || v.RecFailures != 0 {
		t.Fatalf("recovery=%+v", v)
	}
	if v.NextProbeAt != 1000+3600 {
		t.Fatalf("next=%d 应为 at+TTL", v.NextProbeAt)
	}
}

func TestCommitHostProbeRejectsStaleGenWithoutSideEffects(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 5, "d5")
	mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 1000, Gen: 5, CredDigest: "d5", TTL: time.Hour,
	})

	// 探测在 gen=5 的起点上开始；期间凭据变更把 Host 推进到 gen=6。
	d6 := "d6"
	if err := s.ApplyCredChanges([]CredChange{{Host: "quay.io", Digest: &d6}}); err != nil {
		t.Fatal(err)
	}
	before := registrySnapshot(t, s, id, "quay.io")
	var capPre, probeAtPre int64
	var modePre, perrPre string
	if err := s.db.QueryRow(`SELECT registry_cap,registry_path_mode,registry_probe_at,
		COALESCE(registry_probe_error,'') FROM sources WHERE id=?`, id).
		Scan(&capPre, &modePre, &probeAtPre, &perrPre); err != nil {
		t.Fatal(err)
	}

	res := mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathHostPrefix,
		At: 2000, Gen: 5, CredDigest: "d5", TTL: time.Hour,
	})
	if res != HostCapsStaleSnapshot {
		t.Fatalf("旧代次必须整笔拒绝，res=%v", res)
	}
	after := registrySnapshot(t, s, id, "quay.io")
	if before != after {
		t.Fatalf("旧探测产生了副作用:\nbefore=%s\nafter=%s", before, after)
	}

	// 逐项确认旁路写入也没有发生（snapshot 已整体相等，这里是可读证据）。
	var capv int64
	var mode, perr string
	var probeAt int64
	if err := s.db.QueryRow(`SELECT registry_cap,registry_path_mode,registry_probe_at,
		COALESCE(registry_probe_error,'') FROM sources WHERE id=?`, id).
		Scan(&capv, &mode, &probeAt, &perr); err != nil {
		t.Fatal(err)
	}
	if capv != capPre || mode != modePre || probeAt != probeAtPre || perr != perrPre {
		t.Fatalf("sources 旁路被旧探测污染: cap=%d/%d mode=%q/%q probe_at=%d/%d err=%q/%q",
			capv, capPre, mode, modePre, probeAt, probeAtPre, perr, perrPre)
	}
	if capv != RegistryCapUnknown || mode != "" {
		t.Fatalf("凭据变更后 cap/path 应保持已清空: cap=%d mode=%q", capv, mode)
	}
	var recState string
	var recGen, recLast int64
	if err := s.db.QueryRow(`SELECT state,gen,last_probe_at FROM registry_host_recovery
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, id).
		Scan(&recState, &recGen, &recLast); err != nil {
		t.Fatal(err)
	}
	if recState != "initial" || recGen != 6 || recLast != 0 {
		t.Fatalf("recovery 被旧探测污染: %s/gen=%d/last=%d", recState, recGen, recLast)
	}

	// gen=5 的旧 scoped 行既不能清零，也不能被写成新失败。
	var fails, okAt int64
	if err := s.db.QueryRow(`SELECT fails,ok_at FROM source_health_scope
		WHERE source_id=? AND family='registry' AND scope='quay.io' AND gen=5`, id).
		Scan(&fails, &okAt); err != nil {
		t.Fatal(err)
	}
	if fails != 0 || okAt != 1000 {
		t.Fatalf("旧代次 scoped 被改写: fails=%d ok=%d", fails, okAt)
	}
	// 新代次没有任何证据行。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope
		WHERE source_id=? AND gen=6`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("不应出现 gen=6 的 scoped 行: %d", n)
	}
}

func TestCommitHostProbeRejectsDigestMismatch(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 5, "d5")
	before := registrySnapshot(t, s, id, "quay.io")

	res := mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 1000, Gen: 5, CredDigest: "other",
	})
	if res != HostCapsStaleSnapshot {
		t.Fatalf("digest 不匹配必须拒绝, res=%v", res)
	}
	if after := registrySnapshot(t, s, id, "quay.io"); before != after {
		t.Fatalf("digest 不匹配产生了副作用:\n%s\n%s", before, after)
	}
}

func TestCommitHostProbeInitializesMissingRecoveryRow(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 3, "d3")

	res := mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 500, Gen: 3, CredDigest: "d3", TTL: time.Hour,
	})
	if res != HostCapsCommitted {
		t.Fatalf("res=%v", res)
	}
	v, err := s.RecoveryState(id, "registry", "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Exists || v.State != RecoveryOK || v.Gen != 3 {
		t.Fatalf("缺行时应按本次探测结果初始化: %+v", v)
	}
}

func TestCommitHostProbeMissingGenRow(t *testing.T) {
	cases := []struct {
		name    string
		gen     int64
		hasCred bool
		want    HostCapsCommitResult
	}{
		{"无凭据 gen=0 放行并建行", 0, false, HostCapsCommitted},
		{"无凭据但 gen>0 拒绝", 3, false, HostCapsStaleSnapshot},
		{"有凭据但没有代次行拒绝", 0, true, HostCapsStaleSnapshot},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			id := seedRegistrySource(t, s, "quay.io")
			res := mustCommit(t, s, HostProbeCommit{
				SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
				At: 100, Gen: c.gen, HasCredential: c.hasCred,
			})
			if res != c.want {
				t.Fatalf("res=%v want=%v", res, c.want)
			}
			if c.want == HostCapsCommitted {
				gen, _, err := s.HostGenState("quay.io")
				if err != nil || gen != 0 {
					t.Fatalf("应建立 gen=0 行: gen=%d err=%v", gen, err)
				}
			}
		})
	}
}

func TestCommitHostProbeRejectsArchivedAndMovedHost(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 2, "d2")

	if _, err := s.db.Exec(`UPDATE sources SET status='archived' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	before := registrySnapshot(t, s, id, "quay.io")
	if res := mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 10, Gen: 2, CredDigest: "d2",
	}); res != HostCapsSourceUnavailable {
		t.Fatalf("archived 应拒绝, res=%v", res)
	}
	if after := registrySnapshot(t, s, id, "quay.io"); before != after {
		t.Fatalf("archived 拒绝后状态变化:\n%s\n%s", before, after)
	}

	if _, err := s.db.Exec(`UPDATE sources SET status='usable', repo_host='docker.io' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if res := mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 20, Gen: 2, CredDigest: "d2",
	}); res != HostCapsSourceChanged {
		t.Fatalf("归属已变应拒绝旧 Host 结果, res=%v", res)
	}
}

func TestCommitHostProbeRollsBackWhenScopedFails(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 4, "d4")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_scoped_insert BEFORE INSERT ON source_health_scope
		BEGIN SELECT RAISE(ABORT,'injected'); END;`); err != nil {
		t.Fatal(err)
	}

	before := registrySnapshot(t, s, id, "quay.io")
	_, err := s.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 300, Gen: 4, CredDigest: "d4",
	})
	if err == nil {
		t.Fatal("scoped 写失败必须返回错误")
	}
	if after := registrySnapshot(t, s, id, "quay.io"); before != after {
		t.Fatalf("cap 必须随 scoped 一起回滚:\n%s\n%s", before, after)
	}
}

func TestCommitHostProbeRollsBackWhenRecoveryFails(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 4, "d4")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_recovery_insert BEFORE INSERT ON registry_host_recovery
		BEGIN SELECT RAISE(ABORT,'injected'); END;`); err != nil {
		t.Fatal(err)
	}

	before := registrySnapshot(t, s, id, "quay.io")
	_, err := s.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 300, Gen: 4, CredDigest: "d4",
	})
	if err == nil {
		t.Fatal("recovery 写失败必须返回错误")
	}
	if after := registrySnapshot(t, s, id, "quay.io"); before != after {
		t.Fatalf("cap 与 scoped 必须随 recovery 一起回滚:\n%s\n%s", before, after)
	}
}

func TestCommitHostProbeTransportErrorKeepsCap(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 1, "d1")
	mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 1000, Gen: 1, CredDigest: "d1", TTL: time.Hour,
	})

	mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapUnknown, Reason: probeReasonTransportError,
		Transport: "timeout", At: 2000, ErrText: "dial timeout", Gen: 1, CredDigest: "d1",
	})

	var capv, probeAt int64
	var mode string
	if err := s.db.QueryRow(`SELECT registry_cap,registry_path_mode,registry_probe_at FROM sources WHERE id=?`, id).
		Scan(&capv, &mode, &probeAt); err != nil {
		t.Fatal(err)
	}
	if capv != 1 || mode != PathNative || probeAt != 1000 {
		t.Fatalf("传输错误不得改写 cap/probe_at: cap=%d mode=%s at=%d", capv, mode, probeAt)
	}

	var fails, lastAt int64
	if err := s.db.QueryRow(`SELECT fails,last_at FROM source_health_scope
		WHERE source_id=? AND family='registry' AND scope='quay.io' AND gen=1`, id).
		Scan(&fails, &lastAt); err != nil {
		t.Fatal(err)
	}
	if fails != 1 || lastAt != 2000 {
		t.Fatalf("传输错误仍要记 scoped 失败: fails=%d last=%d", fails, lastAt)
	}
	v, _ := s.RecoveryState(id, "registry", "quay.io")
	if v.State != RecoveryFailed || v.RecFailures != 1 || v.LastSuccessAt != 1000 {
		t.Fatalf("recovery=%+v", v)
	}
}

func TestCommitHostProbeHardExcluded(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 2, "d2")
	mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapMismatch,
		At: 100, Gen: 2, CredDigest: "d2",
	})
	v, _ := s.RecoveryState(id, "registry", "quay.io")
	if v.State != RecoveryHardExcluded || v.NextProbeAt != 0 {
		t.Fatalf("recovery=%+v", v)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope WHERE source_id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cap=2 不应写 scoped: %d", n)
	}
}

func TestCommitHostProbeSameHostMultipleSources(t *testing.T) {
	s, _ := openTestStore(t)
	a := seedRegistrySource(t, s, "quay.io")
	b := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 7, "d7")

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.CommitHostProbe(HostProbeCommit{
				SourceID: a, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
				At: 100, Gen: 7, CredDigest: "d7"}); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.CommitHostProbe(HostProbeCommit{
				SourceID: b, Host: "quay.io", Cap: RegistryCapUnknown, Reason: "probe_unavailable",
				At: 100, ErrText: "boom", Gen: 7, CredDigest: "d7"}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发写回失败: %v", err)
	}

	va, _ := s.RecoveryState(a, "registry", "quay.io")
	vb, _ := s.RecoveryState(b, "registry", "quay.io")
	if va.State != RecoveryOK || va.RecFailures != 0 {
		t.Fatalf("A 被污染: %+v", va)
	}
	if vb.State != RecoveryFailed || vb.RecFailures != 8 {
		t.Fatalf("B 失败计数应独立累加: %+v", vb)
	}
}

func TestApplyCredChangesResetsRecovery(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	setGen(t, s, "quay.io", 1, "d1")
	mustCommit(t, s, HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 1000, Gen: 1, CredDigest: "d1", TTL: time.Hour,
	})

	d2 := "d2"
	if err := s.ApplyCredChanges([]CredChange{{Host: "quay.io", Digest: &d2}}); err != nil {
		t.Fatal(err)
	}
	v, err := s.RecoveryState(id, "registry", "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != RecoveryInitial || v.Gen != 2 || v.LastProbeAt != 0 ||
		v.RecFailures != 0 || v.LastError != "" {
		t.Fatalf("recovery 未按新代次重置: %+v", v)
	}
	if v.LastSuccessAt != 1000 {
		t.Fatalf("last_success_at 应保留为历史事实: %d", v.LastSuccessAt)
	}
	if v.NextProbeAt == 0 {
		t.Fatal("next_probe_at 必须置为 now 以便下一轮捞起")
	}
	var capv int64
	if err := s.db.QueryRow(`SELECT registry_cap FROM sources WHERE id=?`, id).Scan(&capv); err != nil {
		t.Fatal(err)
	}
	if capv != RegistryCapUnknown {
		t.Fatalf("凭据变更必须清 cap, got %d", capv)
	}
}

func TestSetRepoHostCleansOldScope(t *testing.T) {
	s, _ := openTestStore(t)
	a := seedRegistrySource(t, s, "quay.io")
	b := seedRegistrySource(t, s, "quay.io")
	other := seedRegistrySource(t, s, "gcr.io")
	setGen(t, s, "quay.io", 3, "d3")
	for _, id := range []int64{a, b} {
		mustCommit(t, s, HostProbeCommit{
			SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
			At: 100, Gen: 3, CredDigest: "d3",
		})
	}

	if err := s.SetRepoHost(a, "gcr.io"); err != nil {
		t.Fatal(err)
	}

	// 旧归属的 scoped 与 recovery 必须清掉。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, a).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("旧 Host 的 scoped 未清理: %d", n)
	}
	v, err := s.RecoveryState(a, "registry", "gcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Exists || v.State != RecoveryInitial || v.LastProbeAt != 0 {
		t.Fatalf("新 Host 应等待新的探测证据: %+v", v)
	}
	if v.NextProbeAt == 0 {
		t.Fatal("新 Host 的 recovery 必须可被下一轮捞起")
	}

	// 同 Host 上的另一个源不受影响。
	vb, _ := s.RecoveryState(b, "registry", "quay.io")
	if !vb.Exists || vb.State != RecoveryOK {
		t.Fatalf("其他源被误清: %+v", vb)
	}
	// 其他 Host 的源不受影响。
	if _, err := s.RecoveryState(other, "registry", "gcr.io"); err != nil {
		t.Fatal(err)
	}
}

func TestCommitHostProbeRejectsHub(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "quay.io")
	for _, h := range []string{"", "ghcr.io"} {
		if _, err := s.CommitHostProbe(HostProbeCommit{
			SourceID: id, Host: h, Cap: RegistryCapYes, PathMode: PathNative, At: 1,
		}); err == nil {
			t.Fatalf("Host %q 应被拒绝", h)
		}
	}
}
