package store

import (
	"path/filepath"
	"testing"
)

// 一次提交完整顺序就应该落成 1..n，并且不用两两交换 rank。
func TestReorderGroups(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var ids []int64
	for _, n := range []string{"A", "B", "C"} {
		id, err := st.CreateGroup(n)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	// C, A, B
	if err := st.ReorderGroups([]int64{ids[2], ids[0], ids[1]}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListGroups()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"C", "A", "B"}
	if len(list) != len(want) {
		t.Fatalf("组数量 %d，期望 %d", len(list), len(want))
	}
	for i, g := range list {
		if g.Name != want[i] || g.Rank != i+1 {
			t.Fatalf("第 %d 个是 %s(rank %d)，期望 %s(rank %d)", i, g.Name, g.Rank, want[i], i+1)
		}
	}

	// 只提交部分 id 时，没提到的组被压到后面，且不与前面的档位并列。
	if err := st.ReorderGroups([]int64{ids[0]}); err != nil {
		t.Fatal(err)
	}
	list, err = st.ListGroups()
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Name != "A" || list[0].Rank != 1 {
		t.Fatalf("提交单个 id 后 A 应在首位，实际 %+v", list[0])
	}
	seen := map[int]bool{}
	for _, g := range list {
		if seen[g.Rank] {
			t.Fatalf("rank 重复：%d", g.Rank)
		}
		seen[g.Rank] = true
	}
}
