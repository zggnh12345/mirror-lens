package store

import (
	"path/filepath"
	"testing"
)

// docker 连续探测失败，只影响 docker 的候选；github 照常走这台源。
func TestKindHealthIsPerProtocol(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(Source{
		Name: "both", BaseURL: "https://both.example", Status: "usable",
		Enabled: true, DockerCap: CapYes, GithubCap: CapYes,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < HealthFails; i++ {
		if err := st.NoteProbe(id, "docker", false); err != nil {
			t.Fatal(err)
		}
	}

	dk, err := st.SourceCandidates("docker")
	if err != nil {
		t.Fatal(err)
	}
	if len(dk) != 0 {
		t.Fatalf("docker 连挂 3 次还在 docker 候选里：%+v", dk)
	}
	gh, err := st.SourceCandidates("github")
	if err != nil {
		t.Fatal(err)
	}
	if len(gh) != 1 || gh[0].ID != id {
		t.Fatalf("github 不该被 docker 的故障牵连，得到 %+v", gh)
	}

	// 恢复一次就把计数清零。
	if err := st.NoteProbe(id, "docker", true); err != nil {
		t.Fatal(err)
	}
	if dk, _ := st.SourceCandidates("docker"); len(dk) != 1 {
		t.Fatalf("恢复后应回到 docker 候选，得到 %+v", dk)
	}
	h, err := st.HealthOf(id)
	if err != nil {
		t.Fatal(err)
	}
	if h["docker"] != 0 {
		t.Fatalf("成功后 fails 应清零，得到 %d", h["docker"])
	}
}

// raw/release/github 共用一个协议族。
func TestFamilyMapping(t *testing.T) {
	for _, k := range []string{"raw", "release", "github"} {
		if Family(k) != "github" {
			t.Fatalf("%s 应归到 github", k)
		}
	}
	if Family("docker") != "docker" || Family("git") != "git" {
		t.Fatal("docker/git 映射错了")
	}
}
