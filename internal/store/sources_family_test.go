package store

import (
	"path/filepath"
	"testing"
	"time"
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

// 信任度同样按家族取，否则 raw 请求会看到 Trust=0，和评分口径分叉。
func TestTrustFamilyMapping(t *testing.T) {
	st, id := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := st.db.Exec(`INSERT INTO checks(source_id, kind, tier, ok, digest_ok, blob_ok, ts)
			VALUES(?,'github',2,1,1,1,?)`, id, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	m, err := st.trustMap("raw", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m[id].OK7 != 3 {
		t.Fatalf("raw 应读到 github 族信任样本，实际 OK7=%d", m[id].OK7)
	}
	if _, ok := m[id]; !ok {
		t.Fatal("raw 没有拿到 github 族信任记录")
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

// 超过 7 天的真实速度样本必须失效，回退到探测速度（而不是当 0 分）。
func TestProxySpeedExpires(t *testing.T) {
	st, id := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: 2, OK: true, LatencyMS: 100, SpeedKBps: 3000}); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Unix() - 8*24*3600
	for i := 0; i < 3; i++ {
		if _, err := st.db.Exec(`INSERT INTO proxy_speed(source_id, kind, ts, bytes, ms) VALUES(?,?,?,?,?)`,
			id, "docker", old, int64(200<<20), int64(8000)); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.proxySpeed(id, "docker"); got != 0 {
		t.Fatalf("8 天前的样本必须失效，实际 %d", got)
	}
	_, _, _, spd, _, err := st.sourceSamples(id, "docker")
	if err != nil {
		t.Fatal(err)
	}
	if spd < 2000 || spd > 4000 {
		t.Fatalf("过期后应回退探测速度 ~3000 KB/s，实际 %d", spd)
	}

	// 新鲜样本仍然生效。
	if err := st.AddProxySpeed(id, "docker", 200<<20, 8000); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.AddProxySpeed(id, "docker", 200<<20, 8000); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.proxySpeed(id, "docker"); got < 20000 {
		t.Fatalf("新样本应生效 ~25MB/s，实际 %d", got)
	}
}

// 2.x 遗留的无 kind 样本也要受 7 天限制；且不能和本协议样本合并计数。
func TestLegacySamplesAgeAndNoDoubleCount(t *testing.T) {
	st, id := newTestStore(t)
	old := time.Now().Unix() - 30*24*3600
	for i := 0; i < 3; i++ {
		if _, err := st.db.Exec(`INSERT INTO checks(source_id, kind, tier, ok, latency_ms, speed_kbps, ts)
			VALUES(?,'',2,1,100,3000,?)`, id, old); err != nil {
			t.Fatal(err)
		}
	}
	if n, _, _, _, _, err := st.sourceSamples(id, "docker"); err != nil || n != 0 {
		t.Fatalf("过期的 legacy 样本应失效，n=%d err=%v", n, err)
	}

	// 新鲜 legacy 行在没有本协议样本时仍可用。
	fresh := time.Now().Unix() - 3600
	for i := 0; i < 3; i++ {
		if _, err := st.db.Exec(`INSERT INTO checks(source_id, kind, tier, ok, latency_ms, speed_kbps, ts)
			VALUES(?,'',2,1,90,3500,?)`, id, fresh); err != nil {
			t.Fatal(err)
		}
	}
	if n, _, _, _, _, err := st.sourceSamples(id, "docker"); err != nil || n != 3 {
		t.Fatalf("新鲜 legacy 样本应可用，n=%d err=%v", n, err)
	}

	// 有本协议样本时不再读 legacy，避免重复计入。
	for i := 0; i < 2; i++ {
		if err := st.AddCheck(Check{SourceID: id, Kind: "github", Tier: 2, OK: true, LatencyMS: 100, SpeedKBps: 4000}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _, _, _, _, err := st.sourceSamples(id, "raw"); err != nil || n != 2 {
		t.Fatalf("raw 应只算 2 条 github 样本（不合并 legacy），实际 %d", n)
	}
}
