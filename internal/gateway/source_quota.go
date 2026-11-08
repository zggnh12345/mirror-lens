package gateway

import (
	"context"
	"sync"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// B 档与预检的独立配额。它们不进 max_per_source 账本：
//   - 预检是 1 字节探测，用全局在途数控制，拿不到就跳过这台源；
//   - 中转竞速按排名截断（不排队）；
//   - Git upload-pack 在 POST 前取名额，响应体关闭才归还。
const (
	defaultPreflightConcurrency = 4
	defaultRelayConcurrency     = 4
	defaultGitConcurrency       = 4
)

func (g *Gateway) initSems() {
	pre := g.par.PreflightConcurrency
	if pre <= 0 {
		pre = defaultPreflightConcurrency
	}
	relay := g.par.RelayConcurrency
	if relay <= 0 {
		relay = defaultRelayConcurrency
	}
	git := g.par.GitConcurrency
	if git <= 0 {
		git = defaultGitConcurrency
	}
	g.preSem = make(chan struct{}, pre)
	g.relayLimit = relay
	g.gitSem = make(chan struct{}, git)
}

func (g *Gateway) sems() {
	g.semOnce.Do(g.initSems)
}

// acquirePreflight 非阻塞：拿不到就跳过这台源的预检，不排队。
func (g *Gateway) acquirePreflight() bool {
	g.sems()
	select {
	case g.preSem <- struct{}{}:
		metrics.PreflightInflight(1)
		return true
	default:
		metrics.PreflightCapped()
		return false
	}
}

func (g *Gateway) releasePreflight() {
	<-g.preSem
	metrics.PreflightInflight(-1)
}

func (g *Gateway) relayCap() int {
	g.sems()
	return g.relayLimit
}

// acquireGit 在 POST 之前取名额；ctx 结束就放弃，避免请求堆积。
func (g *Gateway) acquireGit(ctx context.Context) (func(), error) {
	g.sems()
	select {
	case g.gitSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	metrics.GitInflight(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			<-g.gitSem
			metrics.GitInflight(-1)
		})
	}, nil
}
