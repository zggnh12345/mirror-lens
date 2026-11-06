package store

import (
	"path/filepath"
	"testing"
)

// 真实回源吞吐优先于探测样本：探测只读 4MB，量到的是冷启动速度。
func TestProxySpeedPreferred(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.AddSource(Source{
		Name: "m", BaseURL: "https://m.example", Status: "usable", Enabled: true,
		DockerCap: 1, GithubCap: 1, GitCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: 3, OK: true, SpeedKBps: 2500}); err != nil {
		t.Fatal(err)
	}
	// 200MiB / 8s ≈ 25MB/s
	if err := st.AddProxySpeed(id, "docker", 200<<20, 8000); err != nil {
		t.Fatal(err)
	}
	_, _, _, spd, _, err := st.samples(id, "docker", false)
	if err != nil {
		t.Fatal(err)
	}
	if spd < 25000 || spd > 27000 {
		t.Fatalf("应优先用真实吞吐（约 26MB/s），实际 %d KB/s", spd)
	}
	// 小文件不计：慢启动占比高，会把估计拉低。
	if err := st.AddProxySpeed(id, "docker", 8<<20, 8000); err != nil {
		t.Fatal(err)
	}
	_, _, _, spd2, _, _ := st.samples(id, "docker", false)
	if spd2 < 25000 {
		t.Fatalf("8MiB 的小样本不该参与，实际 %d KB/s", spd2)
	}
}

// 评分按协议隔离：一台源 docker 很快、github 全挂，混算中位数两边都失真。
func TestSourceSamplesIsolateKind(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(Source{
		Name: "m", BaseURL: "https://m.example", Status: "usable", Enabled: true,
		DockerCap: 1, GithubCap: 1, GitCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: 3, OK: true, LatencyMS: 100, SpeedKBps: 5000}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddCheck(Check{SourceID: id, Kind: "github", Tier: 3, OK: true, LatencyMS: 2000, SpeedKBps: 100}); err != nil {
			t.Fatal(err)
		}
	}

	doc, err := st.SourceCandidates("docker")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) != 1 || doc[0].LatencyMS != 100 || doc[0].Samples != 3 {
		t.Fatalf("docker 只看 docker 样本（100ms×3），实际 %+v", doc)
	}

	gh, err := st.SourceCandidates("github")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh) != 1 || gh[0].LatencyMS != 2000 || gh[0].Samples != 3 {
		t.Fatalf("github 只看 github 样本（2000ms×3），实际 %+v", gh)
	}

	// 本协议没样本时不退回别的协议，否则 docker 数据会替 git 排名。
	// 源仍留在候选里（Samples=0 表示未知），只是没有分数。
	git, err := st.SourceCandidates("git")
	if err != nil {
		t.Fatal(err)
	}
	if len(git) != 1 || git[0].Samples != 0 || git[0].LatencyMS != 0 {
		t.Fatalf("git 无样本时不该借 docker 的分数，实际 %+v", git)
	}
}

// 升级前的老库没有 kind 列值，那些行可以当兜底；但别的协议不行。
func TestSourceSamplesFallsBackToLegacyOnly(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(Source{
		Name: "m", BaseURL: "https://m.example", Status: "usable", Enabled: true, DockerCap: 1, GithubCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: 3, OK: true, LatencyMS: 50, SpeedKBps: 9000}); err != nil {
			t.Fatal(err)
		}
	}
	// 老行：kind 为空
	if err := st.AddCheck(Check{SourceID: id, Kind: "", Tier: 3, OK: true, LatencyMS: 800, SpeedKBps: 300}); err != nil {
		t.Fatal(err)
	}

	got, err := st.SourceCandidates("docker")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LatencyMS != 50 || got[0].Samples != 2 {
		t.Fatalf("docker 只该看 docker 的 2 条，实际 %+v", got)
	}

	gh, err := st.SourceCandidates("github")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh) != 1 || gh[0].LatencyMS != 800 || gh[0].Samples != 1 {
		t.Fatalf("github 应只退回 1 条老样本（800ms），实际 %+v", gh)
	}
}
