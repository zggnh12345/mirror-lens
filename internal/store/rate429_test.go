package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 429 率只统计有足够样本的源：十次里两次 429 不该被报成 20%。
func TestListSourcesRate429(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)

	// 30 次里 6 次 429 → 20%
	for i := 0; i < 30; i++ {
		code := 206
		if i < 6 {
			code = 429
		}
		st.RecordSourceEvent(id, "docker", code == 206, 50, code)
	}
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].Rate429["docker"]; got < 0.19 || got > 0.21 {
		t.Fatalf("docker 429 率应约 0.2，实际 %v", got)
	}
	if _, ok := list[0].Rate429["github"]; ok {
		t.Fatalf("没有 github 事件时不该有值")
	}
}

func TestListSourcesRate429NeedsSamples(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	for i := 0; i < 10; i++ {
		st.RecordSourceEvent(id, "docker", false, 50, 429)
	}
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := list[0].Rate429["docker"]; ok {
		t.Fatalf("样本只有 10 次时不该报比例，实际 %v", v)
	}
}

func TestRecordSourceEventKeepsCode(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	st.RecordSourceEvent(id, "docker", false, 50, 429)
	st.RecordSourceEvent(id, "docker", false, 50, 0)

	codes, err := st.SourceEventCodes(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 2 || codes[0] != 0 || codes[1] != 429 {
		t.Fatalf("状态码没按顺序落库：%v", codes)
	}
}

// 老库的 source_events 没有 code 列，Open 之后要补上且默认 0。
func TestMigrateAddsEventCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old2.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := addSrc(t, st)
	st.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE source_events DROP COLUMN code`); err != nil {
		t.Fatalf("drop code: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO source_events(source_id,kind,ts,ok,ms) VALUES(?,?,?,?,?)`,
		id, "docker", time.Now().Unix(), 0, 10); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	codes, err := st2.SourceEventCodes(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 1 || codes[0] != 0 {
		t.Fatalf("补列后老事件应读成 0，实际 %v", codes)
	}
}
