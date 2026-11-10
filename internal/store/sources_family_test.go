package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, int64) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.AddSource(Source{
		Name: "m", BaseURL: "https://m.example", Status: "usable", Enabled: true,
		DockerCap: 1, GithubCap: 1, GitCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

func TestSampleKindMapping(t *testing.T) {
	cases := map[string]string{
		"raw":     "github",
		"release": "github",
		"github":  "github",
		"git":     "git", // git 有独立 git_cap，不借 GitHub 族样本
		"docker":  "docker",
		"ghcr":    "ghcr",
		"":        "",
	}
	for in, want := range cases {
		if got := sampleKind(in); got != want {
			t.Errorf("sampleKind(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// raw/release 必须能读到 github 的探测样本，git/docker 不能串读。
func TestRawReleaseReadGithubProbeSamples(t *testing.T) {
	st, id := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := st.AddCheck(Check{SourceID: id, Kind: "github", Tier: 2, OK: true, LatencyMS: 120, SpeedKBps: 4000}); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"raw", "release", "github"} {
		n, _, _, _, _, err := st.sourceSamples(id, kind)
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("%s 应读到 3 条 github 样本，实际 %d", kind, n)
		}
	}
	for _, kind := range []string{"git", "docker", "ghcr"} {
		n, _, _, _, _, err := st.sourceSamples(id, kind)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s 不应读到 github 样本，实际 %d", kind, n)
		}
	}
}

// raw/release 的代理速度必须复用 github 行，写入侧也要落到 github。
func TestProxySpeedFamilyReadWrite(t *testing.T) {
	st, id := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := st.AddProxySpeed(id, "raw", 200<<20, 8000); err != nil { // 25MB/s
			t.Fatal(err)
		}
	}
	var githubRows, rawRows int
	if err := st.db.QueryRow(`SELECT count(*) FROM proxy_speed WHERE source_id=? AND kind='github'`, id).Scan(&githubRows); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM proxy_speed WHERE source_id=? AND kind='raw'`, id).Scan(&rawRows); err != nil {
		t.Fatal(err)
	}
	if githubRows != 3 || rawRows != 0 {
		t.Fatalf("写入应落在 github 族：github=%d raw=%d", githubRows, rawRows)
	}
	for _, kind := range []string{"raw", "release", "github"} {
		if got := st.proxySpeed(id, kind); got < 20000 {
			t.Errorf("%s 应读到 github 族速度 ~25MB/s，实际 %d", kind, got)
		}
	}
	for _, kind := range []string{"docker", "ghcr", "git"} {
		if got := st.proxySpeed(id, kind); got != 0 {
			t.Errorf("%s 不应读到 github 族速度，实际 %d", kind, got)
		}
	}
}
