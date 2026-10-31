package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func rateOf(t *testing.T, st *Store, id int64, kind string) *float64 {
	t.Helper()
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	for i := range list {
		if list[i].ID == id {
			return list[i].Rate429[kind]
		}
	}
	t.Fatalf("没找到源 %d", id)
	return nil
}

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
	got := rateOf(t, st, id, "docker")
	if got == nil || *got < 0.19 || *got > 0.21 {
		t.Fatalf("docker 429 率应约 0.2，实际 %v", got)
	}
	if v := rateOf(t, st, id, "github"); v != nil {
		t.Fatalf("没有 github 事件时该给 null，实际 %v", *v)
	}
}

// 分母只数真实的上游响应：没拿到响应的（code=0）不进分母，否则
// 一次 429 被几百次超时一稀释，看起来就像「很健康」。
func TestRate429IgnoresNoResponse(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)

	for i := 0; i < 25; i++ {
		st.RecordSourceEvent(id, "docker", true, 40, 206)
	}
	for i := 0; i < 5; i++ {
		st.RecordSourceEvent(id, "docker", false, 40, 429)
	}
	// 100 次连接失败/超时：没有 HTTP 响应，不该稀释 429 率
	for i := 0; i < 100; i++ {
		st.RecordSourceEvent(id, "docker", false, 5000, 0)
	}

	got := rateOf(t, st, id, "docker")
	if got == nil {
		t.Fatal("30 次有效响应足够报比例，不该是 null")
	}
	if *got < 0.16 || *got > 0.17 {
		t.Fatalf("429 率应是 5/30≈0.167，实际 %v（分母被 code=0 污染了？）", *got)
	}
}

// 全是 code=0 时没有有效样本，必须给 null，不能给 0%。
func TestRate429NoValidSamplesIsNull(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	for i := 0; i < 50; i++ {
		st.RecordSourceEvent(id, "docker", false, 5000, 0)
	}
	if v := rateOf(t, st, id, "docker"); v != nil {
		t.Fatalf("没有一次上游响应时该给 null，实际 %v", *v)
	}
}

// 有效样本不够 20 次时也不报，哪怕其中一半是 429。
func TestRate429NeedsEnoughValidSamples(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	for i := 0; i < 10; i++ {
		st.RecordSourceEvent(id, "docker", false, 50, 429)
	}
	for i := 0; i < 200; i++ {
		st.RecordSourceEvent(id, "docker", false, 5000, 0)
	}
	if v := rateOf(t, st, id, "docker"); v != nil {
		t.Fatalf("有效样本只有 10 次时不该报比例，实际 %v", *v)
	}
}

// 5xx 也算有效响应：分母是「对方回话了」，不是「对方回得好」。
func TestRate429CountsErrorResponses(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	for i := 0; i < 30; i++ {
		st.RecordSourceEvent(id, "docker", false, 50, 502)
	}
	got := rateOf(t, st, id, "docker")
	if got == nil || *got != 0 {
		t.Fatalf("30 次 502、0 次 429 应报 0，实际 %v", got)
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
		t.Fatalf("insert: %v", err)
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
