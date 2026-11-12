package store

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

var seedSeq int64

func seedRegistrySource(t *testing.T, s *Store, host string) int64 {
	t.Helper()
	n := atomic.AddInt64(&seedSeq, 1)
	res, err := s.db.Exec(`INSERT INTO sources(base_url,name,created_at,repo_host) VALUES(?,?,?,?)`,
		"https://"+host+"/x"+strconv.FormatInt(n, 10), "src-"+host+"-"+strconv.FormatInt(n, 10), 1, host)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestRecoveryStateScoping(t *testing.T) {
	s, _ := openTestStore(t)
	a := seedRegistrySource(t, s, "ghcr.io")
	b := seedRegistrySource(t, s, "quay.io")

	if _, err := s.db.Exec(`INSERT INTO registry_host_recovery
		(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,last_error,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		a, "registry", "ghcr.io", "ok", 2, 100, 100, 200, 0, "", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO registry_host_recovery
		(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,rec_failures,last_error,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		b, "registry", "quay.io", "failed", 1, 100, 0, 200, 2, "boom", 100); err != nil {
		t.Fatal(err)
	}

	v, err := s.RecoveryState(a, "registry", "ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Exists || v.State != "ok" || v.Gen != 2 {
		t.Fatalf("应读到 A 自己的行: %+v", v)
	}

	// 同一 source 但换成别的 Host：不能读到别的 scope 的行。
	if v, err = s.RecoveryState(a, "registry", "quay.io"); err != nil || v.Exists {
		t.Fatalf("跨 Host 读取应无行: %+v err=%v", v, err)
	}

	// 规范化后命中（大小写 + 尾点）。
	if v, err = s.RecoveryState(a, "registry", "GHCR.IO."); err != nil || !v.Exists {
		t.Fatalf("规范化后应命中: %+v err=%v", v, err)
	}

	// 空 scope 规范化成 ""，不等于 ghcr.io。
	if v, err = s.RecoveryState(a, "registry", ""); err != nil || v.Exists {
		t.Fatalf("空 scope 不应命中: %+v err=%v", v, err)
	}

	// B 的行只能从 B 读到。
	if v, err = s.RecoveryState(b, "registry", "quay.io"); err != nil || !v.Exists || v.State != "failed" {
		t.Fatalf("B 的行读取失败: %+v err=%v", v, err)
	}
	if v, err = s.RecoveryState(b, "registry", "ghcr.io"); err != nil || v.Exists {
		t.Fatalf("B 不能读到 A 的 scope: %+v err=%v", v, err)
	}
}

func TestScopedHealthFiltersStaleGen(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "ghcr.io")
	if _, err := s.db.Exec(`INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at) VALUES(?,?,?,?)`,
		"ghcr.io", 4, "digest", 1); err != nil {
		t.Fatal(err)
	}
	ins := func(family, scope string, gen int64, fails int) {
		if _, err := s.db.Exec(`INSERT INTO source_health_scope(source_id,family,scope,gen,fails,ok_at,last_at)
			VALUES(?,?,?,?,?,0,0)`, id, family, scope, gen, fails); err != nil {
			t.Fatal(err)
		}
	}
	ins("registry", "ghcr.io", 0, 2) // 旧代次：必须忽略
	ins("registry", "ghcr.io", 3, 1) // 旧代次：必须忽略
	ins("registry", "ghcr.io", 4, 0) // 当前代次
	ins("docker", "", 0, 5)          // 非 registry：旧语义保留

	got, err := s.ScopedHealthOf(id)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := got["registry@ghcr.io"]; !ok || n != 0 {
		t.Fatalf("registry 应只返回当前代次那行: %v", got)
	}
	if n, ok := got["docker@"]; !ok || n != 5 {
		t.Fatalf("非 registry family 应保持旧语义: %v", got)
	}

	// 规范化：入库的小写值经 Check 后仍是同一个 key。
	if n, ok := got["registry@GHCR.IO."]; ok && n != 0 {
		t.Fatalf("不应出现未规范化的 key: %v", got)
	}
}

func TestScopedHealthWithoutGenRow(t *testing.T) {
	s, _ := openTestStore(t)
	id := seedRegistrySource(t, s, "ghcr.io")
	if _, err := s.db.Exec(`INSERT INTO source_health_scope(source_id,family,scope,gen,fails,ok_at,last_at)
		VALUES(?,?,?,?,?,0,0)`, id, "registry", "ghcr.io", 0, 2); err != nil {
		t.Fatal(err)
	}
	// 没有 registry_host_gen 行时 currentGen=0，gen=0 就是当前代次。
	got, err := s.ScopedHealthOf(id)
	if err != nil {
		t.Fatal(err)
	}
	if got["registry@ghcr.io"] != 2 {
		t.Fatalf("currentGen=0 时应保留 gen=0 证据: %v", got)
	}
}

func TestDeriveState(t *testing.T) {
	now := int64(100000)
	okBase := DeriveInput{
		Cap: RegistryCapYes, Now: now, TTL: 60 * time.Second, CurrentGen: 2,
		HasRecovery: true, RecoveryGen: 2, LastProbeAt: now - 100, LastSuccessAt: now - 10,
		HasScoped: true, ScopedGen: 2, ScopedFails: 0,
	}
	with := func(f func(*DeriveInput)) DeriveInput {
		in := okBase
		f(&in)
		return in
	}

	cases := []struct {
		name string
		in   DeriveInput
		want string
	}{
		{"cap=2 硬排除优先于一切", with(func(i *DeriveInput) { i.Cap = RegistryCapMismatch; i.RecoveryGen = 0; i.ScopedGen = 0 }), RecoveryHardExcluded},
		{"cap=0 不支持", with(func(i *DeriveInput) { i.Cap = RegistryCapNo; i.ScopedFails = 3 }), RecoveryUnsupported},
		{"新鲜成功", okBase, RecoveryOK},
		{"TTL 恰好等于边界仍算 ok", with(func(i *DeriveInput) { i.LastSuccessAt = now - 60 }), RecoveryOK},
		{"超过 TTL 一秒转 expired", with(func(i *DeriveInput) { i.LastSuccessAt = now - 61 }), RecoveryExpired},
		{"失败证据优先于过期成功", with(func(i *DeriveInput) {
			i.LastSuccessAt = now - 3600
			i.ScopedFails = 1
		}), RecoveryFailed},
		{"失败证据优先于新鲜成功", with(func(i *DeriveInput) { i.ScopedFails = 2 }), RecoveryFailed},
		{"lastError 非空也算失败", with(func(i *DeriveInput) { i.LastError = "timeout" }), RecoveryFailed},
		{"recFailures>0 也算失败", with(func(i *DeriveInput) { i.RecFailures = 1 }), RecoveryFailed},
		{"last_success_at=0 不能判 ok", with(func(i *DeriveInput) { i.LastSuccessAt = 0 }), RecoveryInitial},
		{"recovery.gen 不匹配落 initial", with(func(i *DeriveInput) { i.RecoveryGen = 1 }), RecoveryInitial},
		{"scoped.gen 不匹配落 initial", with(func(i *DeriveInput) { i.ScopedGen = 1 }), RecoveryInitial},
		{"无 scoped 行落 initial", with(func(i *DeriveInput) { i.HasScoped = false }), RecoveryInitial},
		{"无 recovery 行落 initial", with(func(i *DeriveInput) { i.HasRecovery = false }), RecoveryInitial},
		{"未探测过落 initial", with(func(i *DeriveInput) { i.LastProbeAt = 0 }), RecoveryInitial},
		{"cap=-1 但当前代次有失败 -> failed", with(func(i *DeriveInput) {
			i.Cap = RegistryCapUnknown
			i.ScopedFails = 1
		}), RecoveryFailed},
		{"cap=-1 无失败证据 -> initial", with(func(i *DeriveInput) {
			i.Cap = RegistryCapUnknown
			i.LastSuccessAt = 0
		}), RecoveryInitial},
		{"currentGen=0 时 gen=0 仍算当前代次", with(func(i *DeriveInput) {
			i.CurrentGen, i.RecoveryGen, i.ScopedGen = 0, 0, 0
		}), RecoveryOK},
		{"TTL=0 时仅当秒级同刻算 ok", with(func(i *DeriveInput) {
			i.TTL = 0
			i.LastSuccessAt = now
		}), RecoveryOK},
		{"TTL=0 且已过一秒 -> expired", with(func(i *DeriveInput) {
			i.TTL = 0
			i.LastSuccessAt = now - 1
		}), RecoveryExpired},
		{"cap=1 但过期成功 + 无失败 -> expired", with(func(i *DeriveInput) {
			i.LastSuccessAt = now - 3600
		}), RecoveryExpired},
	}

	for _, c := range cases {
		if got := DeriveState(c.in); got != c.want {
			t.Errorf("%s: got=%s want=%s (%+v)", c.name, got, c.want, c.in)
		}
	}
}
