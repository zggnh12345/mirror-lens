package gateway

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

// leaseBody 把上游 Body 与 Lease 生命周期绑在一起：
// 关闭时先确认 Body 退出（retire），再释放所有权。
// 顺序不能反：先 Release 再 retire 会让名额提前归还（I4/I8）。
type leaseBody struct {
	io.ReadCloser
	l        *scheduler.Lease
	tok      scheduler.Token
	retire   func()
	once     sync.Once
	closeErr error
	done     chan struct{}
	doneOnce sync.Once
	last     atomic.Int64 // 最近一次读到字节的时间（UnixNano）
}

func (b *leaseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// watch 是单流的最后一道兜底：判慢换源都没救回来时，回收名额并断开。
func (b *leaseBody) watch(limit time.Duration) {
	if b.done == nil { // 零值对象没有生命周期可观测，直接不启动看门狗。
		return
	}
	if limit <= 0 {
		limit = 90 * time.Second
	}
	b.last.Store(time.Now().UnixNano())
	t := time.NewTicker(limit / 6)
	defer t.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, b.last.Load())) <= limit {
				continue
			}
			if b.l != nil {
				log.Printf("单流 %v 无进展，回收名额（源 %d）", limit, b.l.SourceID)
				b.l.Reclaim("stall")
			}
			// 关闭 Body 会一并断开这条上游连接，读侧立刻返回。
			b.ReadCloser.Close()
			return
		}
	}
}

// newLeaseBody 是唯一构造入口：done 必须初始化，否则 Close 会 close(nil) panic。
// midparallel 的同源接管曾经用字面量绕过这里，导致每次接管成功都在 defer Close 崩掉。
func newLeaseBody(rc io.ReadCloser, l *scheduler.Lease, tok scheduler.Token, retire func()) *leaseBody {
	return &leaseBody{ReadCloser: rc, l: l, tok: tok, retire: retire, done: make(chan struct{})}
}

func (b *leaseBody) stopWatch() {
	if b.done == nil { // 防御：零值不是合法构造，但绝不能因此 panic 掉请求。
		return
	}
	b.doneOnce.Do(func() { close(b.done) })
}

// Close 整体只执行一次：底层 Body、retire、Release 都在同一个 Once 里，
// 重复调用返回同一个错误，避免「生命周期只结算一次、底层却关了多次」的歧义。
func (b *leaseBody) Close() error {
	b.once.Do(func() {
		b.stopWatch()
		if b.ReadCloser != nil {
			b.closeErr = b.ReadCloser.Close()
		}
		if b.retire != nil {
			b.retire()
		}
		if b.l != nil {
			b.l.Release(b.tok)
		}
	})
	return b.closeErr
}

// leaseFor 为目标源申请一个名额；官方源（无 Candidate）不记账。
// err 只在名额拿不到时返回（handover 或 non_blocking / 等待超时）。
func (g *Gateway) leaseFor(ctx context.Context, t fetchTarget, kind scheduler.LeaseKind) (*scheduler.Lease, error) {
	if g == nil || g.sched == nil || t.c == nil {
		return nil, nil
	}
	return g.sched.AcquireLease(ctx, t.c.ID, kind)
}

// wrapLease 把响应体包上租约。必须在响应头到达后、读取正文前调用。
func wrapLease(resp *http.Response, l *scheduler.Lease) (*http.Response, error) {
	if l == nil || resp == nil {
		return resp, nil
	}
	retire, err := l.Activate()
	if err != nil {
		return nil, err
	}
	resp.Body = newLeaseBody(resp.Body, l, l.Token(), retire)
	return resp, nil
}

// leaseHolder 挂在请求 ctx 上：midFill 需要知道「当前单流持的是哪张租约」，
// 同源接管才能做 Transfer 而不是新开一个名额。
type leaseHolder struct {
	mu sync.Mutex
	l  *scheduler.Lease
}

type leaseHolderKey struct{}

func withLeaseHolder(ctx context.Context) (context.Context, *leaseHolder) {
	h := &leaseHolder{}
	return context.WithValue(ctx, leaseHolderKey{}, h), h
}

func mustHolder(ctx context.Context) context.Context {
	if holderFrom(ctx) != nil {
		return ctx
	}
	nctx, _ := withLeaseHolder(ctx)
	return nctx
}

func holderFrom(ctx context.Context) *leaseHolder {
	h, _ := ctx.Value(leaseHolderKey{}).(*leaseHolder)
	return h
}

func (h *leaseHolder) set(l *scheduler.Lease) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.l = l
	h.mu.Unlock()
}

func (h *leaseHolder) get() *scheduler.Lease {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.l
}

// releaseLease 用于申请成功但最终没有使用的路径。
func releaseLease(l *scheduler.Lease) {
	if l != nil {
		l.Release(l.Token())
	}
}
