package ghapi

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

// 401 必须单独归类：Token 过期和限流是两种完全不同的运维动作。
func TestUnauthorizedIsAuthFailed(t *testing.T) {
	c := clientWith(t, `{"message":"Bad credentials"}`, 401, nil)
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	if d.Result != ResultAuthFailed {
		t.Fatalf("401 应归为 auth_failed，实际 %q", d.Result)
	}
}

// 连续 5 次 401 后本进程停用 API：不再发请求，直接返回 auth_failed。
func TestAuthFailuresDisableClient(t *testing.T) {
	var calls int32
	c := clientWith(t, `{"message":"Bad credentials"}`, 401, &calls)

	for i := 0; i < authFailLimit; i++ {
		// 换文件名绕过负结果缓存，模拟真实的不同资产。
		d := c.AssetDigest(context.Background(), "o", "r", "v1", fmt.Sprintf("f%d.tgz", i))
		if d.Result != ResultAuthFailed {
			t.Fatalf("第 %d 次结果 %q", i+1, d.Result)
		}
	}
	if got := atomicLoad(&calls); got != authFailLimit {
		t.Fatalf("停用前应发 %d 次请求，实际 %d", authFailLimit, got)
	}
	if !c.disabled.Load() {
		t.Fatal("连续 401 后应停用")
	}

	// 停用后不再打网络。
	before := atomicLoad(&calls)
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "new.tgz")
	if d.Result != ResultAuthFailed {
		t.Fatalf("停用后结果 %q", d.Result)
	}
	if got := atomicLoad(&calls); got != before {
		t.Fatalf("停用后仍发了 %d 次请求", got-before)
	}
}

// 成功一次就清零连续计数：偶发 401（比如网关抖动）不该累积到熔断。
func TestSuccessResetsAuthFailures(t *testing.T) {
	var calls int32
	// 前 4 次 401，第 5 次成功——用同一个 server 按调用序号切换响应。
	c := clientWith(t, `{"assets":[{"name":"ok.tgz","digest":"sha256:abc123"}]}`, 200, &calls)
	// 手动模拟已累计 4 次失败
	c.authFails.Store(authFailLimit - 1)
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "ok.tgz")
	if d.Result != ResultOK {
		t.Fatalf("应成功，实际 %q", d.Result)
	}
	if got := c.authFails.Load(); got != 0 {
		t.Fatalf("成功后计数应清零，实际 %d", got)
	}
	if c.disabled.Load() {
		t.Fatal("不应被停用")
	}
}

func atomicLoad(p *int32) int32 { return atomic.LoadInt32(p) }
