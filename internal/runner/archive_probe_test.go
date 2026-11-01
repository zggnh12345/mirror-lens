package runner

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 探测期间被人工归档，结果回来不能把 archived 改回 usable。
func TestArchivedDuringProbeSurvivesWriteback(t *testing.T) {
	st, id := capStore(t, "usable", store.CapUnknown, store.CapUnknown)
	e := New(st)

	// 探测开始时拿到的快照：那时它还是 usable。
	snap, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	// 探测还没回来，人先把它归档了。
	if err := st.SetStatus(id, "archived"); err != nil {
		t.Fatal(err)
	}

	e.apply(*snap, map[string]probe.Result{
		"docker": {OK: true, Verdict: probe.VerdictYes, DigestOK: 1},
		"github": {OK: true, Verdict: probe.VerdictYes, DigestOK: 1},
	}, 2)

	got, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "archived" {
		t.Fatalf("探测把归档状态改成了 %q", got.Status)
	}
	if got.DockerCap != store.CapUnknown || got.GithubCap != store.CapUnknown {
		t.Fatalf("归档后能力不该被探测改写：docker=%d github=%d", got.DockerCap, got.GithubCap)
	}
}

// 条件更新的最后一道闸：读快照到写库之间被归档，UPDATE 本身也不能命中。
func TestLiveWritesSkipArchivedRows(t *testing.T) {
	st, id := capStore(t, "usable", store.CapUnknown, store.CapUnknown)
	if err := st.SetStatus(id, "archived"); err != nil {
		t.Fatal(err)
	}

	if ok, err := st.SetCapsLive(id, store.CapYes, store.CapYes); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("已归档的行不该被 SetCapsLive 改到")
	}
	if ok, err := st.SetStatusLive(id, "usable", ""); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("已归档的行不该被 SetStatusLive 改到")
	}

	got, _ := st.GetSource(id)
	if got.Status != "archived" || got.DockerCap != store.CapUnknown || got.GithubCap != store.CapUnknown {
		t.Fatalf("状态被改坏了: status=%s docker=%d github=%d", got.Status, got.DockerCap, got.GithubCap)
	}
}
