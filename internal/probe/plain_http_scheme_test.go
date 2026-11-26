package probe

import (
	"context"
	"testing"
)

// TestPlainHTTPSchemeCaseInsensitive 锁定 3.5.24 §2.3/§2.6：
// 管理员可用 HTTP:// 大写形式配置明文源；必须被识别为 HTTP，
// 不得误补 https:// 而错误执行 TLS 探测。
// 修复前 isPlainHTTP/trimBase 用小写前缀判断，HTTP:// 返回 false → RED。
func TestPlainHTTPSchemeCaseInsensitive(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://example.com", true},
		{"HTTP://example.com", true},
		{"Http://192.0.2.10:5000", true},
		{"https://example.com", false},
		{"HTTPS://example.com", false},
		{"example.com", false},
	}
	for _, tc := range cases {
		if got := isPlainHTTP(tc.in); got != tc.want {
			t.Fatalf("isPlainHTTP(%q)=%v，期望 %v", tc.in, got, tc.want)
		}
	}
	// trimBase 不得把大写 HTTP:// 误补成 https://。
	if got := trimBase("HTTP://example.com/v2/"); got != "HTTP://example.com/v2" {
		t.Fatalf("trimBase(大写 HTTP)=%q，期望保留 http scheme", got)
	}
}

// TestTLSNotProbedForUppercaseHTTP 锁定：大写 HTTP 源不得进入 TLS 探测路径。
func TestTLSNotProbedForUppercaseHTTP(t *testing.T) {
	// isPlainHTTP 判定为真即表示调用方走「不做 TLS 探测」分支。
	if !isPlainHTTP("HTTP://example.com") {
		t.Fatal("大写 HTTP 源被误判为需要 TLS 探测")
	}
	// DockerWithImage 对 HTTP 源置 TLSOK=true 表示"不适用"；这里只验证判定路径，
	// 不发真实网络请求（nil context 探活会失败，但 TLSOK 在探测前已赋值）。
	_ = context.Background()
}
