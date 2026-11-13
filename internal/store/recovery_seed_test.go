package store

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var c6eSeq atomic.Int64

// seedSource 插入一条存量 Host 源。repo_host 为空表示非 Host 源。
func seedSource(t *testing.T, s *Store, host string, cap int, status string, enabled int) int64 {
	t.Helper()
	n := c6eSeq.Add(1)
	base := fmt.Sprintf("https://seed-%d.example", n)
	if host != "" {
		base += "/" + host
	}
	identity := IdentityUnclassified
	if host != "" {
		identity = IdentityIdentified
	}
	res, err := s.db.Exec(`INSERT INTO sources(base_url,name,repo_host,registry_identity_state,registry_cap,registry_path_mode,status,enabled,created_at)
		VALUES(?,?,?,?,?,'native',?,?,?)`,
		base, fmt.Sprintf("seed-%d", n), host, identity, cap, status, enabled, 1)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedScopedLegacy(t *testing.T, s *Store, id int64, host string, gen int64, fails int, okAt, lastAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO source_health_scope
		(source_id,family,scope,gen,fails,ok_at,last_at) VALUES(?,'registry',?,?,?,?,?)`,
		id, host, gen, fails, okAt, lastAt); err != nil {
		t.Fatalf("insert scoped: %v", err)
	}
}

// fixSeedClock 固定时钟，避免迁移时刻与断言跨秒。
func fixSeedClock(t *testing.T, s *Store) int64 {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return now })
	return now.Unix()
}

// seedNow 直接执行一次种子，不清标记：标记存在时应当整体跳过。
func seedNow(t *testing.T, s *Store) {
	t.Helper()
	if err := s.seedRecoveryFromScope(); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// clearSeedMark 还原「尚未执行过种子」的升级现场。
func clearSeedMark(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`DELETE FROM meta WHERE k=?`, recoverySeedKey); err != nil {
		t.Fatal(err)
	}
}

// runFirstSeed 模拟首次升级：无标记状态下执行一次。
func runFirstSeed(t *testing.T, s *Store) {
	t.Helper()
	clearSeedMark(t, s)
	seedNow(t, s)
}

func readRecovery(t *testing.T, s *Store, id int64) (state string, gen, lastProbe, lastSuccess, next int64, fails int) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures
		FROM registry_host_recovery WHERE source_id=? AND family='registry'`, id).
		Scan(&state, &gen, &lastProbe, &lastSuccess, &next, &fails); err != nil {
		t.Fatalf("read recovery %d: %v", id, err)
	}
	return
}

func TestSeedRecoveryLegacyEvidenceBecomesInitial(t *testing.T) {
	s, _ := openTestStore(t)
	now := fixSeedClock(t, s)

	// 历史证据齐全：曾经成功、也有失败计数，但 gen=0 无法证明属于当前代次。
	id := seedSource(t, s, "reg-a.example", RegistryCapYes, "usable", 1)
	seedScopedLegacy(t, s, id, "reg-a.example", 0, 3, now-3600, now-60)

	runFirstSeed(t, s)

	state, gen, lastProbe, lastSuccess, next, fails := readRecovery(t, s, id)
	if state != RecoveryInitial {
		t.Fatalf("历史证据不得派生成 %s", state)
	}
	if gen != 0 || lastProbe != 0 || lastSuccess != 0 || fails != 0 {
		t.Fatalf("历史字段被写入: gen=%d probe=%d success=%d fails=%d", gen, lastProbe, lastSuccess, fails)
	}
	if next != now {
		t.Fatalf("initial 的 next_probe_at 应为迁移时刻，got %d want %d", next, now)
	}

	// 历史 fails=3 绝不能生成 failed。
	if state == RecoveryFailed {
		t.Fatal("gen=0 历史 fails 被当成当前证据")
	}
	// 历史 ok_at 绝不能生成 ok/expired。
	if state == RecoveryOK || state == RecoveryExpired {
		t.Fatal("gen=0 历史 ok_at 被当成当前证据")
	}
}

func TestSeedRecoveryCapStates(t *testing.T) {
	s, _ := openTestStore(t)
	now := fixSeedClock(t, s)

	mismatch := seedSource(t, s, "reg-m.example", RegistryCapMismatch, "usable", 1)
	no := seedSource(t, s, "reg-n.example", RegistryCapNo, "usable", 1)
	unknown := seedSource(t, s, "reg-u.example", RegistryCapUnknown, "usable", 1)

	runFirstSeed(t, s)

	state, _, _, _, next, _ := readRecovery(t, s, mismatch)
	if state != RecoveryHardExcluded || next != 0 {
		t.Fatalf("cap=2: state=%s next=%d", state, next)
	}

	state, _, _, _, next, _ = readRecovery(t, s, no)
	if state != RecoveryUnsupported {
		t.Fatalf("cap=0: state=%s", state)
	}
	if next != now+int64(recoveryUnsupportedInterval/time.Second) {
		t.Fatalf("cap=0 next=%d，应为 now+6h", next)
	}

	state, _, _, _, next, _ = readRecovery(t, s, unknown)
	if state != RecoveryInitial || next != now {
		t.Fatalf("cap=-1: state=%s next=%d", state, next)
	}
}

func TestSeedRecoveryScope(t *testing.T) {
	s, _ := openTestStore(t)

	disabled := seedSource(t, s, "reg-d.example", RegistryCapYes, "usable", 0)
	archived := seedSource(t, s, "reg-a.example", RegistryCapYes, "archived", 1)
	ghcr := seedSource(t, s, "ghcr.io", RegistryCapYes, "usable", 1)
	empty := seedSource(t, s, "", RegistryCapYes, "usable", 1)

	runFirstSeed(t, s)

	// disabled 仍然建行。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM registry_host_recovery WHERE source_id=?`, disabled).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("disabled 源应建行，got %d", n)
	}

	for name, id := range map[string]int64{"archived": archived, "ghcr": ghcr, "empty": empty} {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM registry_host_recovery WHERE source_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s 不应建行，got %d", name, n)
		}
	}
}

func TestSeedRecoveryDoesNotOverwrite(t *testing.T) {
	s, _ := openTestStore(t)
	now := fixSeedClock(t, s)

	id := seedSource(t, s, "reg-a.example", RegistryCapYes, "usable", 1)
	if _, err := s.db.Exec(`INSERT INTO registry_host_recovery
		(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,last_error,updated_at)
		VALUES(?,'registry','reg-a.example','failed',7,?,0,?,3,'boom',?)`,
		id, now-10, now+600, now); err != nil {
		t.Fatal(err)
	}

	runFirstSeed(t, s)

	state, gen, _, _, next, fails := readRecovery(t, s, id)
	if state != RecoveryFailed || gen != 7 || next != now+600 || fails != 3 {
		t.Fatalf("已有行被覆盖: state=%s gen=%d next=%d fails=%d", state, gen, next, fails)
	}
}

func TestSeedRecoveryIdempotent(t *testing.T) {
	s, _ := openTestStore(t)

	kept := seedSource(t, s, "reg-a.example", RegistryCapYes, "usable", 1)
	removed := seedSource(t, s, "reg-b.example", RegistryCapNo, "usable", 1)

	runFirstSeed(t, s)

	var mark string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, recoverySeedKey).Scan(&mark); err != nil {
		t.Fatalf("标记未写入 meta: %v", err)
	}
	if mark != "1" {
		t.Fatalf("标记值=%q", mark)
	}
	before := dumpRecovery(t, s)

	// 手动删掉一条恢复行，模拟运维误删：标记存在时 seed 必须整体跳过，
	// 不能靠 ON CONFLICT 顺带把它补回来。
	if _, err := s.db.Exec(`DELETE FROM registry_host_recovery WHERE source_id=?`, removed); err != nil {
		t.Fatal(err)
	}

	seedNow(t, s)

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM registry_host_recovery WHERE source_id=?`, removed).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("标记存在时 seed 仍补回了被删的行，说明没有整体跳过")
	}

	// 其余行逐字节不变。
	after := dumpRecovery(t, s)
	if strings.Count(after, "\n") != strings.Count(before, "\n")-1 {
		t.Fatalf("除被删行外数据发生变化:\n%s\n---\n%s", before, after)
	}
	if !strings.Contains(after, fmt.Sprintf("%d/", kept)) {
		t.Fatalf("保留行丢失:\n%s", after)
	}
}

func dumpRecovery(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT source_id,scope,state,gen,last_probe_at,last_success_at,
		next_probe_at,rec_failures,last_error FROM registry_host_recovery ORDER BY source_id,scope`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var (
			id, gen, lp, ls, np int64
			scope, state, errS  string
			fails               int
		)
		if err := rows.Scan(&id, &scope, &state, &gen, &lp, &ls, &np, &fails, &errS); err != nil {
			t.Fatal(err)
		}
		out += fmt.Sprintf("%d/%s/%s/%d/%d/%d/%d/%d/%s\n", id, scope, state, gen, lp, ls, np, fails, errS)
	}
	return out
}

func TestSeedRecoveryRollback(t *testing.T) {
	s, _ := openTestStore(t)

	// 三条源、与扫描顺序无关：前两行允许写入，第三行起 RAISE(ABORT)。
	// 任何顺序下都会先成功写入两行再失败，从而真正验证部分写入的整体回滚。
	ids := []int64{
		seedSource(t, s, "reg-a.example", RegistryCapYes, "usable", 1),
		seedSource(t, s, "reg-b.example", RegistryCapNo, "usable", 1),
		seedSource(t, s, "reg-c.example", RegistryCapMismatch, "usable", 1),
	}

	if _, err := s.db.Exec(`CREATE TRIGGER seed_boom BEFORE INSERT ON registry_host_recovery
		WHEN (SELECT COUNT(*) FROM registry_host_recovery) >= 2
		BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}
	clearSeedMark(t, s)

	if err := s.seedRecoveryFromScope(); err == nil {
		t.Fatal("预期种子失败")
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM registry_host_recovery`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("失败后残留了 %d 行，部分写入未回滚", n)
	}
	for _, id := range ids {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM registry_host_recovery WHERE source_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("源 %d 残留恢复行", id)
		}
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM meta WHERE k=?`, recoverySeedKey).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("失败后残留了完成标记")
	}
}

// 种子只搬运 repo_host，依赖「写入即规范化」这条不变量。
// 3.5.0 起 repo_host 的写入路径只有 AddSource / ApplyCredChanges / 历史还原，
// 三处都走 registryhost.Check；这里把该不变量钉在测试里，防止以后新增写入路径绕过。
//
// 注意：AddSource 自己会建 recovery 行，必须删掉再 seed，否则只会命中
// ON CONFLICT DO NOTHING，证明不了 seed 插入时用的是同一个 scope。
func TestSeedRecoveryScopeEqualsNormalizedRepoHost(t *testing.T) {
	s, _ := openTestStore(t)

	id, err := s.AddSource(Source{
		BaseURL:      "https://norm.example",
		Name:         "norm",
		RepoHost:     "  Quay.IO.  ",
		RegistryCap:  RegistryCapYes,
		RegistryPath: "native",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	var host string
	if err := s.db.QueryRow(`SELECT repo_host FROM sources WHERE id=?`, id).Scan(&host); err != nil {
		t.Fatal(err)
	}
	if host != "quay.io" {
		t.Fatalf("repo_host 未规范化: %q", host)
	}

	// 清掉 AddSource 自建的 recovery 行，让随后的 seed 真正执行插入。
	if _, err := s.db.Exec(`DELETE FROM registry_host_recovery WHERE source_id=?`, id); err != nil {
		t.Fatal(err)
	}
	runFirstSeed(t, s)

	var scope, state string
	if err := s.db.QueryRow(`SELECT scope, state FROM registry_host_recovery
		WHERE source_id=? AND family='registry'`, id).Scan(&scope, &state); err != nil {
		t.Fatalf("seed 未建立 recovery 行: %v", err)
	}
	if scope != host {
		t.Fatalf("seed 写入 scope=%q，repo_host=%q，两者必须同源", scope, host)
	}
	if scope != "quay.io" {
		t.Fatalf("seed scope 未规范化: %q", scope)
	}
	if state != RecoveryInitial {
		t.Fatalf("seed 状态=%s，应为 initial", state)
	}
}

func TestSeedRecoveryRunsOnOpen(t *testing.T) {
	s, path := openTestStore(t)
	id := seedSource(t, s, "reg-a.example", RegistryCapYes, "usable", 1)

	if _, err := s.db.Exec(`DELETE FROM meta WHERE k=?`, recoverySeedKey); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if state, _, _, _, _, _ := readRecovery(t, s2, id); state != RecoveryInitial {
		t.Fatalf("Open 后没有种子行: %s", state)
	}
}

// 种子迁移只建立 initial，候选必须等当前代次探测成功。
func TestSeedRecoveryCandidateOnlyAfterProbe(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedSource(t, s, "quay.io", RegistryCapYes, "usable", 1)

	runFirstSeed(t, s)

	got, err := s.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; ok {
		t.Fatal("initial 不应进入候选")
	}

	if _, err := s.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: fixSeedClock(t, s), Gen: 0,
	}); err != nil {
		t.Fatal(err)
	}

	got, err = s.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(got)[id]; !ok {
		t.Fatal("探测成功后仍不在候选")
	}
}
