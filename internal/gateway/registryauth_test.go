package gateway

import (
	"fmt"
	"testing"
	"time"
)

// 全都没过期时也必须封顶：key 带 scope，仓库一多就会无限长。
func TestTokenCacheHardLimit(t *testing.T) {
	c := newTokenCache()
	now := time.Now()
	for i := 0; i < tokenCacheMax+80; i++ {
		c.mu.Lock()
		c.pruneLocked()
		c.m[fmt.Sprintf("k%d", i)] = tokenEntry{tok: "t", exp: now.Add(time.Duration(i+1) * time.Hour)}
		c.mu.Unlock()
	}
	if n := len(c.m); n > tokenCacheMax {
		t.Fatalf("token 缓存应封顶 %d，实际 %d", tokenCacheMax, n)
	}
	// 淘汰的是最早到期的那批，最新写入的必须还在。
	if _, ok := c.m["k0"]; ok {
		t.Fatal("最早到期的条目应被淘汰")
	}
	if _, ok := c.m[fmt.Sprintf("k%d", tokenCacheMax+79)]; !ok {
		t.Fatal("最新写入的条目不该被淘汰")
	}
}
