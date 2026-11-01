package store

import (
	"path/filepath"
	"testing"
)

// preferred 是后加的列：老库要靠 ensureColumns 补上，写入后调度查询必须读得到。
func TestPreferredSourceRoundTrip(t *testing.T) {
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

	// 新源默认不是优先，避免升级后所有源突然都被顶到前面。
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Preferred {
		t.Fatalf("新源默认不应是优先，实际 %+v", list)
	}

	on := true
	if err := st.UpdateSource(id, nil, nil, nil, &on); err != nil {
		t.Fatal(err)
	}
	cand, err := st.SourceCandidates("docker")
	if err != nil {
		t.Fatal(err)
	}
	if len(cand) != 1 || !cand[0].Preferred {
		t.Fatalf("调度候选应带上 preferred=true，实际 %+v", cand)
	}

	off := false
	if err := st.UpdateSource(id, nil, nil, nil, &off); err != nil {
		t.Fatal(err)
	}
	cand, err = st.SourceCandidates("docker")
	if err != nil {
		t.Fatal(err)
	}
	if len(cand) != 1 || cand[0].Preferred {
		t.Fatalf("取消优先后应恢复 false，实际 %+v", cand)
	}
}
