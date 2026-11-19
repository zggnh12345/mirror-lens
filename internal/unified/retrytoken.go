package unified

import "sync"

// RetryTokens 是 per-Registry、per-RequestScope 的重试令牌表（B16）。
// 顺序路径的每次 Registry 重试必须先取令牌；令牌耗尽不再重试该 Registry，
// 也不允许用「无限重试」把预算打满。
type RetryTokens struct {
	mu   sync.Mutex
	per  int
	used map[string]int
}

// NewRetryTokens 创建令牌表。per<=0 表示每个 Registry 一次。
func NewRetryTokens(per int) *RetryTokens {
	if per <= 0 {
		per = 1
	}
	return &RetryTokens{per: per, used: map[string]int{}}
}

// Take 消耗一个令牌。耗尽返回 ErrRetryTokenSpent。
func (t *RetryTokens) Take(registry string) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.used[registry] >= t.per {
		return ErrRetryTokenSpent
	}
	t.used[registry]++
	return nil
}

// Remaining 返回剩余令牌。
func (t *RetryTokens) Remaining(registry string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.per - t.used[registry]
	if n < 0 {
		return 0
	}
	return n
}

// Used 返回已消耗数。
func (t *RetryTokens) Used(registry string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.used[registry]
}
