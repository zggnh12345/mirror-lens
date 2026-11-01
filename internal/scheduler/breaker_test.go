package scheduler

import (
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

func TestHalfOpenAllowsOneProbe(t *testing.T) {
	st := newStore(t)
	addSource(t, st, "https://mirror.example", true, 100, 1500)
	sc := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	cur := time.Now()
	sc.now = func() time.Time { return cur }

	c := sc.Candidates("raw")[0]
	sc.Report("raw", c, false, 0)
	if n := len(sc.Candidates("raw")); n != 0 {
		t.Fatalf("冷却期内应排除，实际 %d 个", n)
	}

	cur = cur.Add(61 * time.Second)
	cs := sc.Candidates("raw")
	if len(cs) != 1 || !cs[0].Probe {
		t.Fatalf("冷却结束应放一个探测，实际 %+v", cs)
	}
	if n := len(sc.Candidates("raw")); n != 0 {
		t.Fatalf("探测在飞时不该再放第二个，实际 %d 个", n)
	}

	sc.Report("raw", cs[0], true, 0)
	cs = sc.Candidates("raw")
	if len(cs) != 1 || cs[0].Probe {
		t.Fatalf("探测成功后应恢复成正常候选，实际 %+v", cs)
	}
}

func TestBackoffDoubles(t *testing.T) {
	st := newStore(t)
	addSource(t, st, "https://mirror.example", true, 100, 1500)
	sc := New(st, Config{Enabled: true, FailCooldown: time.Minute})
	cur := time.Now()
	sc.now = func() time.Time { return cur }

	c := sc.Candidates("raw")[0]
	sc.Report("raw", c, false, 0)
	cur = cur.Add(61 * time.Second)
	cs := sc.Candidates("raw")
	if len(cs) != 1 {
		t.Fatalf("应进入半开，实际 %d 个", len(cs))
	}
	sc.Report("raw", cs[0], false, 0) // 半开探测又失败 → 冷却翻倍

	cur = cur.Add(61 * time.Second)
	if n := len(sc.Candidates("raw")); n != 0 {
		t.Fatalf("第二次失败后冷却应为 2 个周期，1 个周期内不该放行，实际 %d 个", n)
	}
	cur = cur.Add(60 * time.Second)
	if n := len(sc.Candidates("raw")); n != 1 {
		t.Fatalf("2 个周期后应放行探测，实际 %d 个", n)
	}
}

// git 能力默认关：能取 raw 文件不代表实现了 smart HTTP。
func TestGitCapGatesCandidates(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://gh-proxy.com", true, 100, 1500)
	sc := New(st, Config{Enabled: true})

	if n := len(sc.Candidates("git")); n != 0 {
		t.Fatalf("git_cap=0 的源不该进 git 候选，实际 %d 个", n)
	}
	if n := len(sc.Candidates("raw")); n != 1 {
		t.Fatalf("raw 不该受影响，实际 %d 个", n)
	}

	if err := st.SetGitCap(id, true); err != nil {
		t.Fatalf("置 git_cap: %v", err)
	}
	sc2 := New(st, Config{Enabled: true})
	if n := len(sc2.Candidates("git")); n != 1 {
		t.Fatalf("打开 git_cap 后应进候选，实际 %d 个", n)
	}
}

// 一次成功不等于 100%：样本少时向 50% 收，免得新源冲上第一。
func TestScoreSmoothsSmallSamples(t *testing.T) {
	one := score(store.SourceStat{Samples: 1, OK: 1, LatencyMS: 50, SpeedKBps: 2000})
	many := score(store.SourceStat{Samples: 10, OK: 10, LatencyMS: 50, SpeedKBps: 2000})
	if !(one < many) {
		t.Fatalf("单样本不该比十次全成功更高：%.3f vs %.3f", one, many)
	}
	if one > 0.95 {
		t.Fatalf("单样本分数过于乐观：%.3f", one)
	}

	// 没有速度样本时给中性分，不该被当成 0 速
	noSpeed := score(store.SourceStat{Samples: 10, OK: 10, LatencyMS: 50})
	if noSpeed < 0.5 {
		t.Fatalf("缺速度样本时不该被判成慢源：%.3f", noSpeed)
	}
}
