package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Canonical Schema 的核心不变量：新库、旧库升级后必须是同一份 schema。
// 这里只验证运行时入口：空库自动建 Canonical，二次打开仍然一致，且库里
// 不再有 registry_identity_state 或持久化的 _writable。
func TestCanonicalFreshAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror-lens.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("首次打开：%v", err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM pragma_table_info('sources') WHERE name='registry_identity_state'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sources 仍含 registry_identity_state 列")
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='_writable'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("_writable 测试表未被删除")
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Canonical 库不应保留 schema_migrations")
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开：%v", err)
	}
	defer st2.Close()
}

func TestAddSourceCanonicalHostSemantics(t *testing.T) {
	st, _ := newTestStore(t)
	defer st.Close()

	cases := []struct {
		name  string
		in    Source
		host  string
		ident string
	}{
		{"github-only", Source{Name: "gh", BaseURL: "https://github.com/a/b", GithubCap: CapYes, DockerCap: CapNo}, "", IdentityNotApplicable},
		{"ghcr", Source{Name: "ghcr", BaseURL: "https://ghcr.io/a/b", GhcrCap: RegistryCapYes, DockerCap: CapNo}, "ghcr.io", IdentityIdentified},
		{"unknown-stays-unclassified", Source{Name: "hub", BaseURL: "https://hub.example", DockerCap: CapUnknown, GithubCap: CapUnknown}, "", IdentityUnclassified},
		{"explicit", Source{Name: "quay", BaseURL: "https://quay.example", RepoHost: "quay.io"}, "quay.io", IdentityIdentified},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, err := st.AddSource(c.in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.GetSource(id)
			if err != nil {
				t.Fatal(err)
			}
			if got.RepoHost != c.host {
				t.Errorf("repo_host=%q，期望 %q", got.RepoHost, c.host)
			}
			if got.RegistryIdentity != c.ident {
				t.Errorf("identity=%q，期望 %q", got.RegistryIdentity, c.ident)
			}
		})
	}
}

// 空 Host 只表示「没有 Registry 归属」，不能因为 docker_cap=1 被当成 Hub。
func TestEmptyHostStaysUnclassified(t *testing.T) {
	st, _ := newTestStore(t)
	defer st.Close()
	res, err := st.db.Exec(`INSERT INTO sources(base_url,name,docker_cap,github_cap,ghcr_cap,status,enabled,created_at,repo_host)
		VALUES('https://legacy.example','legacy',1,-1,-1,'usable',1,1,'')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	x, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if x.RepoHost != "" || x.RegistryIdentity != IdentityUnclassified {
		t.Fatalf("空 Host 源被错误归类: host=%q identity=%q", x.RepoHost, x.RegistryIdentity)
	}
}

// 双用途源清空 Host 后仍是 unclassified，不能仅凭 github_cap=yes 冒充 GitHub-only。
func TestEmptyHostDualCapStaysUnclassified(t *testing.T) {
	st, _ := newTestStore(t)
	defer st.Close()
	res, err := st.db.Exec(`INSERT INTO sources(base_url,name,docker_cap,github_cap,ghcr_cap,status,enabled,created_at,repo_host)
		VALUES('https://dual.example','dual',1,1,-1,'usable',1,1,'')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	x, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if x.RepoHost != "" || x.RegistryIdentity != IdentityUnclassified {
		t.Fatalf("双用途空 Host 源被错误归类: host=%q identity=%q", x.RepoHost, x.RegistryIdentity)
	}
}

// 旧库（没有 schema_meta）必须在打开阶段被拒绝，不能触发起始化或补列。
func TestOpenRejectsLegacyWithoutSchemaMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sources(id INTEGER PRIMARY KEY, repo_host TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("旧 Schema 数据库被错误接受")
	}
}
