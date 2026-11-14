package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
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

// 3.5.0 起的五个归属列必须存在于 Canonical 新建库中。
func TestHostColumnsExist(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "h.db"))
	defer st.Close()
	rows, err := st.db.Query(`PRAGMA table_info(sources)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"repo_host", "registry_cap", "registry_path_mode", "registry_probe_at", "registry_probe_error"} {
		if !got[c] {
			t.Fatalf("缺列 %s", c)
		}
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
	if res, err := st.SetHostCaps(id, "lscr.io", RegistryCapYes, PathNative, 123, "ok", gen, ""); err != nil || res != HostCapsSourceChanged {
		t.Fatalf("先要把源改成 lscr.io 才能写 lscr 能力: res=%v err=%v", res, err)
	}
	if err := st.SetRepoHost(id, "LSCR.IO."); err != nil {
		t.Fatal(err)
	}
	if res, err := st.SetHostCaps(id, "lscr.io", RegistryCapYes, PathNative, 123, "ok", gen, ""); err != nil || res != HostCapsCommitted {
		t.Fatalf("写回失败: res=%v err=%v", res, err)
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

func TestSetRepoHostEmptyClearsOwnership(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "rh-empty.db"))
	defer st.Close()
	id := addRegSource(t, st, "quay.io", "native", 1, true, "usable", 1)

	if err := st.SetRepoHost(id, ""); err != nil {
		t.Fatalf("SetRepoHost(id, \"\"): %v", err)
	}
	var host string
	if err := st.db.QueryRow(`SELECT COALESCE(repo_host,'') FROM sources WHERE id=?`, id).Scan(&host); err != nil {
		t.Fatal(err)
	}
	if host != "" {
		t.Fatalf("清除归属后 repo_host=%q，期望空串", host)
	}
	for _, kind := range []string{"registry:quay.io", "docker", "ghcr"} {
		for _, c := range mustCandidates(t, st, kind) {
			if c.ID == id {
				t.Fatalf("清除归属后源仍出现在 %s 候选池", kind)
			}
		}
	}

	// 显式设置 Docker Hub 必须落 docker.io，而不是复用空串。
	if err := st.SetRepoHost(id, "docker.io"); err != nil {
		t.Fatalf("SetRepoHost(id, docker.io): %v", err)
	}
	if err := st.db.QueryRow(`SELECT repo_host FROM sources WHERE id=?`, id).Scan(&host); err != nil {
		t.Fatal(err)
	}
	if host != "docker.io" {
		t.Fatalf("显式 Hub 后 repo_host=%q，期望 docker.io", host)
	}
	found := false
	for _, c := range mustCandidates(t, st, "docker") {
		if c.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("显式设置 docker.io 后源不在 docker 候选池")
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
	if res, err := st.SetHostCaps(id, "lscr.io", RegistryCapYes, PathNative, 1, "", gen, ""); err != nil || res != HostCapsSourceChanged {
		t.Fatalf("Host 不匹配时应拒绝: res=%v err=%v", res, err)
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
	if res, err := st.SetHostCaps(id, "quay.io", RegistryCapMismatch, "", 42, long, gen, ""); err != nil || res != HostCapsCommitted {
		t.Fatalf("写回失败: res=%v err=%v", res, err)
	}
	x := mustSource(t, st, id)
	if x.RegistryCap != RegistryCapMismatch || x.RegistryPath != "" || x.RegistryProbeAt != 42 {
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
	if x := mustSource(t, st, id); x.RegistryCap != RegistryCapUnknown || x.RegistryPath != "" {
		t.Fatalf("Bump 后应清空能力: %+v", x)
	} else if x.RegistryProbeError != strings.Repeat("错", 200) {
		t.Fatalf("Bump 不应抹掉诊断: %d rune", len([]rune(x.RegistryProbeError)))
	}
	if res, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 50, "", gen, ""); err != nil || res != HostCapsStaleSnapshot {
		t.Fatalf("旧代次结果不应写入: res=%v err=%v", res, err)
	}
	if res, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 51, "", gen2, ""); err != nil || res != HostCapsCommitted {
		t.Fatalf("新代次结果应写入: res=%v err=%v", res, err)
	}

	// 归档后连正确的写入也要拒绝。
	mustExec(t, st, `UPDATE sources SET status='archived' WHERE id=?`, id)
	if res, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 52, "", gen2, ""); err != nil || res != HostCapsSourceUnavailable {
		t.Fatalf("归档源不应被写入: res=%v err=%v", res, err)
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
			if res, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, int64(i), "", gen, ""); err != nil || res != HostCapsCommitted {
				t.Errorf("并发写失败: res=%v err=%v", res, err)
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
			res, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, int64(100+i), "", gen, "")
			if err != nil {
				t.Errorf("旧代次写入报错: %v", err)
			}
			if res != HostCapsStaleSnapshot {
				t.Errorf("旧代次应归类 stale_snapshot，得到 %v", res)
			}
		}(i)
	}
	wg.Wait()
	if x := mustSource(t, st, id); x.RegistryCap != RegistryCapUnknown {
		t.Fatalf("旧代次写入污染了结果: %+v", x)
	}
	if res, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 200, "", gen2, ""); err != nil || res != HostCapsCommitted {
		t.Fatalf("新代次写入失败: res=%v err=%v", res, err)
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
	if res, err := st.SetHostCaps(quay, "quay.io", RegistryCapYes, PathNative, 9, "", gen, ""); err != nil || res != HostCapsCommitted {
		t.Fatalf("写回失败: %v %v", res, err)
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

// 写库形状：cap=1 必须带合法路径，其余必须不带。这是 Probe 聚合之外的
// 第二道闸，防止调用方传错组合把脏状态写进去。
func TestSetHostCapsRejectsBadShape(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "shape.db"))
	defer st.Close()
	id, err := st.AddSource(Source{Name: "s", BaseURL: "https://shape.example", Status: "usable", RepoHost: "quay.io"})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := st.HostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	bad := []struct {
		cap  int
		mode string
	}{
		{RegistryCapYes, ""},
		{RegistryCapYes, "weird"},
		{RegistryCapNo, PathNative},
		{RegistryCapMismatch, PathHostPrefix},
		{RegistryCapUnknown, PathNative},
	}
	for _, c := range bad {
		if _, err := st.SetHostCaps(id, "quay.io", c.cap, c.mode, 1, "", gen, ""); err == nil {
			t.Fatalf("cap=%d mode=%q 应被拒绝", c.cap, c.mode)
		}
	}
	if x := mustSource(t, st, id); x.RegistryCap != RegistryCapUnknown {
		t.Fatalf("非法组合不应落库: %+v", x)
	}
}

func TestSetHostCapsMissingSource(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "miss.db"))
	defer st.Close()
	res, err := st.SetHostCaps(9999, "quay.io", RegistryCapYes, PathNative, 1, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if res != HostCapsSourceUnavailable {
		t.Fatalf("已删除的源应归类 source_unavailable，得到 %v", res)
	}
}

func TestClipProbeNote(t *testing.T) {
	long := strings.Repeat("错", 500)
	if got := clipProbeNote(" \n" + long + "\t"); len([]rune(got)) != probeNoteMaxRunes {
		t.Fatalf("旧文本应截到 %d rune，得到 %d", probeNoteMaxRunes, len([]rune(got)))
	}

	diag := `{"version":1,"native":{"cap":1},"host_prefix":{"cap":0}}`
	if got := clipProbeNote("  \n" + diag + "\n "); got != diag {
		t.Fatalf("结构化诊断应原样保留: %q", got)
	}

	big := `{"version":1,"native":{"error":"` + strings.Repeat("x", probeNoteMaxBytes) + `"}}`
	got := clipProbeNote(big)
	if isDiagnosticsJSON(got) {
		t.Fatal("超限诊断不能被截成残缺 JSON 落库")
	}
	if len(got) > probeNoteMaxBytes {
		t.Fatalf("兜底文本也应短: %d", len(got))
	}

	var notDiag struct{ Version int }
	if json.Unmarshal([]byte(`{"error":"boom"}`), &notDiag); notDiag.Version != 0 {
		t.Fatal("测试前提错误")
	}
	if got := clipProbeNote(`{"error":"boom"}`); got != `{"error":"boom"}` {
		t.Fatalf("无版本字段的普通文本按旧规则处理: %q", got)
	}
}

// 清能力必须带条件：对账读到的 (id, host, cap) 变了就不能写回。
func TestResetHostCapsConditional(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "r.db"))
	defer st.Close()
	id, err := st.AddSource(Source{Name: "s", BaseURL: "https://r.example", Status: "usable", RepoHost: "quay.io"})
	if err != nil {
		t.Fatal(err)
	}
	gen, _, err := st.HostGenState("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := st.SetHostCaps(id, "quay.io", RegistryCapYes, PathNative, 100, "diag", gen, ""); err != nil || res != HostCapsCommitted {
		t.Fatalf("seed: %v %v", res, err)
	}
	if ok, err := st.ResetHostCaps(id, "quay.io", RegistryCapMismatch); err != nil || ok {
		t.Fatalf("cap 不符不应写回: ok=%v err=%v", ok, err)
	}
	if x := mustSource(t, st, id); x.RegistryCap != RegistryCapYes {
		t.Fatalf("不应误清: %+v", x)
	}
	if ok, err := st.ResetHostCaps(id, "other.example", RegistryCapYes); err != nil || ok {
		t.Fatalf("host 不符不应写回: ok=%v err=%v", ok, err)
	}
	if ok, err := st.ResetHostCaps(id, "quay.io", RegistryCapYes); err != nil || !ok {
		t.Fatalf("条件一致应清能力: ok=%v err=%v", ok, err)
	}
	x := mustSource(t, st, id)
	if x.RegistryCap != RegistryCapUnknown || x.RegistryPath != "" || x.RegistryProbeError != "diag" || x.RegistryProbeAt != 100 {
		t.Fatalf("清理结果不对: %+v", x)
	}
}
