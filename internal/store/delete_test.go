package store

import (
	"path/filepath"
	"testing"
	"time"
)

// 删除源要么全删干净，要么什么都不动：不能留下指向不存在源的检查记录。
func TestDeleteSourceCleansEverything(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(Source{
		Name: "gone", BaseURL: "https://gone.example", Status: "usable",
		Enabled: true, DockerCap: CapYes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddCheck(Check{SourceID: id, Kind: "docker", OK: true, TS: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := st.NoteProbe(id, "docker", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO source_events(source_id,kind,ts,ok,ms,code) VALUES(?,?,?,?,?,?)`,
		id, "docker", time.Now().Unix(), 1, 100, 200); err != nil {
		t.Fatalf("记录事件失败: %v", err)
	}

	if err := st.DeleteSource(id); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM checks WHERE source_id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("checks 还剩 %d 条", n)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_events WHERE source_id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("source_events 还剩 %d 条", n)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_health WHERE source_id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("source_health 还剩 %d 条", n)
	}
	if _, err := st.GetSource(id); err == nil {
		t.Fatal("源还在")
	}
}
