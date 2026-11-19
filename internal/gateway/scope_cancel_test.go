package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/unified"
)

// withScopeCancel 每次派发注册的 AfterFunc 必须随派发结束或 Scope 关闭释放，
// 不能在长 Scope（大文件分片、多轮换源）上按派发次数累积。
func TestScopeCancelCleanupReleased(t *testing.T) {
	sc, err := unified.NewScope(context.Background(), unified.ScopeConfig{}, nil)
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	ctx := unified.WithScope(context.Background(), sc)

	const n = 8
	cancels := make([]context.Context, 0, n)
	for i := 0; i < n; i++ {
		c := withScopeCancel(ctx)
		cancels = append(cancels, c)
	}
	if got := sc.CleanupCount(); got != n {
		t.Fatalf("派发期间应登记 %d 个回调，实际 %d", n, got)
	}

	// 派发结束（父 ctx 取消 / 派发返回）时应立即摘除，不必等 Scope 关闭。
	// 这里只验证 Scope 关闭这条兜底路径：它必须在关闭时全部释放并取消在途派发。
	sc.Close()
	if got := sc.CleanupCount(); got != 0 {
		t.Fatalf("Scope 关闭后必须清空回调，实际残留 %d", got)
	}
	for _, c := range cancels {
		select {
		case <-c.Done():
		case <-time.After(time.Second):
			t.Fatal("Scope 关闭后派发 context 必须被取消")
		}
	}
}

// Scope 耗尽（markExhausted）也要触发派发取消，且不依赖调用方记得 cancel。
func TestScopeCancelFiresOnExhaust(t *testing.T) {
	sc, err := unified.NewScope(context.Background(), unified.ScopeConfig{}, nil)
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	ctx := unified.WithScope(context.Background(), sc)
	c := withScopeCancel(ctx)
	sc.Exhaust("P")
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("预算耗尽必须取消在途派发")
	}
	sc.Close()
	if got := sc.CleanupCount(); got != 0 {
		t.Fatalf("Scope 关闭后必须清空回调，实际残留 %d", got)
	}
}
