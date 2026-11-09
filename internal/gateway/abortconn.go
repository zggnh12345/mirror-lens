package gateway

import (
	"log"
	"net"
	"net/http"
	"sync"
)

// abortResponse 返回「异常终止本次响应」的函数，以及当前 writer 是否支持这种终止。
//
// 用途：midfill 已经把字节写给客户端之后才发现内容不可信。此时响应无法撤回，
// 唯一诚实的做法是让连接以错误结束（HTTP/1.x 用 RST），而不是正常收尾，
// 否则客户端会把损坏内容当成完整响应。
//
// 必须走 http.NewResponseController：生产链路上 metrics.Middleware 会把 writer
// 包成 recorder，它不实现 Hijacker，但实现了 Unwrap；直接做类型断言会永远失败。
//
// HTTP/2 没有 hijack 语义，此时返回 ok=false，调用方必须拒绝接管 midfill——
// 不能边填边发却无法在失败时中止。
func abortResponse(w http.ResponseWriter) (kill func(), ok bool) {
	if !supportsHijack(w) {
		return nil, false
	}
	var once sync.Once
	return func() {
		// 只允许关闭一次：defer 与显式调用都可能走到这里。
		once.Do(func() {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				log.Printf("中止响应失败: %v", err)
				return
			}
			// Hijack 之后连接归调用方所有，http.Server 不会再碰它。
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // 用 RST 而不是 FIN，避免对端把截断当正常结束
			}
			_ = conn.Close()
		})
	}, true
}

// supportsHijack 沿 Unwrap 链找 Hijacker，不依赖任何一层中间件手工转发。
func supportsHijack(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Hijacker); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
		if w == nil {
			return false
		}
	}
}
