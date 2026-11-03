package store

import (
	"path/filepath"
	"testing"
)

// 整组覆盖：勾上的进组，原本在组里但没勾的退回未分组；跨组是移动不是复制。
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

	if err := st.SetGroupMembers(ga, []int64{src[0], src[1]}); err != nil {
		t.Fatal(err)
	}
	if got := groupOf(t, st, src[0]); got != ga {
		t.Fatalf("a 应在 A 组，实际 %d", got)
	}

	// b 挪到 B，a 不受影响。
	if err := st.SetGroupMembers(gb, []int64{src[1]}); err != nil {
		t.Fatal(err)
	}
	if got := groupOf(t, st, src[1]); got != gb {
		t.Fatalf("b 应被挪到 B 组，实际 %d", got)
	}
	if got := groupOf(t, st, src[0]); got != ga {
		t.Fatalf("a 应仍在 A 组，实际 %d", got)
	}

	// 清空 A：a 退回未分组，别的组不受影响。
	if err := st.SetGroupMembers(ga, nil); err != nil {
		t.Fatal(err)
	}
	if got := groupOf(t, st, src[0]); got != 0 {
		t.Fatalf("清空后 a 应未分组，实际 %d", got)
	}
	if got := groupOf(t, st, src[1]); got != gb {
		t.Fatalf("清空 A 不应动 B 组的 b，实际 %d", got)
	}

	// 不存在的组要报错，不能把源静默清空。
	if err := st.SetGroupMembers(9999, []int64{src[2]}); err == nil {
		t.Fatal("不存在的组应报错")
	}
	if got := groupOf(t, st, src[2]); got != 0 {
		t.Fatalf("报错时不应改数据，c 实际 %d", got)
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
