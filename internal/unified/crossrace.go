package unified

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// 3.5.17 B 层：跨站可信竞速（B 模式）。
//
// 边界（冻结）：
//   - 竞速只决定「谁先提供可信 Manifest」，不改身份、认证、内容完整性与缓存语义；
//   - M 个候选中先并发 C 路；只有两路独立 Digest 一致才能直接交付；
//   - 不一致时不猜、不择优：要么用官方权威 Digest 对照，要么顺序补位；
//   - 一次请求最多问官方权威一次（Authority 单飞），不做链式追认；
//   - 未通过投票/权威对照的内容一律不交付，交由调用方走原有顺序回退。

// ErrCrossRaceNoCandidate 表示没有任何候选形成可信结论。
var ErrCrossRaceNoCandidate = errors.New("unified: 跨站竞速没有形成可信结论")

// CrossCandidate 是一次跨站候选尝试。
//
// Run 负责真正取回并校验内容，返回内容寻址的 Digest（Manifest 场景下
// 就是正文 SHA-256）。Abort 用于释放未当选候选占用的在途资源。
type CrossCandidate struct {
	Registry string
	SourceID int64
	Run      func(ctx context.Context) (digest string, err error)
	Abort    func()
}

// CrossRaceConfig 是竞速参数。零值走冻结默认。
type CrossRaceConfig struct {
	// MaxCandidates 是候选上限（M）。默认 4，超过按顺序截断。
	MaxCandidates int
	// FirstRound 是首轮并发数（C）。默认 2，上限 2：
	// 超过 2 会削弱「两路互相印证」的独立性，并且抬高并发预算。
	FirstRound int
	// Window 是投票宽限窗口，默认 200ms。
	Window time.Duration
	// Authority 返回官方权威 Digest（只会被调用一次）。为空表示
	// 没有权威来源，此时只能依赖两路一致；不一致就整体放弃。
	Authority func(ctx context.Context) (string, error)
}

func (c CrossRaceConfig) normalized() CrossRaceConfig {
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = 4
	}
	if c.FirstRound <= 0 {
		c.FirstRound = 2
	}
	if c.FirstRound > 2 {
		c.FirstRound = 2
	}
	if c.MaxCandidates < c.FirstRound {
		c.MaxCandidates = c.FirstRound
	}
	if c.Window <= 0 {
		c.Window = VoteWindow
	}
	return c
}

// CrossRaceOutcome 是竞速结论。Winner < 0 表示没有当选候选。
type CrossRaceOutcome struct {
	State  VoteState
	Winner int
	Digest string
	// Truncated 是被 M 截断掉的候选数（诊断用，不影响结论）。
	Truncated int
	// AuthorityUsed 表示本次是否真的问了官方权威。
	AuthorityUsed bool
}

type crossVote struct {
	mu      sync.Mutex
	results []crossResult
	done    chan struct{}
	once    sync.Once
}

type crossResult struct {
	idx    int
	digest string
	err    error
}

// RunCrossRace 执行 B 层竞速。
//
// 返回的 outcome.Winner 是调用方应当使用的候选下标；其余候选已被 Abort。
// 返回 ErrCrossRaceNoCandidate 时调用方必须按原有顺序回退，不能把
// 「竞速无结论」当成失败写进源健康（这是中性事件）。
func RunCrossRace(ctx context.Context, cfg CrossRaceConfig, cands []CrossCandidate) (CrossRaceOutcome, error) {
	cfg = cfg.normalized()
	out := CrossRaceOutcome{Winner: -1}
	if len(cands) == 0 {
		return out, ErrCrossRaceNoCandidate
	}
	if len(cands) > cfg.MaxCandidates {
		out.Truncated = len(cands) - cfg.MaxCandidates
		cands = cands[:cfg.MaxCandidates]
	}
	abortExcept := func(keep int) {
		for i := range cands {
			if i == keep || cands[i].Abort == nil {
				continue
			}
			cands[i].Abort()
		}
	}

	// 单候选没有「独立印证」可言，只能请权威裁定。
	if len(cands) == 1 {
		d, err := runCandidate(ctx, cands[0])
		if err != nil || d == "" {
			abortExcept(-1)
			return out, ErrCrossRaceNoCandidate
		}
		out.AuthorityUsed = true
		ok, aerr := compareAuthority(ctx, cfg, d)
		if aerr != nil || !ok {
			abortExcept(-1)
			return out, ErrCrossRaceNoCandidate
		}
		out.State = VoteAuthorityCompare
		out.Winner = 0
		out.Digest = d
		return out, nil
	}

	vote := NewManifestVote(cfg.Window)
	first := cfg.FirstRound
	if first > len(cands) {
		first = len(cands)
	}
	// 首轮并发。每个结果都投票；投票关闭后不再改变结论。
	firstRes := make([]crossResult, first)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < first; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := runCandidate(ctx, cands[i])
			mu.Lock()
			firstRes[i] = crossResult{idx: i, digest: d, err: err}
			mu.Unlock()
			if err != nil || d == "" {
				return
			}
			// Run 的契约是「正文已读完且 Digest 已校验」，因此这里投票
			// 可以直接标记 Verified/Complete；只报响应头的候选不许投票。
			vote.Offer(VoteCandidate{
				Registry: cands[i].Registry,
				SourceID: cands[i].SourceID,
				Digest:   d,
				Verified: true,
				Complete: true,
			})
		}(i)
	}
	wg.Wait()
	state := vote.CloseWindow()

	// 零有效票：没有任何可对照的正文，不打扰官方权威，直接交给顺序回退。
	if vote.Votes() == 0 {
		out.State = state
		abortExcept(-1)
		return out, ErrCrossRaceNoCandidate
	}

	switch state {
	case VoteConfirmed:
		agreed := vote.Agreed()
		// 选出第一个与共识 Digest 一致的候选（确定性顺序，不按完成时间）。
		// 复用首轮结果：同一候选不得因为胜负判定被请求两次。
		for i := 0; i < first; i++ {
			res := firstRes[i]
			if res.err == nil && res.digest == agreed {
				out.State = VoteConfirmed
				out.Winner = i
				out.Digest = agreed
				abortExcept(i)
				return out, nil
			}
		}
		out.State = state
		abortExcept(-1)
		return out, ErrCrossRaceNoCandidate

	case VoteMismatch, VoteWindowClosed, VoteAuthorityCompare:
		// 正文已经取到但结论不成立：先问一次权威，再用剩余候选顺序补位。
		// 补位只接受与权威 Digest 一致的内容——「换个源再试」不能降低完整性。
		ad, aerr := authorityDigest(ctx, cfg)
		if aerr != nil || ad == "" {
			out.State = state
			abortExcept(-1)
			return out, ErrCrossRaceNoCandidate
		}
		out.AuthorityUsed = true
		// 先看首轮已拿到的正文是否就与权威一致（不再重复请求）。
		for i := 0; i < first; i++ {
			if res := firstRes[i]; res.err == nil && res.digest == ad {
				out.State = VoteAuthorityCompare
				out.Winner = i
				out.Digest = ad
				abortExcept(i)
				return out, nil
			}
		}
		// 顺序补位段：一次只跑一个，避免用并发掩盖不一致。
		for i := first; i < len(cands); i++ {
			if d, err := runCandidate(ctx, cands[i]); err == nil && d == ad {
				out.State = VoteAuthorityCompare
				out.Winner = i
				out.Digest = ad
				abortExcept(i)
				return out, nil
			}
		}
		out.State = state
		abortExcept(-1)
		return out, ErrCrossRaceNoCandidate
	}

	out.State = state
	abortExcept(-1)
	return out, ErrCrossRaceNoCandidate
}

func runCandidate(ctx context.Context, c CrossCandidate) (string, error) {
	if c.Run == nil {
		return "", fmt.Errorf("候选没有 Run")
	}
	return c.Run(ctx)
}

func authorityDigest(ctx context.Context, cfg CrossRaceConfig) (string, error) {
	if cfg.Authority == nil {
		return "", fmt.Errorf("没有权威来源")
	}
	return cfg.Authority(ctx)
}

func compareAuthority(ctx context.Context, cfg CrossRaceConfig, d string) (bool, error) {
	ad, err := authorityDigest(ctx, cfg)
	if err != nil {
		return false, err
	}
	return ad != "" && ad == d, nil
}
