package store

import (
	"path/filepath"
	"testing"
)

// 整组覆盖：勾上的未分组源进组，原本在组里但没勾的退回未分组；
// 已在别的组的源不会被悄悄搬走（分组成员唯一）。
func TestSetGroupMembers(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var src []int64
	for _, n := range []string{"a", "b", "c"} {
		id, err := st.AddSource(Source{Name: n, BaseURL: "https://" + n + ".example", Status: "usable", Enabled: true, DockerCap: 1})
		if err != nil {
			t.Fatal(err)
		}
		src = append(src, id)
	}
	ga, err := st.CreateGroup("A")
	if err != nil {
		t.Fatal(err)
	}
	gb, err := st.CreateGroup("B")
	if err != nil {
		t.Fatal(err)
	}

	if n, err := st.SetGroupMembers(ga, []int64{src[0], src[1]}); err != nil || n != 2 {
		t.Fatalf("两个都没分组的源应全部进组：n=%d err=%v", n, err)
	}
	if got := groupOf(t, st, src[0]); got != ga {
		t.Fatalf("a 应在 A 组，实际 %d", got)
	}

	// b 已在 A 组，B 组整组覆盖时不能把它搬走；c 未分组可以进。
	if n, err := st.SetGroupMembers(gb, []int64{src[1], src[2]}); err != nil || n != 1 {
		t.Fatalf("只有 c 该进组：n=%d err=%v", n, err)
	}
	if got := groupOf(t, st, src[1]); got != ga {
		t.Fatalf("b 已在 A 组，不该被搬到 B，实际 %d", got)
	}
	if got := groupOf(t, st, src[2]); got != gb {
		t.Fatalf("c 未分组，应进 B，实际 %d", got)
	}
	if got := groupOf(t, st, src[0]); got != ga {
		t.Fatalf("a 应仍在 A 组，实际 %d", got)
	}

	// 清空 A：a 退回未分组，别的组不受影响。
	if _, err := st.SetGroupMembers(ga, nil); err != nil {
		t.Fatal(err)
	}
	if got := groupOf(t, st, src[0]); got != 0 {
		t.Fatalf("清空后 a 应未分组，实际 %d", got)
	}
	if got := groupOf(t, st, src[2]); got != gb {
		t.Fatalf("清空 A 不应动 B 组的 c，实际 %d", got)
	}

	// 不存在的组要报错，不能把源静默清空。
	if _, err := st.SetGroupMembers(9999, []int64{src[2]}); err == nil {
		t.Fatal("不存在的组应报错")
	}
	if got := groupOf(t, st, src[2]); got != gb {
		t.Fatalf("报错时不应改数据，c 应仍在 B 组，实际 %d", got)
	}
}

func groupOf(t *testing.T, st *Store, id int64) int64 {
	t.Helper()
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.ID == id {
			return x.GroupID
		}
	}
	t.Fatalf("找不到源 %d", id)
	return -1
}
