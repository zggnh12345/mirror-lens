package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openReplicaStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func replicaFixture(authorityRepo, replicaRepo string, prio int) RegistryReplica {
	return RegistryReplica{
		AuthorityRegistry:   "registry.k8s.io",
		AuthorityRepository: authorityRepo,
		ReplicaRegistry:     "quay.io",
		ReplicaRepository:   replicaRepo,
		Priority:            prio,
		Enabled:             true,
	}
}

// 生命周期：新建/修改/删除都必须同事务递增权威 Registry 代次并写审计快照。
func TestRegistryReplicaLifecycle(t *testing.T) {
	st := openReplicaStore(t)
	auth := "registry.k8s.io"

	g0, err := st.ReplicaGeneration(auth)
	if err != nil {
		t.Fatalf("初始代次: %v", err)
	}
	rep := replicaFixture("kube-apiserver", "mirror/kube-apiserver", 10)
	g1, err := st.PutRegistryReplica(rep, "admin", true)
	if err != nil {
		t.Fatalf("新建: %v", err)
	}
	if g1 <= int64(g0) {
		t.Fatalf("新建必须递增代次：%d → %d", g0, g1)
	}
	if _, err := st.PutRegistryReplica(rep, "admin", true); !errors.Is(err, ErrReplicaExists) {
		t.Fatalf("POST 重复必须返回 ErrReplicaExists，实得 %v", err)
	}
	rep.Priority = 20
	g2, err := st.PutRegistryReplica(rep, "admin", false)
	if err != nil {
		t.Fatalf("修改: %v", err)
	}
	if g2 != g1+1 {
		t.Fatalf("修改应只递增 1：%d → %d", g1, g2)
	}
	g3, err := st.DeleteRegistryReplica(auth, rep.AuthorityRepository, rep.ReplicaRegistry, "admin")
	if err != nil {
		t.Fatalf("删除: %v", err)
	}
	if g3 != g2+1 {
		t.Fatalf("删除应只递增 1：%d → %d", g2, g3)
	}
	if _, err := st.DeleteRegistryReplica(auth, rep.AuthorityRepository, rep.ReplicaRegistry, "admin"); !errors.Is(err, ErrReplicaMissing) {
		t.Fatalf("重复删除应返回 ErrReplicaMissing，实得 %v", err)
	}
	audit, err := st.ListRegistryReplicaAudit(auth, 10)
	if err != nil {
		t.Fatalf("审计: %v", err)
	}
	if len(audit) != 3 {
		t.Fatalf("审计行数 = %d，期望 3（create/update/delete）", len(audit))
	}
	if audit[0].Operation != "delete" || audit[0].OldSnapshot == "" {
		t.Fatalf("删除审计必须带删除前快照: %+v", audit[0])
	}
	if audit[2].Operation != "create" || audit[2].NewSnapshot == "" {
		t.Fatalf("新建审计必须带新快照: %+v", audit[2])
	}
}

// 校验：通配符/同名 Registry/路径穿越都必须在写入口被拒绝。
func TestRegistryReplicaValidation(t *testing.T) {
	st := openReplicaStore(t)
	cases := []struct {
		name string
		rep  RegistryReplica
	}{
		{"通配符仓库", RegistryReplica{AuthorityRegistry: "registry.k8s.io", AuthorityRepository: "kube-*", ReplicaRegistry: "quay.io", ReplicaRepository: "x/y", Enabled: true}},
		{"通配符 Registry", RegistryReplica{AuthorityRegistry: "*.pkg.dev", AuthorityRepository: "a/b", ReplicaRegistry: "quay.io", ReplicaRepository: "x/y", Enabled: true}},
		{"副本等于权威", RegistryReplica{AuthorityRegistry: "quay.io", AuthorityRepository: "a/b", ReplicaRegistry: "QUAY.IO", ReplicaRepository: "x/y", Enabled: true}},
		{"路径穿越", RegistryReplica{AuthorityRegistry: "registry.k8s.io", AuthorityRepository: "a/../b", ReplicaRegistry: "quay.io", ReplicaRepository: "x/y", Enabled: true}},
		{"非法字符", RegistryReplica{AuthorityRegistry: "registry.k8s.io", AuthorityRepository: "a/b", ReplicaRegistry: "quay.io", ReplicaRepository: "x/Y", Enabled: true}},
		{"有效期倒置", RegistryReplica{AuthorityRegistry: "registry.k8s.io", AuthorityRepository: "a/b", ReplicaRegistry: "quay.io", ReplicaRepository: "x/y", ValidFrom: 100, ValidUntil: 100, Enabled: true}},
	}
	for _, c := range cases {
		if _, err := st.PutRegistryReplica(c.rep, "admin", true); err == nil {
			t.Fatalf("%s: 非法映射被接受: %+v", c.name, c.rep)
		}
	}
	if _, err := st.ReplicaGeneration("registry.k8s.io"); err != nil {
		t.Fatalf("代次: %v", err)
	}
	if all, _ := st.ListRegistryReplicas(""); len(all) != 0 {
		t.Fatalf("非法写入不得落库，实得 %d 行", len(all))
	}
}

// 精确匹配 + 按 priority 固定排序 + 有效期过滤；不做同名隐式映射。
func TestRegistryReplicaLookupExactAndOrdered(t *testing.T) {
	st := openReplicaStore(t)
	p1 := replicaFixture("kube-apiserver", "mirror/api-a", 30)
	p2 := replicaFixture("kube-apiserver", "mirror/api-b", 10)
	other := replicaFixture("kube-scheduler", "mirror/sched", 5)
	// 同一权威仓库下，副本 Registry 由主键去重；换一个副本 Registry 才能加第二行。
	p2.ReplicaRegistry = "gcr.io"
	for _, r := range []RegistryReplica{p1, p2, other} {
		if _, err := st.PutRegistryReplica(r, "admin", true); err != nil {
			t.Fatalf("写入 %+v: %v", r, err)
		}
	}
	got, err := st.ActiveRegistryReplicas("registry.k8s.io", "kube-apiserver", time.Now())
	if err != nil {
		t.Fatalf("查询: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("精确匹配应只返回 2 行，实得 %d", len(got))
	}
	if got[0].ReplicaRegistry != "gcr.io" || got[1].ReplicaRegistry != "quay.io" {
		t.Fatalf("必须按 priority 排序，实得 %s,%s", got[0].ReplicaRegistry, got[1].ReplicaRegistry)
	}
	// 不做同名隐式映射：权威仓库没有登记时不能返回任何副本。
	if got, _ := st.ActiveRegistryReplicas("registry.k8s.io", "kube-proxy", time.Now()); len(got) != 0 {
		t.Fatalf("未登记的仓库不得有隐式副本，实得 %d", len(got))
	}
	// 有效期过滤：未生效的行不参与。
	p3 := replicaFixture("kube-apiserver", "mirror/api-c", 1)
	p3.ReplicaRegistry = "mcr.microsoft.com"
	p3.ValidFrom = time.Now().Add(time.Hour).Unix()
	if _, err := st.PutRegistryReplica(p3, "admin", true); err != nil {
		t.Fatalf("写入未来生效行: %v", err)
	}
	if got, _ := st.ActiveRegistryReplicas("registry.k8s.io", "kube-apiserver", time.Now()); len(got) != 2 {
		t.Fatalf("未生效副本不应参与，实得 %d", len(got))
	}
}

// 并发写：无 database is locked、代次单调、最终一致（T58 的副本侧）。
func TestRegistryReplicaConcurrentWrites(t *testing.T) {
	st := openReplicaStore(t)
	var wg sync.WaitGroup
	regs := []string{"quay.io", "gcr.io", "mcr.microsoft.com", "public.ecr.aws"}
	errs := make(chan error, len(regs))
	for i, reg := range regs {
		wg.Add(1)
		go func(i int, reg string) {
			defer wg.Done()
			r := replicaFixture("kube-apiserver", "mirror/api", 10+i)
			r.ReplicaRegistry = reg
			if _, err := st.PutRegistryReplica(r, "admin", true); err != nil {
				errs <- err
			}
		}(i, reg)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发写失败: %v", err)
	}
	got, err := st.ListRegistryReplicas("registry.k8s.io")
	if err != nil {
		t.Fatalf("查询: %v", err)
	}
	if len(got) != len(regs) {
		t.Fatalf("最终行数 = %d，期望 %d", len(got), len(regs))
	}
	g, err := st.ReplicaGeneration("registry.k8s.io")
	if err != nil {
		t.Fatalf("代次: %v", err)
	}
	if g < uint64(len(regs)) {
		t.Fatalf("代次 = %d，期望至少 %d", g, len(regs))
	}
}
