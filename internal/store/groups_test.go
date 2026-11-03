package store

import (
	"path/filepath"
	"testing"
)

// 分组要能落库、读回、带给调度候选；删组只清标签不动源。
func TestGroupsRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fast, err := st.AddSource(Source{Name: "fast", BaseURL: "https://fast.example", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	slow, err := st.AddSource(Source{Name: "slow", BaseURL: "https://slow.example", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}

	gid, err := st.CreateGroup("实测最快")
	if err != nil {
		t.Fatal(err)
	}
	// 新组排在最后：rank 从 1 起，未分组给 999。
	if gid == 0 {
		t.Fatal("建组应返回 id")
	}
	if err := st.SetSourceGroup(fast, gid); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	var got *Source
	for i := range list {
		if list[i].ID == fast {
			got = &list[i]
		}
	}
	if got == nil || got.GroupID != gid || got.GroupName != "实测最快" {
		t.Fatalf("源应带上组信息，实际 %+v", got)
	}

	cands, err := st.SourceCandidates("docker")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		switch c.ID {
		case fast:
			if c.GroupRank != 1 {
				t.Fatalf("组内源 rank 应为 1，实际 %d", c.GroupRank)
			}
		case slow:
			if c.GroupRank != 999 {
				t.Fatalf("未分组源 rank 应为 999，实际 %d", c.GroupRank)
			}
		}
	}

	groups, err := st.ListGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Sources != 1 {
		t.Fatalf("分组应统计到 1 个源，实际 %+v", groups)
	}

	// 删组：源退回未分组，源本身还在。
	if err := st.DeleteGroup(gid); err != nil {
		t.Fatal(err)
	}
	src, err := st.GetSource(fast)
	if err != nil {
		t.Fatal(err)
	}
	if src.GroupID != 0 {
		t.Fatalf("删组后源应退回未分组，实际 %d", src.GroupID)
	}
}

// 把源塞进不存在的组要报错，不能悄悄写成悬空 group_id。
func TestSetSourceGroupRejectsMissing(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "g2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.AddSource(Source{Name: "x", BaseURL: "https://x.example", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSourceGroup(id, 42); err == nil {
		t.Fatal("不存在的组应当报错")
	}
}
