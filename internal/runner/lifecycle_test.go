package runner

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func lifecycleStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func lifecycleSource(t *testing.T, st *store.Store, status string) int64 {
	t.Helper()
	id, err := st.AddSource(store.Source{
		Name: "s", BaseURL: "https://s.example", Status: status, Enabled: true,
		DockerCap: store.CapYes, GithubCap: store.CapNo,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// 摘要不一致必须封禁，并且原因写清楚——面板上「为什么封的」比一个红点有用。
func TestApplyBlocksOnDigestMismatch(t *testing.T) {
	st := lifecycleStore(t)
	id := lifecycleSource(t, st, "usable")
	e := New(st)

	s, _ := st.GetSource(id)
	e.apply(*s, map[string]probe.Result{
		"docker": {OK: true, DigestOK: 0, Digest: "sha256:bad"},
	}, 2)

	got, _ := st.GetSource(id)
	if got.Status != "blocked" {
		t.Fatalf("摘要不一致应封禁，实际 %s", got.Status)
	}
	if got.StatusReason == "" {
		t.Fatal("封禁要写原因")
	}
}

// 一次干净不算恢复：污染源可能间歇性返回正确内容，连续两次才放行。
func TestBlockedNeedsTwoCleanVerifications(t *testing.T) {
	st := lifecycleStore(t)
	id := lifecycleSource(t, st, "blocked")
	e := New(st)
	clean := map[string]probe.Result{"docker": {OK: true, DigestOK: 1}}

	s, _ := st.GetSource(id)
	e.apply(*s, clean, 2)
	if got, _ := st.GetSource(id); got.Status != "blocked" {
		t.Fatalf("一次干净仍应保持封禁，实际 %s", got.Status)
	}

	// 探测流程会先把本轮结果写进 checks，再调 apply。这里手动补上，
	// 模拟「又跑了一轮且校验一致」。
	if err := st.AddCheck(store.Check{SourceID: id, Kind: "docker", Tier: 2, OK: true, DigestOK: 1, TS: 100}); err != nil {
		t.Fatal(err)
	}
	s, _ = st.GetSource(id)
	e.apply(*s, clean, 2)
	if got, _ := st.GetSource(id); got.Status != "blocked" {
		t.Fatalf("只有一次干净记录时仍应封禁，实际 %s", got.Status)
	}

	if err := st.AddCheck(store.Check{SourceID: id, Kind: "docker", Tier: 2, OK: true, DigestOK: 1, TS: 101}); err != nil {
		t.Fatal(err)
	}
	s, _ = st.GetSource(id)
	e.apply(*s, clean, 2)
	if got, _ := st.GetSource(id); got.Status != "usable" {
		t.Fatalf("连续两次干净应恢复可用，实际 %s（%s）", got.Status, got.StatusReason)
	}
}

// 封禁期间又出现一次不一致，计数清零，继续封着。
func TestBlockedStaysOnFreshMismatch(t *testing.T) {
	st := lifecycleStore(t)
	id := lifecycleSource(t, st, "blocked")
	e := New(st)
	if err := st.AddCheck(store.Check{SourceID: id, Kind: "docker", Tier: 2, OK: true, DigestOK: 1, TS: 100}); err != nil {
		t.Fatal(err)
	}
	s, _ := st.GetSource(id)
	e.apply(*s, map[string]probe.Result{"docker": {OK: true, DigestOK: 0}}, 2)
	if got, _ := st.GetSource(id); got.Status != "blocked" {
		t.Fatalf("又对不上就该继续封禁，实际 %s", got.Status)
	}
}

func TestProbeDueOnlySlowsDeadAndBlocked(t *testing.T) {
	e := New(lifecycleStore(t))
	now := time.Now().Unix()
	gap := int64(time.Hour.Seconds())

	cases := []struct {
		name string
		src  store.Source
		want bool
	}{
		{"usable 每轮都探", store.Source{Status: "usable", ProbeAt: now - 10}, true},
		{"pending 每轮都探", store.Source{Status: "pending"}, true},
		{"dead 刚探过，跳过", store.Source{Status: "dead", ProbeAt: now - 60}, false},
		{"dead 到点了，探", store.Source{Status: "dead", ProbeAt: now - 3700}, true},
		{"blocked 从没探过，探", store.Source{Status: "blocked", ProbeAt: 0}, true},
		{"blocked 刚探过，跳过", store.Source{Status: "blocked", ProbeAt: now - 10}, false},
	}
	for _, c := range cases {
		if got := e.probeDue(c.src, now, gap); got != c.want {
			t.Fatalf("%s：期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}

// 归档和清理走同一个 maintain：坏够久的归档，归档够久的删掉。
func TestMaintainArchivesThenCleans(t *testing.T) {
	st := lifecycleStore(t)
	dead := lifecycleSource(t, st, "dead")
	time.Sleep(20 * time.Millisecond) // status_at 是秒级时间戳，等它落进过去

	e := New(st, Options{ArchiveAfter: time.Millisecond, CleanupAfter: -1})
	e.maintain(time.Now())
	if got, _ := st.GetSource(dead); got.Status != "archived" {
		t.Fatalf("该归档，实际 %s", got.Status)
	}

	time.Sleep(20 * time.Millisecond)
	e2 := New(st, Options{ArchiveAfter: time.Millisecond, CleanupAfter: time.Millisecond})
	e2.maintain(time.Now())
	if _, err := st.GetSource(dead); err == nil {
		t.Fatal("归档超期应被清理")
	}
}
