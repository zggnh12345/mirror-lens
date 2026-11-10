package store

import (
	"path/filepath"
	"testing"
)

func TestSchedOrderRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := st.GetSchedOrder(); got.Mode != "auto" {
		t.Fatalf("默认应为自动，实际 %q", got.Mode)
	}
	if err := st.SetSchedOrder(SchedOrder{Mode: "manual", Orders: map[string][]int64{"docker": {3, 1, 2}}}); err != nil {
		t.Fatal(err)
	}
	got := st.GetSchedOrder()
	if got.Mode != "manual" || len(got.Orders["docker"]) != 3 || got.Orders["docker"][0] != 3 {
		t.Fatalf("回读不一致：%+v", got)
	}
	// 非法模式一律落成自动，避免脏数据把调度锁死。
	if err := st.SetSchedOrder(SchedOrder{Mode: "weird"}); err != nil {
		t.Fatal(err)
	}
	if st.GetSchedOrder().Mode != "auto" {
		t.Fatal("非法模式应回落为 auto")
	}
}
