package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openLifecycle(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addLifecycleSource(t *testing.T, st *Store, name, status string, enabled bool) int64 {
	t.Helper()
	id, err := st.AddSource(Source{
		Name: name, BaseURL: "https://" + name + ".example", Status: status, Enabled: enabled,
		DockerCap: CapYes, GithubCap: CapNo,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// status_at 只在状态真的变化时更新：否则常年 dead 的源每轮探测都把
// 「坏了多久」清零，永远到不了归档条件。
func TestSetStatusWhyKeepsTransitionTime(t *testing.T) {
	st := openLifecycle(t)
	id := addLifecycleSource(t, st, "a", "usable", true)

	if err := st.SetStatusWhy(id, "dead", "连续 3 次探测失败"); err != nil {
		t.Fatal(err)
	}
	s, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.StatusAt == 0 || s.StatusReason != "连续 3 次探测失败" {
		t.Fatalf("状态切换要记时间和原因，实际 %+v", s)
	}
	first := s.StatusAt

	time.Sleep(1100 * time.Millisecond) // unix 秒级精度
	if err := st.SetStatusWhy(id, "dead", "又失败一次"); err != nil {
		t.Fatal(err)
	}
	s, _ = st.GetSource(id)
	if s.StatusAt != first {
		t.Fatalf("状态没变不该动 status_at：%d → %d", first, s.StatusAt)
	}
	if s.StatusReason != "连续 3 次探测失败" {
		t.Fatalf("状态没变不该改原因，实际 %q", s.StatusReason)
	}

	if err := st.SetStatusWhy(id, "usable", ""); err != nil {
		t.Fatal(err)
	}
	s, _ = st.GetSource(id)
	if s.StatusAt <= first {
		t.Fatalf("状态变了要刷新 status_at：%d → %d", first, s.StatusAt)
	}
	if s.StatusReason != "" {
		t.Fatalf("恢复可用要清空原因，实际 %q", s.StatusReason)
	}
}

func TestArchiveStaleOnlyOldDeadOrBlocked(t *testing.T) {
	st := openLifecycle(t)
	oldDead := addLifecycleSource(t, st, "old-dead", "dead", true)
	freshDead := addLifecycleSource(t, st, "fresh-dead", "dead", true)
	oldBlocked := addLifecycleSource(t, st, "old-blocked", "blocked", true)
	oldUsable := addLifecycleSource(t, st, "old-usable", "usable", true)
	oldDisabled := addLifecycleSource(t, st, "old-disabled", "dead", false)

	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	fresh := time.Now().Add(-time.Hour).Unix()
	for id, at := range map[int64]int64{
		oldDead: old, oldBlocked: old, oldUsable: old, oldDisabled: old, freshDead: fresh,
	} {
		if _, err := st.db.Exec(`UPDATE sources SET status_at=? WHERE id=?`, at, id); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.ArchiveStale(time.Now().Add(-14 * 24 * time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("只该归档 old-dead 和 old-blocked，实际 %d", n)
	}
	want := map[int64]string{
		oldDead: "archived", oldBlocked: "archived",
		freshDead: "dead", oldUsable: "usable", oldDisabled: "dead",
	}
	for id, w := range want {
		s, err := st.GetSource(id)
		if err != nil {
			t.Fatal(err)
		}
		if s.Status != w {
			t.Fatalf("源 %d 状态应为 %s，实际 %s", id, w, s.Status)
		}
	}
}

func TestCleanupArchivedDeletesOnlyArchived(t *testing.T) {
	st := openLifecycle(t)
	gone := addLifecycleSource(t, st, "gone", "archived", true)
	keep := addLifecycleSource(t, st, "keep", "archived", true)
	dead := addLifecycleSource(t, st, "dead", "dead", true)

	old := time.Now().Add(-400 * 24 * time.Hour).Unix()
	recent := time.Now().Add(-time.Hour).Unix()
	if _, err := st.db.Exec(`UPDATE sources SET status_at=? WHERE id IN (?,?)`, old, gone, dead); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE sources SET status_at=? WHERE id=?`, recent, keep); err != nil {
		t.Fatal(err)
	}
	// 检测记录和调度事件要跟着删，不能留孤儿。
	if err := st.AddCheck(Check{SourceID: gone, Kind: "docker", Tier: 2, OK: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO source_events(source_id,kind,ts,ok,ms,code) VALUES(?,?,?,?,?,?)`,
		gone, "docker", time.Now().Unix(), 1, 10, 200); err != nil {
		t.Fatal(err)
	}

	n, err := st.CleanupArchived(time.Now().Add(-180 * 24 * time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("只该删 gone，实际 %d", n)
	}
	if _, err := st.GetSource(gone); err == nil {
		t.Fatal("gone 应该已被删除")
	}
	var checks, events int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM checks WHERE source_id=?`, gone).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_events WHERE source_id=?`, gone).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if checks != 0 || events != 0 {
		t.Fatalf("删源要连带清理记录：checks=%d events=%d", checks, events)
	}
	for _, id := range []int64{keep, dead} {
		if _, err := st.GetSource(id); err != nil {
			t.Fatalf("源 %d 不该被删: %v", id, err)
		}
	}
}

func TestEnabledSourcesSkipsArchived(t *testing.T) {
	st := openLifecycle(t)
	live := addLifecycleSource(t, st, "live", "usable", true)
	arch := addLifecycleSource(t, st, "arch", "archived", true)

	list, err := st.EnabledSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != live {
		t.Fatalf("归档源不该进探测列表，实际 %+v", list)
	}
	_ = arch
}

func TestConsecutiveClean(t *testing.T) {
	st := openLifecycle(t)
	id := addLifecycleSource(t, st, "c", "blocked", true)

	// 最近两次干净、再往前一次是脏的 → 2。
	// ts 显式递增，别让同一秒内的插入顺序决定结论。
	for i, c := range []struct {
		ok       bool
		digestOK int
	}{{true, 0}, {true, 1}, {true, 1}} {
		if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: 2, OK: c.ok, DigestOK: c.digestOK, TS: 1000 + int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := st.ConsecutiveClean(id, "docker"); n != 2 {
		t.Fatalf("应为 2，实际 %d", n)
	}
	// L1 探测不算证据
	if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: 1, OK: true, DigestOK: 1, TS: 2000}); err != nil {
		t.Fatal(err)
	}
	if n := st.ConsecutiveClean(id, "docker"); n != 2 {
		t.Fatalf("tier1 不该计入，实际 %d", n)
	}
}
