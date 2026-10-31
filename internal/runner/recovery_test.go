package runner

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func capStore(t *testing.T, status string, docker, github int) (*store.Store, int64) {
	t.Helper()
	st := lifecycleStore(t)
	id, err := st.AddSource(store.Source{
		Name: "s", BaseURL: "https://s.example", Status: status, Enabled: true,
		DockerCap: docker, GithubCap: github,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

// 首次探测超时（不是「这里没有 docker」），不能写成 CapNo，否则这个源
// 以后永远不会再被识别，网络恢复了也回不来。
func TestTimeoutDoesNotMarkCapNo(t *testing.T) {
	st, id := capStore(t, "degraded", store.CapUnknown, store.CapUnknown)
	e := New(st)
	s, _ := st.GetSource(id)

	e.apply(*s, map[string]probe.Result{
		"docker": {OK: false, DigestOK: -1, Error: "context deadline exceeded"},
		"github": {OK: false, DigestOK: -1, Error: "dial tcp: lookup s.example: no such host"},
	}, 2)

	got, _ := st.GetSource(id)
	if got.DockerCap != store.CapUnknown || got.GithubCap != store.CapUnknown {
		t.Fatalf("超时/DNS 不该改能力，得到 docker=%d github=%d", got.DockerCap, got.GithubCap)
	}
}

// 明确的 404 才是结论。
func TestDefinitiveMissMarksCapNo(t *testing.T) {
	st, id := capStore(t, "degraded", store.CapUnknown, store.CapUnknown)
	e := New(st)
	s, _ := st.GetSource(id)

	e.apply(*s, map[string]probe.Result{
		"docker": {OK: false, DigestOK: -1, Verdict: probe.VerdictNo, HTTPCode: 404},
		"github": {OK: false, DigestOK: -1, Verdict: probe.VerdictNo, HTTPCode: 404},
	}, 2)

	got, _ := st.GetSource(id)
	if got.DockerCap != store.CapNo || got.GithubCap != store.CapNo {
		t.Fatalf("404 应记为不支持，得到 docker=%d github=%d", got.DockerCap, got.GithubCap)
	}
}

// 判死之后网络恢复：能力回来、状态回 usable。
func TestDeadSourceRecoversToUsable(t *testing.T) {
	st, id := capStore(t, "dead", store.CapUnknown, store.CapNo)
	e := New(st)
	s, _ := st.GetSource(id)

	e.apply(*s, map[string]probe.Result{
		"docker": {OK: true, Verdict: probe.VerdictYes, DigestOK: 1},
	}, 2)

	got, _ := st.GetSource(id)
	if got.Status != "usable" {
		t.Fatalf("恢复后应 usable，得到 %s（%s）", got.Status, got.StatusReason)
	}
	if got.DockerCap != store.CapYes {
		t.Fatalf("应记 docker=yes，得到 %d", got.DockerCap)
	}
}

// 首次探测全超时：状态是 pending（等复测），不是 dead。
func TestFirstProbeTimeoutStaysPending(t *testing.T) {
	st, id := capStore(t, "pending", store.CapUnknown, store.CapUnknown)
	e := New(st)
	s, _ := st.GetSource(id)

	e.apply(*s, map[string]probe.Result{
		"docker": {OK: false, DigestOK: -1, Error: "i/o timeout"},
		"github": {OK: false, DigestOK: -1, Error: "i/o timeout"},
	}, 2)

	got, _ := st.GetSource(id)
	if got.Status != "pending" {
		t.Fatalf("没拿到结论应 pending，得到 %s（%s）", got.Status, got.StatusReason)
	}
}

// 探测又通了也不能把人工归档顶掉。
func TestProbeDoesNotResurrectArchived(t *testing.T) {
	st, id := capStore(t, "archived", store.CapYes, store.CapNo)
	e := New(st)
	s, _ := st.GetSource(id)

	e.apply(*s, map[string]probe.Result{
		"docker": {OK: true, Verdict: probe.VerdictYes, DigestOK: 1},
	}, 2)

	got, _ := st.GetSource(id)
	if got.Status != "archived" {
		t.Fatalf("归档状态被探测覆盖成 %s", got.Status)
	}
}

// 早先被误判成 CapNo 的死源，也要继续识别（否则 kindsFor 一条都不返回，
// 永远停在 dead）。
func TestDeadSourceWithNoCapsIsStillProbed(t *testing.T) {
	s := store.Source{Status: "dead", DockerCap: store.CapNo, GithubCap: store.CapNo}
	if got := kindsFor(s, 2); len(got) != 2 {
		t.Fatalf("死源应两种都重测，得到 %v", got)
	}
	// 还活着的源不浪费这份额度。
	s.Status = "usable"
	if got := kindsFor(s, 2); len(got) != 0 {
		t.Fatalf("活着的 CapNo 源不该再测，得到 %v", got)
	}
}
