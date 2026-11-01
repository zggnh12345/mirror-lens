package scheduler

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 加一个源并灌 4 条 tier=3 样本。
func addSource(t *testing.T, st *store.Store, base string, ok bool, lat, spd int64) int64 {
	t.Helper()
	id, err := st.AddSource(store.Source{BaseURL: base, Name: base, GithubCap: 1, DockerCap: 1, Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	for i := 0; i < 4; i++ {
		// 评分按协议隔离：灌 raw 的样本，查询也得用 raw。
		if err := st.AddCheck(store.Check{SourceID: id, Kind: "raw", Tier: 3, OK: ok, LatencyMS: lat, SpeedKBps: spd}); err != nil {
			t.Fatalf("加样本: %v", err)
		}
	}
	return id
}

func TestOrderPrefersFastAndHealthy(t *testing.T) {
	st := newStore(t)
	addSource(t, st, "https://slow.example", true, 4000, 50)
	fast := addSource(t, st, "https://fast.example", true, 120, 1800)
	broken := addSource(t, st, "https://broken.example", false, 300, 0)

	s := New(st, Config{Enabled: true, MaxCandidates: 3})
	cs := s.Candidates("raw")
	if len(cs) != 3 {
		t.Fatalf("应有 3 个候选，实际 %d", len(cs))
	}
	if cs[0].ID != fast {
		t.Fatalf("最快的源应排第一，实际 %s", cs[0].Name)
	}
	if cs[len(cs)-1].ID != broken {
		t.Fatalf("全失败的源应排最后，实际 %s", cs[len(cs)-1].Name)
	}
}

func TestMaxCandidates(t *testing.T) {
	st := newStore(t)
	for i := 0; i < 5; i++ {
		addSource(t, st, "https://s"+string(rune('a'+i))+".example", true, int64(100*(i+1)), 500)
	}
	s := New(st, Config{Enabled: true, MaxCandidates: 2})
	if got := len(s.Candidates("raw")); got != 2 {
		t.Fatalf("上限 2，实际 %d", got)
	}
}

// 代理失败只进冷却，不改探测样本（测量与代理隔离）。
func TestReportDoesNotTouchProbeSamples(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://a.example", true, 200, 900)
	s := New(st, Config{Enabled: true})

	before, err := st.SourceCandidates("raw")
	if err != nil {
		t.Fatal(err)
	}
	s.Report("raw", Candidate{ID: id, Name: "a", BaseURL: "https://a.example"}, false, 3*time.Second)

	after, err := st.SourceCandidates("raw")
	if err != nil {
		t.Fatal(err)
	}
	if before[0].Samples != after[0].Samples || before[0].OK != after[0].OK || before[0].LatencyMS != after[0].LatencyMS {
		t.Fatalf("代理失败不该改变探测评分：%+v -> %+v", before[0], after[0])
	}
}

func TestFailureDemotesCandidate(t *testing.T) {
	st := newStore(t)
	a := addSource(t, st, "https://a.example", true, 100, 1500)
	b := addSource(t, st, "https://b.example", true, 300, 800)
	s := New(st, Config{Enabled: true, MaxCandidates: 3})

	if s.Candidates("raw")[0].ID != a {
		t.Fatal("初始 a 应排第一")
	}
	s.Report("raw", Candidate{ID: a, Name: "a", BaseURL: "https://a.example"}, false, time.Second)
	if got := s.Candidates("raw")[0].ID; got != b {
		t.Fatalf("a 失败一次后应让位给 b，实际 %d", got)
	}
	// 成功一次立刻恢复粘性
	s.Report("raw", Candidate{ID: a, Name: "a", BaseURL: "https://a.example"}, true, time.Millisecond)
	if got := s.Candidates("raw")[0].ID; got != a {
		t.Fatalf("成功后应回到第一位，实际 %d", got)
	}
}

func TestRewriteGithubPrefix(t *testing.T) {
	s := New(nil, Config{Enabled: true})
	base, _ := url.Parse("https://raw.githubusercontent.com/o/r/main/a.txt")
	got, ok := s.Rewrite("raw", base, Candidate{BaseURL: "https://gh-proxy.com"})
	if !ok {
		t.Fatal("改写失败")
	}
	want := "https://gh-proxy.com/https://raw.githubusercontent.com/o/r/main/a.txt"
	if got.String() != want {
		t.Fatalf("得到 %s，期望 %s", got, want)
	}
}

func TestRewriteDockerHostSwap(t *testing.T) {
	s := New(nil, Config{Enabled: true})
	base, _ := url.Parse("https://registry-1.docker.io/v2/library/alpine/manifests/latest?ns=x")
	got, ok := s.Rewrite("docker", base, Candidate{BaseURL: "https://docker.m.daocloud.io"})
	if !ok {
		t.Fatal("改写失败")
	}
	want := "https://docker.m.daocloud.io/v2/library/alpine/manifests/latest?ns=x"
	if got.String() != want {
		t.Fatalf("得到 %s，期望 %s", got, want)
	}
}

func TestDisabledSchedulerReturnsNothing(t *testing.T) {
	st := newStore(t)
	addSource(t, st, "https://a.example", true, 100, 900)
	s := New(st, Config{Enabled: false})
	if len(s.Candidates("raw")) != 0 {
		t.Fatal("关闭时不该返回候选")
	}
}

// 某个源做不了 git 协议，不该把它从 raw 的候选里踢掉。
func TestCooldownIsPerKind(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://gh-proxy.com", true, 100, 1500)
	sc := New(st, Config{Enabled: true, FailCooldown: time.Minute})

	sc.Report("git", Candidate{ID: id, Name: "gh-proxy"}, false, 0)
	if n := len(sc.Candidates("git")); n != 0 {
		t.Fatalf("git 冷却期应排除该源，还剩 %d 个", n)
	}
	if n := len(sc.Candidates("raw")); n != 1 {
		t.Fatalf("raw 不该受影响，实际 %d 个", n)
	}
}
