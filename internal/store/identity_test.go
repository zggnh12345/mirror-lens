package store

import (
	"path/filepath"
	"testing"
	"time"
)

func insRaw(t *testing.T, st *Store, url, host string, dc, gc, ghc int) int64 {
	t.Helper()
	res, err := st.db.Exec(`INSERT INTO sources(base_url,name,created_at,repo_host,docker_cap,ghcr_cap,github_cap)
		VALUES(?,?,?,?,?,?,?)`, url, url, time.Now().Unix(), host, dc, gc, ghc)
	if err != nil {
		t.Fatalf("insert %s: %v", url, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func sourceIdentity(t *testing.T, st *Store, id int64) (host, identity string) {
	t.Helper()
	if err := st.db.QueryRow(`SELECT repo_host, registry_identity_state FROM sources WHERE id=?`, id).
		Scan(&host, &identity); err != nil {
		t.Fatalf("read %d: %v", id, err)
	}
	return
}

// 3.5.4 迁移：只处理 docker_cap=1 与 github_cap=1，其余保持 unclassified。
func TestRegistryIdentityMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hub := insRaw(t, st, "https://hub.example", "", 1, 0, 0)
	dual := insRaw(t, st, "https://dual.example", "", 1, 1, 0)
	gh := insRaw(t, st, "https://gh.example", "", -1, 0, 1)
	unk := insRaw(t, st, "https://unk.example", "", -1, 0, 0)
	zero := insRaw(t, st, "https://zero.example", "", 0, 0, 0)

	// 回到迁移前状态：清掉标记后重开。
	if _, err := st.db.Exec(`DELETE FROM schema_migrations WHERE name='3.5.4-registry-identity'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE sources SET repo_host='', registry_identity_state='unclassified'`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cases := []struct {
		id   int64
		host string
		idnt string
	}{
		{hub, "docker.io", IdentityIdentified},
		{dual, "docker.io", IdentityIdentified},
		{gh, "", IdentityNotApplicable},
		{unk, "", IdentityUnclassified},
		{zero, "", IdentityUnclassified},
	}
	for _, c := range cases {
		h, i := sourceIdentity(t, st, c.id)
		if h != c.host || i != c.idnt {
			t.Errorf("id=%d got (%q,%q) want (%q,%q)", c.id, h, i, c.host, c.idnt)
		}
	}

	// 能力字段不得被迁移改写。
	var dc, gc, ghc int
	if err := st.db.QueryRow(`SELECT docker_cap, ghcr_cap, github_cap FROM sources WHERE id=?`, dual).
		Scan(&dc, &gc, &ghc); err != nil {
		t.Fatal(err)
	}
	if dc != 1 || gc != 1 || ghc != 0 {
		t.Errorf("迁移改动了能力字段: %d/%d/%d", dc, gc, ghc)
	}
}

func TestHostCapsIdentityGate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "caps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	unclassified := insRaw(t, st, "https://u.example", "quay.io", -1, 0, 0)
	known := insRaw(t, st, "https://k.example", "gcr.io", -1, 0, 0)
	if _, err := st.db.Exec(`UPDATE sources SET registry_identity_state='identified' WHERE id=?`, known); err != nil {
		t.Fatal(err)
	}
	hub := insRaw(t, st, "https://h.example", "docker.io", -1, 0, 0)
	ghcr := insRaw(t, st, "https://g.example", "ghcr.io", -1, 0, 0)
	if _, err := st.db.Exec(`UPDATE sources SET registry_identity_state='identified' WHERE id IN (?,?)`, hub, ghcr); err != nil {
		t.Fatal(err)
	}

	caps, err := st.ListHostCaps()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, c := range caps {
		seen[c.SourceID] = true
	}
	if seen[unclassified] {
		t.Error("unclassified 源出现在 Host 面板")
	}
	if seen[hub] || seen[ghcr] {
		t.Error("docker.io/ghcr.io 出现在 Host 面板")
	}
	if !seen[known] {
		t.Error("identified 的 foreign Host 未出现在 Host 面板")
	}
}

// 未确认身份的源不得进入 Host Probe；docker.io 与空 Host 同样排除。
func TestListDueHostProbesIdentityGate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "due.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	unknown := insRaw(t, st, "https://u.example", "quay.io", -1, 0, 0)
	known := insRaw(t, st, "https://k.example", "quay.io", -1, 0, 0)
	if _, err := st.db.Exec(`UPDATE sources SET registry_identity_state='identified' WHERE id=?`, known); err != nil {
		t.Fatal(err)
	}
	hub := insRaw(t, st, "https://hub.example", "docker.io", -1, 0, 0)
	if _, err := st.db.Exec(`UPDATE sources SET registry_identity_state='identified' WHERE id=?`, hub); err != nil {
		t.Fatal(err)
	}
	empty := insRaw(t, st, "https://e.example", "", -1, 0, 0)
	if _, err := st.db.Exec(`UPDATE sources SET registry_identity_state='identified' WHERE id=?`, empty); err != nil {
		t.Fatal(err)
	}

	due, err := st.ListDueHostProbes(time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, d := range due {
		got[d.SourceID] = true
	}
	if got[unknown] {
		t.Error("unclassified 源进入了 Host Probe")
	}
	if got[hub] || got[empty] {
		t.Error("docker.io/空 Host 进入了 Host Probe")
	}
	if !got[known] {
		t.Error("identified 的 foreign Host 未进入 Probe")
	}
}

// SetRepoHost 必须在同一事务里把身份改为 identified。
func TestSetRepoHostWritesIdentity(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "set.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id := insRaw(t, st, "https://s.example", "", -1, 0, 0)
	if err := st.SetRepoHost(id, "quay.io"); err != nil {
		t.Fatal(err)
	}
	h, i := sourceIdentity(t, st, id)
	if h != "quay.io" || i != IdentityIdentified {
		t.Errorf("SetRepoHost(quay.io) -> (%q,%q)", h, i)
	}

	if err := st.SetRepoHost(id, ""); err != nil {
		t.Fatal(err)
	}
	h, i = sourceIdentity(t, st, id)
	if h != "docker.io" || i != IdentityIdentified {
		t.Errorf("SetRepoHost(Hub) -> (%q,%q)", h, i)
	}
}

// AddSource 只在有证据时映射 Hub；无证据保持 unclassified。
func TestAddSourceIdentity(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "add.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hubID, err := st.AddSource(Source{BaseURL: "https://a.example", Name: "a", DockerCap: RegistryCapYes})
	if err != nil {
		t.Fatal(err)
	}
	h, i := sourceIdentity(t, st, hubID)
	if h != "docker.io" || i != IdentityIdentified {
		t.Errorf("docker 能力源 -> (%q,%q)", h, i)
	}

	ghID, err := st.AddSource(Source{BaseURL: "https://b.example", Name: "b", GithubCap: RegistryCapYes})
	if err != nil {
		t.Fatal(err)
	}
	h, i = sourceIdentity(t, st, ghID)
	if h != "" || i != IdentityNotApplicable {
		t.Errorf("GitHub 源 -> (%q,%q)", h, i)
	}

	unkID, err := st.AddSource(Source{BaseURL: "https://c.example", Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	h, i = sourceIdentity(t, st, unkID)
	if h != "" || i != IdentityUnclassified {
		t.Errorf("未知源 -> (%q,%q)", h, i)
	}

	ghcrID, err := st.AddSource(Source{BaseURL: "https://d.example", Name: "d", GhcrCap: RegistryCapYes, DockerCap: RegistryCapNo})
	if err != nil {
		t.Fatal(err)
	}
	h, i = sourceIdentity(t, st, ghcrID)
	if h != "ghcr.io" || i != IdentityIdentified {
		t.Errorf("ghcr 能力源 -> (%q,%q)", h, i)
	}
}
