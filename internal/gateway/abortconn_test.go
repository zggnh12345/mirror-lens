package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 已经写出完整 Content-Length 的响应，在发现内容不可信时必须让客户端看到
// 连接异常结束（RST），而不是干净的 EOF。
func TestAbortResponseBreaksConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("0123456789"))
		if f := abortResponse(w); f != nil {
			f()
		}
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err == nil {
		_, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr == nil {
			t.Fatal("应该中断连接，但客户端完整读到了 10 字节")
		}
	}
}

// 不支持 Hijack 的 ResponseWriter 返回 nil，调用方自行降级。
func TestAbortResponseWithoutHijacker(t *testing.T) {
	if f := abortResponse(httptest.NewRecorder()); f != nil {
		t.Fatal("Recorder 不支持 Hijack，应返回 nil")
	}
}
