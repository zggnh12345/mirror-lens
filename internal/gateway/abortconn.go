package gateway

import (
	"log"
	"net"
	"net/http"
)

// abortResponse 返回「异常终止本次响应」的函数。
//
// 用途：midfill 已经把字节写给客户端之后才发现内容不可信。此时响应无法撤回，
// 唯一诚实的做法是让连接以错误结束（HTTP/1.x 用 RST），而不是正常收尾，
// 否则客户端会把损坏内容当成完整响应。
//
// HTTP/2 没有 hijack 语义，返回 nil，调用方需自行记录日志；这类客户端
// （Docker/OCI 拉取）本来就带摘要校验，不会静默接受错误内容。
func abortResponse(w http.ResponseWriter) func() {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil
	}
	return func() {
		conn, _, err := hj.Hijack()
		if err != nil {
			log.Printf("中止响应失败: %v", err)
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
	}
}
