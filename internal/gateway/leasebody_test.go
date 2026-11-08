package gateway

import (
	"io"
	"strings"
	"testing"
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
