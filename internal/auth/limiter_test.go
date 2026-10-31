package auth

import (
	"net/http"
	"testing"
	"time"
)

func resetLimiter() {
	loginLimiter.mu.Lock()
	loginLimiter.hits = map[string][]time.Time{}
	loginLimiter.mu.Unlock()
}

// 没有 RemoteAddr 的请求不能全挤在 "" 这一个键上。
func TestEmptyRemoteAddrUsesStableKey(t *testing.T) {
	resetLimiter()
	r := &http.Request{RemoteAddr: ""}
	if !Allowed(r) {
		t.Fatal("新请求应放行")
	}
	loginLimiter.mu.Lock()
	_, hasEmpty := loginLimiter.hits[""]
	_, hasUnknown := loginLimiter.hits["unknown"]
	loginLimiter.mu.Unlock()
	if hasEmpty {
		t.Fatal("不该用空串做限流键")
	}
	if hasUnknown {
		t.Fatal("只是检查一次不该留下记录")
	}
	if ip := clientIP(r); ip != "unknown" {
		t.Fatalf("空 RemoteAddr 应归到 unknown，得到 %q", ip)
	}
}

// 窗口内没有失败记录的 IP 不该在 map 里留空壳，长期运行会一直长。
func TestLimiterDropsStaleKeys(t *testing.T) {
	resetLimiter()
	r := &http.Request{RemoteAddr: "203.0.113.9:5555"}
	for i := 0; i < 8; i++ {
		Fail(r)
	}
	if Allowed(r) {
		t.Fatal("8 次失败后应被限流")
	}
	loginLimiter.mu.Lock()
	loginLimiter.hits["203.0.113.9"] = []time.Time{time.Now().Add(-time.Hour)}
	loginLimiter.mu.Unlock()

	if !Allowed(r) {
		t.Fatal("窗口外的失败不该继续限流")
	}
	loginLimiter.mu.Lock()
	_, ok := loginLimiter.hits["203.0.113.9"]
	loginLimiter.mu.Unlock()
	if ok {
		t.Fatal("过期记录应连键一起删掉")
	}
}
