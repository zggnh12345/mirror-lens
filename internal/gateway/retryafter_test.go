package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func TestRetryAfterHeader(t *testing.T) {
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-90 * time.Second).UTC().Format(http.TimeFormat)

	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"秒数", "120", 120 * time.Second},
		{"零", "0", 0},
		{"负数", "-5", 0},
		{"乱写", "soon", 0},
		{"空", "", 0},
		{"过去的日期", past, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.raw != "" {
				h.Set("Retry-After", tc.raw)
			}
			if got := retryAfterHeader(h); got != tc.want {
				t.Fatalf("%q: want %s got %s", tc.raw, tc.want, got)
			}
		})
	}

	t.Run("日期", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", future)
		got := retryAfterHeader(h)
		if got < 80*time.Second || got > 95*time.Second {
			t.Fatalf("日期形式应换算成剩余时间，得到 %s", got)
		}
	})
}

// 预检 429 带 Retry-After 时，冷却长度要按源说的来，而不是本地 5 分钟。
// 把 FailCooldown 压到 50ms、Retry-After 设 120s：等 300ms 后仍被熔断，
// 只可能是 header 真的传到了调度器。
func TestPreflight429RetryAfterReachesScheduler(t *testing.T) {
	g, st, _, body, _, mirror, os, ms := parFixture(t, 4<<20)
	plan, cand := parPlan(t, os, body)

	mirror.status = http.StatusTooManyRequests
	mirror.hdr = map[string]string{"Retry-After": "120"}

	sched := scheduler.New(st, scheduler.Config{Enabled: true, FailCooldown: 50 * time.Millisecond})
	g.SetScheduler(sched)

	id, err := st.AddSource(store.Source{
		Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id

	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{
			{u: mustURL(t, ms.URL), c: cand},
			{u: plan.Upstream},
		}
	}

	rec := parRun(g, plan)
	if rec.Code != -1 {
		t.Fatalf("预检未通过时应退回单流，得到 %d", rec.Code)
	}

	_, failN, _, err := st.SourceEvents(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if failN != 1 {
		t.Fatalf("429 应记一次失败，实际 %d", failN)
	}

	time.Sleep(300 * time.Millisecond)
	if got := sched.Candidates("docker"); len(got) != 0 {
		t.Fatalf("Retry-After=120s 应压过 50ms 的本地退避，实际已放出来：%+v", got)
	}
}
