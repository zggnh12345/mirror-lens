package runner

import (
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 新源在观察池里只做 L1，攒够通过次数才放开 L2。
func TestObservedGate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(store.Source{
		Name: "m", BaseURL: "https://m.example", Status: "pending", Enabled: true,
		DockerCap: store.CapUnknown, GithubCap: store.CapUnknown,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{st: st}
	src := store.Source{ID: id, Status: "pending", DockerCap: store.CapUnknown, GithubCap: store.CapUnknown}

	if e.observed(src, "docker", 2) {
		t.Fatal("没有 L1 记录不该进 L2")
	}
	for i := 0; i < observePass; i++ {
		if err := st.AddCheck(store.Check{SourceID: id, Kind: "docker", Tier: 1, OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	if !e.observed(src, "docker", 2) {
		t.Fatal("L1 连续通过后应放行 L2")
	}
	if e.observed(src, "github", 2) {
		t.Fatal("另一类型没记录，不该被顺带放行")
	}

	// 已确认能力的源不受观察池限制
	src.DockerCap = store.CapYes
	if !e.observed(src, "docker", 2) {
		t.Fatal("已确认能力不该受观察池限制")
	}
}

func TestKindsForPendingUnknown(t *testing.T) {
	s := store.Source{Status: "pending", DockerCap: store.CapUnknown, GithubCap: store.CapUnknown}
	if got := kindsFor(s, 1); len(got) != 2 {
		t.Fatalf("pending 源在 L1 应两种都测，实际 %v", got)
	}
	s.Status = "usable"
	if got := kindsFor(s, 1); len(got) != 0 {
		t.Fatalf("usable 但在 L1 不该探测未知能力，实际 %v", got)
	}
}
