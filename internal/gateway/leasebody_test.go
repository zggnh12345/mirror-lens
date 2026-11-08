package gateway

import (
	"errors"
	"io"
	"strings"
	"testing"

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
