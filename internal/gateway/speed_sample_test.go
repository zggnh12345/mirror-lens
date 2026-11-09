package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 客户端限速时，上游读到的只是 TCP 背压后的节奏；这种样本必须丢弃，
// 否则源站会被记成和客户端一样慢（实测 1MB/s 客户端把 10MB/s 源记成 1.6MB/s）。
func TestNoteSpeedDropsClientBound(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.AddSource(store.Source{Name: "m", BaseURL: "https://m.example", Status: "usable", Enabled: true, DockerCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{st: st}
	tgt := &fetchTarget{c: &scheduler.Candidate{ID: id}}

	// 10s 里 9s 阻塞在写客户端：丢弃。
	g.noteSpeed(tgt, "docker", 20<<20, time.Now().Add(-10*time.Second), 9*time.Second)
	if got := st.RecentProxySpeeds(id, "docker", 10); len(got) != 0 {
		t.Fatalf("客户端受限的样本不该入库，实际 %v", got)
	}

	// 没有下游阻塞：20MiB / 10s ≈ 2MB/s，正常入库。
	g.noteSpeed(tgt, "docker", 20<<20, time.Now().Add(-10*time.Second), 0)
	got := st.RecentProxySpeeds(id, "docker", 10)
	if len(got) != 1 {
		t.Fatalf("应记 1 条样本，实际 %v", got)
	}
	if got[0] < 1500 || got[0] > 2500 {
		t.Fatalf("速度应约 2000 KB/s，实际 %d", got[0])
	}
}
