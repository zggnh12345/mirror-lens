package scheduler

import (
	"testing"
	"time"
)

// Retry-After 参与冷却，但只在比本地退避更久时生效，且有上限。
func TestReportRetryAfterDrivesCooldown(t *testing.T) {
	cases := []struct {
		name string
		hint time.Duration
		cool time.Duration
	}{
		{"源要求比本地退避久", 20 * time.Minute, 20 * time.Minute},
		{"源要求比本地退避短", 30 * time.Second, 5 * time.Minute},
		{"异常大值夹到上限", 48 * time.Hour, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			id := addSource(t, st, "https://m.example", true, 100, 900)

			now := time.Unix(1700000000, 0)
			s := New(st, Config{Enabled: true, MaxCandidates: 3})
			s.now = func() time.Time { return now }

			c := Candidate{ID: id, Name: "m", BaseURL: "https://m.example"}
			s.Report("raw", c, false, 0, Hint{RetryAfter: tc.hint})

			if got := s.Candidates("raw"); len(got) != 0 {
				t.Fatalf("失败后应立刻排除，实际 %d 个候选", len(got))
			}
			now = now.Add(tc.cool - time.Second)
			if got := s.Candidates("raw"); len(got) != 0 {
				t.Fatalf("冷却未到期（%s）就放出来了", tc.cool)
			}
			now = now.Add(2 * time.Second)
			got := s.Candidates("raw")
			if len(got) != 1 {
				t.Fatalf("冷却到期应恢复为半开候选，实际 %d 个", len(got))
			}
			if !got[0].Probe {
				t.Fatalf("冷却结束后第一个请求应是探测请求")
			}
		})
	}
}

// 没有提示时行为必须和以前一模一样：5 分钟起跳、逐次翻倍。
func TestReportWithoutHintKeepsBackoff(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://m.example", true, 100, 900)
	now := time.Unix(1700000000, 0)
	s := New(st, Config{Enabled: true, MaxCandidates: 3})
	s.now = func() time.Time { return now }

	c := Candidate{ID: id, Name: "m", BaseURL: "https://m.example"}
	s.Report("raw", c, false, 0)
	now = now.Add(5*time.Minute - time.Second)
	if got := s.Candidates("raw"); len(got) != 0 {
		t.Fatalf("默认冷却应仍是 5 分钟")
	}
	now = now.Add(2 * time.Second)
	if got := s.Candidates("raw"); len(got) != 1 {
		t.Fatalf("5 分钟后应恢复半开")
	}
}

// 成功照旧清掉熔断和提示，粘性照旧刷新。
func TestReportSuccessClearsRetryAfterState(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://m.example", true, 100, 900)
	now := time.Unix(1700000000, 0)
	s := New(st, Config{Enabled: true, MaxCandidates: 3})
	s.now = func() time.Time { return now }

	c := Candidate{ID: id, Name: "m", BaseURL: "https://m.example"}
	s.Report("raw", c, false, 0, Hint{RetryAfter: time.Hour})
	now = now.Add(time.Minute)
	s.Report("raw", c, true, 0, Hint{RetryAfter: time.Hour})

	got := s.Candidates("raw")
	if len(got) != 1 || got[0].Probe {
		t.Fatalf("成功应闭合熔断，实际 %+v", got)
	}
	sticky := got[0].Score
	// 粘性只活 StickyTTL，过期后分数应掉回去。
	now = now.Add(11 * time.Minute)
	if later := s.Candidates("raw"); len(later) == 1 {
		if later[0].Score >= sticky {
			t.Fatalf("粘性过期后分数应下降：%v → %v", sticky, later[0].Score)
		}
	}
}
