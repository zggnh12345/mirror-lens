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
			// 走统一的 Close：底层关闭、retire、Release 都在同一个 Once 里，
			// 外层 defer Close 随后到达时不会重复关闭底层 Body。
			_ = b.Close()
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

// chunkLease 给分片请求同时记两笔账：Lease 名额（Body 进 bodies 账本）和
// source_inflight（在途请求）。finish 是所有出口的唯一结算点，保证各只减一次。
type chunkLease struct {
	l    *scheduler.Lease
	tok  scheduler.Token
	id   int64
	body io.ReadCloser
	done func() // 闸门 + 在途计数，AcquireBody 交回

	unbind   func()
	acancel  context.CancelFunc
	dropOnce sync.Once
	once     sync.Once
}

// bindAttempt 把本次 Attempt 的取消函数登记到 Lease：
// 顺序固定为 unbind → cancel → Release，任何出口都只做一次。
// 返回 false 表示名额已被回收（登记即取消），调用方不得继续拨号。
func (c *chunkLease) bindAttempt(actx context.Context, cancel context.CancelFunc) bool {
	if c == nil || c.l == nil || cancel == nil {
		return true
	}
	c.acancel = cancel
	c.unbind = c.l.BindCancel(cancel)
	// BindCancel 对已失效的名额会立即 cancel，必须回查再决定是否拨号。
	return actx.Err() == nil
}

func (c *chunkLease) unbindOnly() {
	if c == nil {
		return
	}
	c.dropOnce.Do(func() {
		if c.unbind != nil {
			c.unbind()
			c.unbind = nil
		}
	})
}

func (c *chunkLease) dropAttempt() {
	if c == nil {
		return
	}
	c.dropOnce.Do(func() {
		if c.unbind != nil {
			c.unbind()
			c.unbind = nil
		}
		if c.acancel != nil {
			c.acancel()
			c.acancel = nil
		}
	})
}

func (g *Gateway) openChunkLease(ctx context.Context, t fetchTarget) (*chunkLease, error) {
	if g == nil || g.sched == nil || t.c == nil {
		return nil, nil
	}
	l, tok, done, err := g.sched.AcquireBody(ctx, t.c.ID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		if done != nil {
			done() // 没有名额可托管，立即归还在途
		}
		return nil, nil
	}
	return &chunkLease{l: l, tok: tok, id: t.c.ID, done: done}, nil
}

// attach 在响应头校验通过后调用：Body 正式进入 Lease 生命周期账本。
// 约定：要么接管 Body，要么把它关掉——失败时绝不留半开的连接。
func (c *chunkLease) attach(resp *http.Response) error {
	retire, err := c.l.Activate()
	if err != nil {
		_ = resp.Body.Close()
		return err
	}
	// Body 退出时先解绑，避免已结束的 Attempt 还挂在 Lease 上；
	// Context 的取消交给 finish/Close 的收尾顺序处理。
	inner := retire
	retire = func() {
		c.unbindOnly()
		inner()
	}
	c.body = newLeaseBody(resp.Body, c.l, c.tok, retire)
	resp.Body = c.body
	return nil
}

// finish 处理所有出口：已接管 Body 走 Close（retire+Release），
// 未接管时关闭调用方给的原始 Body 并直接 Release——绝不留半开连接。
func (c *chunkLease) finish(rc io.ReadCloser) {
	if c == nil {
		if rc != nil {
			_ = rc.Close()
		}
		return
	}
	c.once.Do(func() {
		c.dropAttempt()
		if c.body != nil {
			_ = c.body.Close()
		} else {
			if rc != nil {
				_ = rc.Close()
			}
			c.l.Release(c.tok)
		}
		if c.done != nil {
			c.done()
		}
	})
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
