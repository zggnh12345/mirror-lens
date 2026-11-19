package unified

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const digA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const digC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
const digD = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

type fakeCand struct {
	digest string
	delay  time.Duration
	runs   atomic.Int32
	aborts atomic.Int32
}

func (f *fakeCand) candidate(registry string, id int64) CrossCandidate {
	return CrossCandidate{
		Registry: registry,
		SourceID: id,
		Run: func(ctx context.Context) (string, error) {
			f.runs.Add(1)
			if f.delay > 0 {
				select {
				case <-time.After(f.delay):
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			if f.digest == "" {
				return "", errors.New("源失败")
			}
			return f.digest, nil
		},
		Abort: func() { f.aborts.Add(1) },
	}
}

// B.3：两路独立 Digest 一致就直接交付，不打扰官方权威。
func TestCrossRaceConfirmedWithoutAuthority(t *testing.T) {
	a, b, c := &fakeCand{digest: digA}, &fakeCand{digest: digA}, &fakeCand{digest: digB}
	var authorityCalls atomic.Int32
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) {
			authorityCalls.Add(1)
			return digA, nil
		},
	}, []CrossCandidate{
		a.candidate("quay.io", 1),
		b.candidate("ghcr.io", 2),
		c.candidate("docker.io", 3),
	})
	if err != nil {
		t.Fatalf("应确认: %v", err)
	}
	if out.State != VoteConfirmed || out.Digest != digA {
		t.Fatalf("结论不对: %+v", out)
	}
	if authorityCalls.Load() != 0 {
		t.Fatal("两路一致时不应请求官方权威")
	}
	if out.Winner < 0 || out.Winner > 1 {
		t.Fatalf("赢家必须来自首轮，实得 %d", out.Winner)
	}
	if c.runs.Load() != 0 {
		t.Fatal("确认后不应再请求第三候选")
	}
	// 每个首轮候选最多请求一次；未当选的必须被 Abort。
	if a.runs.Load() > 1 || b.runs.Load() > 1 {
		t.Fatalf("首轮候选被重复请求: a=%d b=%d", a.runs.Load(), b.runs.Load())
	}
	aborts := a.aborts.Load() + b.aborts.Load()
	if out.Winner == 0 && b.aborts.Load() != 1 {
		t.Fatal("落选候选必须 Abort")
	}
	if out.Winner == 1 && a.aborts.Load() != 1 {
		t.Fatal("落选候选必须 Abort")
	}
	_ = aborts
}

// B.6：两路不一致时先用剩余可信副本顺序补位；补位形成「不同 Registry +
// 不同 Source」的多数一致即可确认，不得提前动用 Authority。
func TestCrossRaceMismatchFillQuorumBeforeAuthority(t *testing.T) {
	a, b := &fakeCand{digest: digA}, &fakeCand{digest: digB}
	fill1, fill2 := &fakeCand{digest: digB}, &fakeCand{digest: digC}
	var calls atomic.Int32
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) {
			calls.Add(1)
			return digC, nil
		},
	}, []CrossCandidate{
		a.candidate("quay.io", 1),
		b.candidate("ghcr.io", 2),
		fill1.candidate("docker.io", 3),
		fill2.candidate("registry.k8s.io", 4),
	})
	if err != nil {
		t.Fatalf("补位多数一致应确认: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("补位已形成多数时不允许问 Authority，实际 %d", calls.Load())
	}
	if out.Digest != digB || (out.Winner != 1 && out.Winner != 2) {
		t.Fatalf("应选中补位后的多数 Digest（复用首轮已下载的候选）: %+v", out)
	}
	if fill1.runs.Load() != 1 || fill2.runs.Load() != 0 {
		t.Fatalf("补位应逐个尝试并在多数成立时停止: fill1=%d fill2=%d",
			fill1.runs.Load(), fill2.runs.Load())
	}
}

// 补位仍不一致：才动用唯一一次 Authority，并且只接受与权威一致的内容。
func TestCrossRaceMismatchFillThenAuthorityOnce(t *testing.T) {
	a, b := &fakeCand{digest: digA}, &fakeCand{digest: digB}
	fill1, fill2 := &fakeCand{digest: digC}, &fakeCand{digest: digD}
	var calls atomic.Int32
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) {
			calls.Add(1)
			return digD, nil
		},
	}, []CrossCandidate{
		a.candidate("quay.io", 1),
		b.candidate("ghcr.io", 2),
		fill1.candidate("docker.io", 3),
		fill2.candidate("registry.k8s.io", 4),
	})
	if err != nil {
		t.Fatalf("补位后与权威一致应交付: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("Authority 只允许问一次，实际 %d", calls.Load())
	}
	if out.Digest != digD || out.Winner != 3 || out.State != VoteAuthorityCompare {
		t.Fatalf("应选中与权威一致的补位候选: %+v", out)
	}
}

// 不一致且没有权威来源：整体放弃，交给原有顺序回退（不得猜一个交付）。
func TestCrossRaceMismatchWithoutAuthorityFailsOver(t *testing.T) {
	a, b := &fakeCand{digest: digA}, &fakeCand{digest: digB}
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{}, []CrossCandidate{
		a.candidate("quay.io", 1),
		b.candidate("ghcr.io", 2),
	})
	if !errors.Is(err, ErrCrossRaceNoCandidate) {
		t.Fatalf("应无结论，实得 %v", err)
	}
	if out.Winner != -1 || out.AuthorityUsed {
		t.Fatalf("不应有赢家或权威调用: %+v", out)
	}
}

// 单候选只能用官方权威裁定。
func TestCrossRaceSingleCandidateNeedsAuthority(t *testing.T) {
	ok := &fakeCand{digest: digA}
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) { return digA, nil },
	}, []CrossCandidate{ok.candidate("quay.io", 1)})
	if err != nil || out.Winner != 0 || !out.AuthorityUsed {
		t.Fatalf("单候选与权威一致应通过: %+v err=%v", out, err)
	}

	bad := &fakeCand{digest: digB}
	if _, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) { return digA, nil },
	}, []CrossCandidate{bad.candidate("quay.io", 1)}); !errors.Is(err, ErrCrossRaceNoCandidate) {
		t.Fatalf("与权威不一致必须放弃，实得 %v", err)
	}
	// 单候选 + 没有权威：无法独立印证，只能放弃。
	if _, err := RunCrossRace(context.Background(), CrossRaceConfig{}, []CrossCandidate{
		(&fakeCand{digest: digA}).candidate("quay.io", 1),
	}); !errors.Is(err, ErrCrossRaceNoCandidate) {
		t.Fatalf("无权威时必须放弃，实得 %v", err)
	}
}

// M 截断：超过 4 个候选时只看前 4，且被截断的候选完全不发请求。
func TestCrossRaceTruncatesToM(t *testing.T) {
	cands := make([]CrossCandidate, 0, 6)
	fakes := make([]*fakeCand, 0, 6)
	for i := 0; i < 6; i++ {
		f := &fakeCand{digest: digA}
		fakes = append(fakes, f)
		cands = append(cands, f.candidate("h"+string(rune('a'+i)), int64(i+1)))
	}
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{}, cands)
	if err != nil {
		t.Fatalf("前四路一致应确认: %v", err)
	}
	if out.Truncated != 2 {
		t.Fatalf("应截断 2 个，实得 %d", out.Truncated)
	}
	if fakes[4].runs.Load() != 0 || fakes[5].runs.Load() != 0 {
		t.Fatal("被截断的候选不得发请求")
	}
}

// 全部失败：无结论、无权威调用（不让官方替失败兜底当「确认」）。
func TestCrossRaceAllFailNeedsNoAuthority(t *testing.T) {
	var calls atomic.Int32
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) {
			calls.Add(1)
			return digA, nil
		},
	}, []CrossCandidate{
		(&fakeCand{}).candidate("quay.io", 1),
		(&fakeCand{}).candidate("ghcr.io", 2),
	})
	if !errors.Is(err, ErrCrossRaceNoCandidate) {
		t.Fatalf("应无结论，实得 %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("没有候选正文时不应请求权威")
	}
	if out.Winner != -1 {
		t.Fatalf("不应有赢家: %+v", out)
	}
}

// 并发安全：多轮竞速共享候选闭包不产生 data race（配合 -race 运行）。
func TestCrossRaceConcurrentRuns(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, b := &fakeCand{digest: digA}, &fakeCand{digest: digA}
			_, _ = RunCrossRace(context.Background(), CrossRaceConfig{}, []CrossCandidate{
				a.candidate("quay.io", 1),
				b.candidate("ghcr.io", 2),
			})
		}()
	}
	wg.Wait()
}
