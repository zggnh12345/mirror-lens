package store

import (
	"fmt"
	"sync/atomic"
	"testing"
)

var regSeq atomic.Int64

// addRegSource 建一台只服务某个 Registry Host 的源。
// dockerCap 故意可调：用于证明 registry 候选不靠 docker_cap 间接放行。
func addRegSource(t *testing.T, st *Store, host, path string, cap int, enabled bool, status string, dockerCap int) int64 {
	t.Helper()
	n := regSeq.Add(1)
	id, err := st.AddSource(Source{
		Name:      fmt.Sprintf("reg-%s-%d-%d", host, cap, n),
		BaseURL:   fmt.Sprintf("https://reg-%d.example", n),
		Status:    status,
		Enabled:   enabled,
		DockerCap: dockerCap,
		RepoHost:  host,
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE sources SET repo_host=?, registry_cap=?, registry_path_mode=? WHERE id=?`,
		host, cap, path, id); err != nil {
		t.Fatalf("update registry cols: %v", err)
	}
	// cap=1 且路径合法的源代表「已经探测通过」：候选准入还要求 recovery
	// 行是 ok 且成功结论未过期（C5 五重约束），所以这里补一条。
	if cap == 1 && (path == "native" || path == "host_prefix") {
		now := st.nowTime().Unix()
		if _, err := st.db.Exec(`INSERT INTO registry_host_recovery
			(source_id, family, scope, state, gen, last_success_at, next_probe_at, updated_at)
			VALUES(?, 'registry', ?, 'ok', 0, ?, 0, ?)
			ON CONFLICT(source_id, family, scope) DO UPDATE SET
			state='ok', gen=0, last_success_at=excluded.last_success_at`,
			id, host, now, now); err != nil {
			t.Fatalf("seed recovery: %v", err)
		}
	}
	return id
}

func candidatesIDs(list []SourceStat) map[int64]string {
	m := make(map[int64]string, len(list))
	for _, s := range list {
		m[s.ID] = s.RegistryPath
	}
	return m
}

func TestRegistryCandidatesOnlyVerifiedPaths(t *testing.T) {
	st, _ := newTestStore(t)

	okNative := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	okPrefix := addRegSource(t, st, "quay.io", "host_prefix", 1, true, "degraded", 0)
	// 不靠 docker_cap 放行：这台 docker_cap=1，但 registry 条件同样成立，应入选。
	okWithDocker := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 1)

	badCap2 := addRegSource(t, st, "quay.io", "native", 2, true, "usable", 0)
	badCap0 := addRegSource(t, st, "quay.io", "native", 0, true, "usable", 0)
	badCapMinus1 := addRegSource(t, st, "quay.io", "native", -1, true, "usable", 0)
	badEmptyPath := addRegSource(t, st, "quay.io", "", 1, true, "usable", 0)
	badUnknownPath := addRegSource(t, st, "quay.io", "weird", 1, true, "usable", 0)
	badOtherHost := addRegSource(t, st, "registry.k8s.io", "native", 1, true, "usable", 0)
	badDisabled := addRegSource(t, st, "quay.io", "native", 1, false, "usable", 0)
	badStatus := addRegSource(t, st, "quay.io", "native", 1, true, "blocked", 0)

	list, err := st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatalf("SourceCandidates: %v", err)
	}
	got := candidatesIDs(list)

	for _, id := range []int64{okNative, okPrefix, okWithDocker} {
		if _, ok := got[id]; !ok {
			t.Errorf("源 %d 应进入 quay.io 候选，实际没有", id)
		}
	}
	if got[okNative] != "native" {
		t.Errorf("native 源路径=%q，期望 native", got[okNative])
	}
	if got[okPrefix] != "host_prefix" {
		t.Errorf("host_prefix 源路径=%q，期望 host_prefix", got[okPrefix])
	}
	for name, id := range map[string]int64{
		"cap=2": badCap2, "cap=0": badCap0, "cap=-1": badCapMinus1,
		"空路径": badEmptyPath, "未知路径": badUnknownPath,
		"其他 Host": badOtherHost, "未启用": badDisabled, "状态不可用": badStatus,
	} {
		if _, ok := got[id]; ok {
			t.Errorf("%s 的源 %d 不应进入候选", name, id)
		}
	}
	if len(list) != 3 {
		t.Errorf("候选数=%d，期望 3（%v）", len(list), got)
	}
}

// 读路径必须自己挡住身份不明的源，不能只依赖写入端不变量。
func TestRegistryCandidatesRequireIdentified(t *testing.T) {
	st, _ := newTestStore(t)
	ok := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	bad := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	if _, err := st.db.Exec(`UPDATE sources SET registry_identity_state='unclassified' WHERE id=?`, bad); err != nil {
		t.Fatal(err)
	}
	na := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	if _, err := st.db.Exec(`UPDATE sources SET registry_identity_state='not_applicable' WHERE id=?`, na); err != nil {
		t.Fatal(err)
	}

	m := candidatesIDs(mustCandidates(t, st, "registry:quay.io"))
	if _, got := m[ok]; !got {
		t.Error("identified foreign Registry 源应进入候选")
	}
	for name, id := range map[string]int64{"unclassified": bad, "not_applicable": na} {
		if _, got := m[id]; got {
			t.Errorf("%s 的源 %d 不应进入 Registry 候选", name, id)
		}
	}
}

// Hub/GHCR 即使被构造成「cap=1 + recovery ok」，也不能进入 foreign Registry 池。
func TestRegistryCandidatesExcludeHubAndGHCR(t *testing.T) {
	st, _ := newTestStore(t)
	addRegSource(t, st, "docker.io", "native", 1, true, "usable", 0)
	addRegSource(t, st, "ghcr.io", "native", 1, true, "usable", 0)
	for _, kind := range []string{"registry:docker.io", "registry:ghcr.io"} {
		if got := mustCandidates(t, st, kind); len(got) != 0 {
			t.Errorf("%s 候选数=%d，期望 0", kind, len(got))
		}
	}
	// 直接读路径也不得被绕过。
	for _, host := range []string{"", "docker.io", "ghcr.io"} {
		got, err := st.RegistryCandidatesRaw(host)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("RegistryCandidatesRaw(%q) 返回 %d 条，期望 0", host, len(got))
		}
	}
}

// 不同 Host 的候选池必须互不可见。
func TestRegistryCandidatesHostIsolation(t *testing.T) {
	st, _ := newTestStore(t)
	a := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	b := addRegSource(t, st, "registry.k8s.io", "host_prefix", 1, true, "usable", 0)

	qa, err := st.SourceCandidates("registry:quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidatesIDs(qa)[b]; ok {
		t.Error("quay.io 候选里出现了 registry.k8s.io 的源")
	}
	qb, err := st.SourceCandidates("registry:registry.k8s.io")
	if err != nil {
		t.Fatal(err)
	}
	m := candidatesIDs(qb)
	if len(m) != 1 {
		t.Fatalf("registry.k8s.io 候选数=%d，期望 1", len(m))
	}
	if _, ok := m[a]; ok {
		t.Error("registry.k8s.io 候选里出现了 quay.io 的源")
	}
	if m[b] != "host_prefix" {
		t.Errorf("路径=%q，期望 host_prefix", m[b])
	}
}

// 非白名单/空 Host 直接返回空，不报错也不放宽。
func TestRegistryCandidatesUnknownHost(t *testing.T) {
	st, _ := newTestStore(t)
	for _, kind := range []string{"registry:", "registry:example.com", "registry:quay.io.evil"} {
		list, err := st.SourceCandidates(kind)
		if err != nil {
			t.Errorf("%q 不应报错: %v", kind, err)
		}
		if len(list) != 0 {
			t.Errorf("%q 应返回空，实际 %d 条", kind, len(list))
		}
	}
}

// docker 候选不能被 registry 源污染：registry_cap=1 但 docker_cap=0 的源不能进 docker 池。
func TestRegistrySourceNotInDockerCandidates(t *testing.T) {
	st, _ := newTestStore(t)
	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 0)
	for _, c := range mustCandidates(t, st, "docker") {
		if c.ID == id {
			t.Error("registry-only 源出现在了 docker 候选里")
		}
	}
}

func mustCandidates(t *testing.T, st *Store, kind string) []SourceStat {
	t.Helper()
	list, err := st.SourceCandidates(kind)
	if err != nil {
		t.Fatalf("SourceCandidates(%q): %v", kind, err)
	}
	return list
}

func TestRegistryFamilyAndSampleKind(t *testing.T) {
	for _, kind := range []string{"registry:quay.io", "registry:registry.k8s.io"} {
		if got := Family(kind); got != "registry" {
			t.Errorf("Family(%q)=%q，期望 registry", kind, got)
		}
		if got := sampleKind(kind); got != "registry" {
			t.Errorf("sampleKind(%q)=%q，期望 registry", kind, got)
		}
	}
	// 旧协议族不受影响。
	for kind, want := range map[string]string{
		"docker": "docker", "ghcr": "ghcr", "github": "github", "raw": "github", "git": "git",
	} {
		if got := Family(kind); got != want {
			t.Errorf("Family(%q)=%q，期望 %q", kind, got, want)
		}
	}
	if got := Family("registry"); got != "registry" {
		t.Errorf("Family(registry)=%q，期望 registry（不得落进 github）", got)
	}
}
