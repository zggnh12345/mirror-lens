package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

func openTestAt(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// 3.5.0 的五个列必须同时存在于新建库与老库补列两条路径上。
func TestHostColumnsExist(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "h.db"))
	defer st.Close()
	cols, err := st.columns("sources")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"repo_host", "registry_cap", "registry_path_mode", "registry_probe_at", "registry_probe_error"} {
		if !cols[c] {
			t.Fatalf("缺列 %s", c)
		}
	}
}

// V1 库升级：重建表后必须补上新列，且原有数据不丢。
func TestMigrationFromV1KeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE sources (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  base_url TEXT NOT NULL,
		  name TEXT NOT NULL,
		  kind TEXT NOT NULL,
		  origin TEXT NOT NULL DEFAULT 'manual',
		  enabled INTEGER NOT NULL DEFAULT 1,
		  status TEXT NOT NULL DEFAULT 'pending',
		  note TEXT NOT NULL DEFAULT '',
		  created_at INTEGER NOT NULL,
		  UNIQUE(base_url, kind)
		)`,
		`INSERT INTO sources(id,base_url,name,kind,origin,enabled,status,note,created_at)
		 VALUES(7,'https://old.example','old','docker','seed',1,'usable','n',1000)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st := openTestAt(t, path)
	defer st.Close()

	x, err := st.GetSource(7)
	if err != nil {
		t.Fatalf("V1 升级后取源失败: %v", err)
	}
	if x.BaseURL != "https://old.example" || x.Name != "old" || x.Status != "usable" {
		t.Fatalf("V1 升级丢了字段: %+v", x)
	}
	if x.RepoHost != "" || x.RegistryCap != CapUnknown || x.RegistryPath != "" {
		t.Fatalf("新列默认值不对: %+v", x)
	}
	// 迁移必须可重复：再开一次不报错、不改变结果。
	st.Close()
	st2 := openTestAt(t, path)
	defer st2.Close()
	y, err := st2.GetSource(7)
	if err != nil {
		t.Fatal(err)
	}
	if !sameCore(t, *x, *y) {
		t.Fatalf("重复迁移改变了数据:\n%+v\n%+v", x, y)
	}
}

// GHCR 回填：只回填纯 ghcr 源，双能力源保持 Hub 归属，旧列一律不动。
func TestBackfillRepoHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bf.db")
	st := openTestAt(t, path)

	pure, err := st.AddSource(Source{Name: "pure-ghcr", BaseURL: "https://g1.example", Status: "usable"})
	if err != nil {
		t.Fatal(err)
	}
	dual, err := st.AddSource(Source{Name: "dual", BaseURL: "https://g2.example", Status: "usable"})
	if err != nil {
		t.Fatal(err)
	}
	arch, err := st.AddSource(Source{Name: "arch", BaseURL: "https://g3.example", Status: "archived"})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 3.4.6 探测写回的结果：纯 ghcr 与「同时代 Hub」的源。
	mustExec(t, st, `UPDATE sources SET ghcr_cap=1, ghcr_prefix=1 WHERE id=?`, pure)
	mustExec(t, st, `UPDATE sources SET ghcr_cap=1, ghcr_prefix=0, docker_cap=1 WHERE id=?`, dual)
	mustExec(t, st, `UPDATE sources SET ghcr_cap=1, ghcr_prefix=1 WHERE id=?`, arch)
	st.Close()

	st = openTestAt(t, path)
	defer st.Close()

	x := mustSource(t, st, pure)
	if x.RepoHost != "ghcr.io" || x.RegistryCap != RegistryCapYes || x.RegistryPath != PathHostPrefix {
		t.Fatalf("纯 ghcr 源回填不对: %+v", x)
	}
	// 旧列必须原样保留：3.5.1 之前 ghcr 调度仍读它们。
	if x.GhcrCap != 1 || !x.GhcrPrefix {
		t.Fatalf("回填改动了旧列: %+v", x)
	}

	d := mustSource(t, st, dual)
	if d.RepoHost != "" || d.RegistryCap != CapUnknown {
		t.Fatalf("双能力源不应被改成 ghcr 归属: %+v", d)
	}
	if d.DockerCap != 1 || d.GhcrCap != 1 {
		t.Fatalf("双能力源的旧列被改动: %+v", d)
	}

	a := mustSource(t, st, arch)
	if a.RepoHost != "" || a.RegistryCap != CapUnknown {
		t.Fatalf("归档源不应回填: %+v", a)
	}

	// 幂等：再开一次，值不变。
	st.Close()
	st = openTestAt(t, path)
	defer st.Close()
	if got := mustSource(t, st, pure); !sameCore(t, got, x) {
		t.Fatalf("重复回填改变了结果:\n%+v\n%+v", x, got)
	}
}

// 阶段 1 建的库没有 cred_digest 列，升级时必须补上且不丢原有 gen。
func TestRegistryHostGenGetsDigestColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gen.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE registry_host_gen (
		host TEXT PRIMARY KEY, gen INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO registry_host_gen(host,gen,updated_at) VALUES('quay.io',7,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st := openTestAt(t, path)
	defer st.Close()
	gen, digest, err := st.HostGenState("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if gen != 7 || digest != "" {
		t.Fatalf("升级后应保留 gen=7、摘要为空，得到 gen=%d digest=%q", gen, digest)
	}
}

func TestSetRepoHostClearsCaps(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "rh.db"))
	defer st.Close()

	id, err := st.AddSource(Source{Name: "s", BaseURL: "https://s.example", Status: "usable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRepoHost(id, "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	gen, err := st.BumpHostGen("lscr.io")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetHostCaps(id, "lscr.io", RegistryCapYes, PathNative, 123, "ok", gen, ""); err != nil || ok {
		t.Fatalf("先要把源改成 lscr.io 才能写 lscr 能力: ok=%v err=%v", ok, err)
	}
	if err := st.SetRepoHost(id, "LSCR.IO."); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetHostCaps(id, "lscr.io", RegistryCapYes, PathNative, 123, "ok", gen, ""); err != nil || !ok {
		t.Fatalf("写回失败: ok=%v err=%v", ok, err)
	}
	// 改归属必须清空旧结论。
	if err := st.SetRepoHost(id, "quay.io"); err != nil {
		t.Fatal(err)
	}
	x := mustSource(t, st, id)
	if x.RepoHost != "quay.io" || x.RegistryCap != RegistryCapUnknown || x.RegistryPath != "" ||
		x.RegistryProbeAt != 0 || x.RegistryProbeError != "" {
		t.Fatalf("改归属未清理能力: %+v", x)
	}
}

func TestSetRepoHostRejectsBadHostAndArchived(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "rh2.db"))
	defer st.Close()
	id, err := st.AddSource(Source{Name: "s", BaseURL: "https://s2.example", Status: "archived"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRepoHost(id, "https://ghcr.io/x"); err == nil {
		t.Fatal("非法 Host 应报错")
	}
	if err := st.SetRepoHost(id, "nvcr.io"); err == nil {
		t.Fatal("未入白名单的 Host 应报错")
	}
	if err := st.SetRepoHost(id, "quay.io"); err != ErrArchived {
		t.Fatalf("归档源应拒绝修改，得到 %v", err)
	}
	if err := st.SetRepoHost(99999, "quay.io"); err != ErrSourceNotFound {
		t.Fatalf("不存在的源应报 ErrSourceNotFound，得到 %v", err)
	}
}

func TestSetHostCapsGuards(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "hc.db"))
	defer st.Close()

	id, err := st.AddSource(Source{Name: "s", BaseURL: "https://s3.example", Status: "usable", RepoHost: "quay.io"})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := st.HostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}

	// 错误的 Host：用户中途改了归属，旧结果不能落库。
	if ok, err := st.SetHostCaps(id, "lscr.io", RegistryCapYes, PathNative, 1, "", gen, ""); err != nil || ok {
		t.Fatalf("Host 不匹配时应拒绝: ok=%v err=%v", ok, err)
	}
	// Hub / ghcr 不接受 Host Probe 结果。
	if _, err := st.SetHostCaps(id, "", RegistryCapYes, PathNative, 1, "", gen, ""); err == nil {
		t.Fatal("Hub 应拒绝 SetHostCaps")
	}
	// 非法能力值。
	if _, err := st.SetHostCaps(id, "quay.io", 3, PathNative, 1, "", gen, ""); err == nil {
		t.Fatal("cap=3 应报错")
	}
	if _, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, "weird", 1, "", gen, ""); err == nil {
		t.Fatal("非法路径模式应报错")
	}

	// 正常写入 + 错误文本按 rune 截断。
	long := ""
	for i := 0; i < 500; i++ {
		long += "错"
	}
	if ok, err := st.SetHostCaps(id, "quay.io", RegistryCapMismatch, PathHostPrefix, 42, long, gen, ""); err != nil || !ok {
		t.Fatalf("写回失败: ok=%v err=%v", ok, err)
	}
	x := mustSource(t, st, id)
	if x.RegistryCap != RegistryCapMismatch || x.RegistryPath != PathHostPrefix || x.RegistryProbeAt != 42 {
		t.Fatalf("写回结果不对: %+v", x)
	}
	if n := len([]rune(x.RegistryProbeError)); n != 200 {
		t.Fatalf("错误文本应截断到 200 rune，得到 %d", n)
	}

	// 凭据代次变更后，旧代次的结果必须被丢弃。
	gen2, err := st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if gen2 != gen+1 {
		t.Fatalf("代次应 +1：%d -> %d", gen, gen2)
	}
	if x := mustSource(t, st, id); x.RegistryCap != RegistryCapUnknown || x.RegistryProbeError != "" {
		t.Fatalf("Bump 后应清空能力: %+v", x)
	}
	if ok, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 50, "", gen, ""); err != nil || ok {
		t.Fatalf("旧代次结果不应写入: ok=%v err=%v", ok, err)
	}
	if ok, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 51, "", gen2, ""); err != nil || !ok {
		t.Fatalf("新代次结果应写入: ok=%v err=%v", ok, err)
	}

	// 归档后连正确的写入也要拒绝。
	mustExec(t, st, `UPDATE sources SET status='archived' WHERE id=?`, id)
	if ok, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 52, "", gen2, ""); err != nil || ok {
		t.Fatalf("归档源不应被写入: ok=%v err=%v", ok, err)
	}
}

// 并发：同一代次下多个写者不能写坏数据；旧代次写者在 Bump 之后必须全部落空。
func TestSetHostCapsConcurrentGenerations(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "cc.db"))
	defer st.Close()

	id, err := st.AddSource(Source{Name: "c", BaseURL: "https://c.example", Status: "usable", RepoHost: "quay.io"})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := st.HostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ok, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, int64(i), "", gen, ""); err != nil || !ok {
				t.Errorf("并发写失败: ok=%v err=%v", ok, err)
			}
		}(i)
	}
	wg.Wait()

	gen2, err := st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, int64(100+i), "", gen, "")
			if err != nil {
				t.Errorf("旧代次写入报错: %v", err)
			}
			if ok {
				t.Errorf("旧代次写入不应成功")
			}
		}(i)
	}
	wg.Wait()
	if x := mustSource(t, st, id); x.RegistryCap != RegistryCapUnknown {
		t.Fatalf("旧代次写入污染了结果: %+v", x)
	}
	if ok, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 200, "", gen2, ""); err != nil || !ok {
		t.Fatalf("新代次写入失败: ok=%v err=%v", ok, err)
	}
}

// ListHostCaps 只暴露新 Host：Hub、ghcr、归档源都不出现。
func TestListHostCapsScope(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "lh.db"))
	defer st.Close()

	add := func(name, host, status string) int64 {
		t.Helper()
		id, err := st.AddSource(Source{Name: name, BaseURL: "https://" + name + ".example", Status: status, RepoHost: host})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	hub := add("hub", "", "usable")
	ghcr := add("ghcr", "ghcr.io", "usable")
	quay := add("quay", "quay.io", "usable")
	arch := add("arch", "lscr.io", "archived")
	_ = hub
	_ = ghcr

	list, err := st.ListHostCaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SourceID != quay || list[0].RepoHost != "quay.io" {
		t.Fatalf("列表范围不对: %+v", list)
	}
	pending, err := st.ListPendingHostCaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SourceID != quay {
		t.Fatalf("待探测列表应包含未验证的 quay 源: %+v", pending)
	}
	gen, err := st.HostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetHostCaps(quay, "quay.io", RegistryCapYes, PathNative, 9, "", gen, ""); err != nil || !ok {
		t.Fatalf("写回失败: %v %v", ok, err)
	}
	pending, err = st.ListPendingHostCaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("已验证的源不应再进待探测队列: %+v", pending)
	}
	for _, x := range list {
		if x.SourceID == arch {
			t.Fatal("归档源不应出现")
		}
	}
}

// sameCore 只比迁移关心的字段：Last/Recent 之类是运行时聚合出来的，
// nil 与空切片在 JSON 里不同（null vs []），拿来判「迁移是否改变数据」会误报。
func sameCore(t *testing.T, a, b Source) bool {
	t.Helper()
	return a.BaseURL == b.BaseURL && a.Name == b.Name && a.Status == b.Status &&
		a.StatusAt == b.StatusAt && a.DockerCap == b.DockerCap && a.GhcrCap == b.GhcrCap &&
		a.GhcrPrefix == b.GhcrPrefix && a.RepoHost == b.RepoHost &&
		a.RegistryCap == b.RegistryCap && a.RegistryPath == b.RegistryPath
}

func mustExec(t *testing.T, st *Store, q string, args ...any) {
	t.Helper()
	if _, err := st.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func mustSource(t *testing.T, st *Store, id int64) Source {
	t.Helper()
	x, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	return *x
}
