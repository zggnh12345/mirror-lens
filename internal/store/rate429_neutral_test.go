package store

import "testing"

// 404/410、401/403 不是限流，不能进 429 率分母；code=0 也不进。
// 分母只留「有成功/失败语义的真实响应」。
func TestRate429ExcludesNeutralResults(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)

	for i := 0; i < 20; i++ {
		st.RecordSourceEvent(id, "docker", true, 40, 200)
	}
	for i := 0; i < 10; i++ {
		st.RecordSourceEvent(id, "docker", false, 40, 429)
	}
	// 中性结果：缺文件、没资格问，混进来会把 10/30 稀释成 10/430。
	for i := 0; i < 200; i++ {
		st.RecordSourceEvent(id, "docker", true, 20, 404)
		st.RecordSourceEvent(id, "docker", true, 20, 410)
		st.RecordSourceEvent(id, "docker", true, 20, 401)
		st.RecordSourceEvent(id, "docker", true, 20, 403)
	}

	got := rateOf(t, st, id, "docker")
	if got == nil {
		t.Fatal("30 次有效响应足够报比例，不该是 null")
	}
	if *got < 0.33 || *got > 0.34 {
		t.Fatalf("429 率应是 10/30≈0.333，实际 %v（分母被 404/401 稀释了？）", *got)
	}
}

// 中性结果不影响其它协议族的分母。
func TestRate429NeutralDoesNotLeakAcrossKinds(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)

	for i := 0; i < 30; i++ {
		st.RecordSourceEvent(id, "github", true, 40, 200)
	}
	for i := 0; i < 30; i++ {
		st.RecordSourceEvent(id, "docker", true, 20, 404)
	}

	if got := rateOf(t, st, id, "docker"); got != nil {
		t.Fatalf("docker 只有中性结果，不该有 429 率，实际 %v", *got)
	}
	got := rateOf(t, st, id, "github")
	if got == nil || *got != 0 {
		t.Fatalf("github 的 429 率应为 0，实际 %v", got)
	}
}

// 中性事件仍然要留档：只能从分母里排除，不能从表里删。
func TestNeutralEventsAreStillRecorded(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)

	st.RecordSourceEvent(id, "docker", true, 20, 404)
	st.RecordSourceEvent(id, "docker", true, 20, 401)

	okN, neutralN, failN, _, err := st.SourceEvents(id, 10)
	if err != nil {
		t.Fatalf("读事件: %v", err)
	}
	// 三态口径：404/401 是中性，不是成功。
	if okN != 0 || neutralN != 2 || failN != 0 {
		t.Fatalf("中性事件应保留为 ok=0/neutral=2/fail=0，实际 %d/%d/%d", okN, neutralN, failN)
	}

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_events WHERE source_id=? AND code IN (404,401)`, id).Scan(&n); err != nil {
		t.Fatalf("查事件: %v", err)
	}
	if n != 2 {
		t.Fatalf("原始事件应留 2 条，实际 %d", n)
	}
}
