package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

// leaseBody 的 done channel 必须由构造函数初始化：
// stopWatch 无条件 close(b.done)，零值 leaseBody 一旦 Close 就 panic。
func TestLeaseBodyCloseWithNilDone(t *testing.T) {
	b := &leaseBody{ReadCloser: io.NopCloser(strings.NewReader(""))}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Close 对未初始化的 leaseBody panic: %v", r)
		}
	}()
	_ = b.Close()
}

type countingCloser struct {
	closes int
	err    error
}

func (c *countingCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (c *countingCloser) Close() error {
	c.closes++
	return c.err
}

// Close 整体幂等：底层 Body、retire、Release 都只执行一次，重复调用返回同一错误。
func TestLeaseBodyCloseIsIdempotent(t *testing.T) {
	boom := errors.New("boom")
	rc := &countingCloser{err: boom}
	var retired int
	b := newLeaseBody(rc, nil, scheduler.Token{}, func() { retired++ })
	if b.done == nil {
		t.Fatal("newLeaseBody 没有初始化 done")
	}
	if err := b.Close(); err != boom {
		t.Fatalf("第一次 Close 错误 %v，期望 %v", err, boom)
	}
	if err := b.Close(); err != boom {
		t.Fatalf("第二次 Close 错误 %v，期望与第一次一致", err)
	}
	if rc.closes != 1 {
		t.Fatalf("底层 Body 关闭 %d 次，期望 1", rc.closes)
	}
	if retired != 1 {
		t.Fatalf("retire 调用 %d 次，期望 1", retired)
	}
}

// 看门狗回收后外层 defer 再关闭：底层 Body / retire / Release 都必须只结算一次。
// watch 曾经直接 b.ReadCloser.Close()，绕过了 Once，导致底层被关两次、retire 漏掉。
func TestLeaseBodyWatchCloseIsOnce(t *testing.T) {
	f := newMidFixture(t, 1)
	defer f.close()
	metrics.Enable()
	t.Cleanup(metrics.Disable)

	const srcID = int64(91)
	sched := scheduler.New(f.st, scheduler.Config{Enabled: true, MaxPerSource: 4})
	l, err := sched.AcquireLease(context.Background(), srcID, scheduler.KindNormal)
	if err != nil || l == nil {
		t.Fatalf("acquire lease: %v", err)
	}
	var retired int
	retire, err := l.Activate()
	if err != nil {
		t.Fatal(err)
	}
	rc := &countingCloser{}
	b := newLeaseBody(rc, l, l.Token(), func() { retired++; retire() })

	go b.watch(60 * time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for l.State() != scheduler.StateReleased && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := l.State(); st != scheduler.StateReleased {
		t.Fatalf("看门狗应回收 Lease，状态 %s", st)
	}
	// 外层 handler 的 defer 随后到达。
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if rc.closes != 1 {
		t.Fatalf("底层 Body 关闭 %d 次，期望 1", rc.closes)
	}
	if retired != 1 {
		t.Fatalf("retire 调用 %d 次，期望 1", retired)
	}
	if got := leaseGauge(t, "leases_held", srcID); got != 0 {
		t.Fatalf("看门狗回收后 held=%v，期望 0", got)
	}
}
