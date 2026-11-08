package gateway

import (
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

// 同源接管拿到的 Body 必须已完成初始化，且重复 Close 只结算一次。
func TestNewLeaseBodyClosesOnce(t *testing.T) {
	var released int
	b := newLeaseBody(io.NopCloser(strings.NewReader("x")), nil, scheduler.Token{}, func() { released++ })
	if b.done == nil {
		t.Fatal("newLeaseBody 没有初始化 done")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("retire 调用 %d 次，期望 1", released)
	}
}
