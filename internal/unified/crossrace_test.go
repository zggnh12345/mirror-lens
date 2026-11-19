package unified

import (
	"context"
	"errors"
	"strings"
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
	if out.Winner != 0 {
		// 确定性：不按完成先后，首轮内按固定顺序取第一个一致者。
		t.Fatalf("赢家必须是下标 0，实得 %d", out.Winner)
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
	if out.Digest != digB || out.Winner != 1 {
		t.Fatalf("应复用首轮已下载的多数候选（下标 1）: %+v", out)
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

// 投票窗口是等待上界：慢候选不得把整条请求拖到它超时。
func TestCrossRaceWindowBoundsSlowCandidate(t *testing.T) {
	fast := &fakeCand{digest: digA}
	slow := &fakeCand{digest: digA, delay: 2 * time.Second}
	start := time.Now()
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Window: 50 * time.Millisecond,
		Authority: func(context.Context) (string, error) {
			return digA, nil
		},
	}, []CrossCandidate{fast.candidate("quay.io", 1), slow.candidate("ghcr.io", 2)})
	if err != nil {
		t.Fatalf("单票 + 权威一致应交付: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("窗口外候选必须被丢弃，实际等待 %v", el)
	}
	if out.Winner != 0 || out.Digest != digA {
		t.Fatalf("应交付已完成的候选: %+v", out)
	}
}

// 同 Registry 或同 Source 的观察不构成独立多数。
func TestQuorumRequiresDistinctRegistryAndSource(t *testing.T) {
	a, b, c := &fakeCand{digest: digA}, &fakeCand{digest: digA}, &fakeCand{digest: digB}
	var authority atomic.Int32
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) {
			authority.Add(1)
			return digB, nil
		},
	}, []CrossCandidate{
		a.candidate("quay.io", 7),
		b.candidate("quay.io", 7), // 同 Registry + 同 Source：不独立
		c.candidate("ghcr.io", 8),
	})
	if err != nil {
		t.Fatalf("权威对照一致应交付: %v", err)
	}
	if authority.Load() != 1 {
		t.Fatalf("非独立多数不能当确认，必须问一次权威，实际 %d", authority.Load())
	}
	if out.State != VoteAuthorityCompare || out.Digest != digB || out.Winner != 2 {
		t.Fatalf("应选与权威一致的候选: %+v", out)
	}
}

// 全部分歧且权威也不匹配：不猜一个交付。
func TestCrossRaceNoCandidateWhenAuthorityDisagreesWithAll(t *testing.T) {
	var cands []CrossCandidate
	for i, d := range []string{digA, digB, digC, digD} {
		f := &fakeCand{digest: d}
		cands = append(cands, f.candidate([]string{"quay.io", "ghcr.io", "docker.io", "registry.k8s.io"}[i], int64(i+1)))
	}
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) {
			return "sha256:" + strings.Repeat("e", 64), nil
		},
	}, cands)
	if !errors.Is(err, ErrCrossRaceNoCandidate) {
		t.Fatalf("应按无结论返回: %v", err)
	}
	if out.Winner != -1 || out.Digest != "" {
		t.Fatalf("没有可信结论时不得指定赢家: %+v", out)
	}
}

// 首轮都慢于窗口：窗口必须从首票起算，否则零票会把正常竞速判成无结论。
func TestCrossRaceWindowStartsAtFirstVote(t *testing.T) {
	a := &fakeCand{digest: digA, delay: 300 * time.Millisecond}
	b := &fakeCand{digest: digA, delay: 300 * time.Millisecond}
	var authority atomic.Int32
	start := time.Now()
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Authority: func(context.Context) (string, error) {
			authority.Add(1)
			return digA, nil
		},
	}, []CrossCandidate{a.candidate("quay.io", 1), b.candidate("ghcr.io", 2)})
	if err != nil {
		t.Fatalf("两路同内容应确认: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("等待过长: %v", el)
	}
	if out.State != VoteConfirmed || out.Winner != 0 || out.Digest != digA {
		t.Fatalf("结论不对: %+v", out)
	}
	if authority.Load() != 0 {
		t.Fatal("两票一致时不应问权威")
	}
}

// ---- 第四轮：窗口关闭与计时起点 ----

// 窗口关闭后 Offer 不得改变结论：Votes 不增、Agreed 不变、状态不回退。
func TestManifestVoteOfferAfterCloseIgnored(t *testing.T) {
	v := NewManifestVote(30 * time.Millisecond)
	if st, ok := v.Offer(VoteCandidate{Registry: "quay.io", SourceID: 1, Digest: digA, Verified: true, Complete: true}); !ok || st != VoteCollecting {
		t.Fatalf("首票应被接受: st=%v ok=%v", st, ok)
	}
	state := v.CloseWindow()
	if state != VoteAuthorityCompare {
		t.Fatalf("单票窗口关闭应为 authority_compare，实际 %v", state)
	}
	votes, agreed := v.Votes(), v.Agreed()
	if st, ok := v.Offer(VoteCandidate{Registry: "ghcr.io", SourceID: 2, Digest: digA, Verified: true, Complete: true}); ok || st != state {
		t.Fatalf("窗口关闭后不得再收票: st=%v ok=%v", st, ok)
	}
	if v.Votes() != votes || v.Agreed() != agreed {
		t.Fatalf("窗口关闭后结论被改变: votes=%d agreed=%q", v.Votes(), v.Agreed())
	}
}

// 失败候选既不投票也不阻塞有效候选：胜者只能是正文已校验的那个，
// 且只软比对一次权威（窗口起点由首个有效候选决定，不由失败候选决定）。
func TestCrossRaceWindowStartsAtFirstValidCandidate(t *testing.T) {
	bad := &fakeCand{digest: "", delay: time.Millisecond}         // 失败候选：先返回
	good := &fakeCand{digest: digA, delay: 40 * time.Millisecond} // 有效候选：晚于失败
	var authorityCalls atomic.Int32
	out, err := RunCrossRace(context.Background(), CrossRaceConfig{
		Window:     60 * time.Millisecond,
		FirstRound: 2,
		Authority: func(context.Context) (string, error) {
			authorityCalls.Add(1)
			return digA, nil
		},
	}, []CrossCandidate{
		bad.candidate("quay.io", 1),
		good.candidate("ghcr.io", 2),
	})
	if err != nil {
		t.Fatalf("应形成结论: %v", err)
	}
	// 胜者必须是那个「有效」候选（下标 1），不是先返回的失败候选。
	if out.Winner != 1 || out.Digest != digA {
		t.Fatalf("winner=%d digest=%q", out.Winner, out.Digest)
	}
	if authorityCalls.Load() != 1 {
		t.Fatalf("单有效票必须软比对一次权威，实际 %d", authorityCalls.Load())
	}
	if bad.aborts.Load() != 1 {
		t.Fatalf("落败候选应被 Abort 一次，实际 %d", bad.aborts.Load())
	}
}
