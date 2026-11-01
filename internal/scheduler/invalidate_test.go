package scheduler

import (
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 状态一变就要作废缓存：否则新源要等 ListTTL，死源还会被选中。
func TestInvalidateDropsCandidateCache(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := New(st, Config{Enabled: true, ListTTL: 60e9})
	if got := s.Candidates("docker"); len(got) != 0 {
		t.Fatalf("空库不该有候选，得到 %+v", got)
	}

	if _, err := st.AddSource(store.Source{
		Name: "new", BaseURL: "https://new.example", Status: "usable",
		Enabled: true, DockerCap: store.CapYes,
	}); err != nil {
		t.Fatal(err)
	}
	// 缓存里还是空名单（60s 内不会重查）。
	if got := s.Candidates("docker"); len(got) != 0 {
		t.Fatalf("缓存期不该看到新源，得到 %+v", got)
	}

	s.Invalidate()
	if got := s.Candidates("docker"); len(got) != 1 {
		t.Fatalf("作废后应立即看到新源，得到 %+v", got)
	}
}
