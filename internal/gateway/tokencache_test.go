package gateway

import (
	"testing"
	"time"
)

// 过期的 token 不能一直堆在 map 里（key 带 scope，每个仓库一条）。
func TestTokenCachePrunesExpired(t *testing.T) {
	c := newTokenCache()
	c.mu.Lock()
	for i := 0; i < tokenCacheMax+40; i++ {
		c.m[string(rune('a'+i%26))+string(rune('a'+i/26))] = tokenEntry{
			tok: "old",
			exp: time.Now().Add(-time.Minute),
		}
	}
	c.pruneLocked()
	c.mu.Unlock()

	if n := len(c.m); n != 0 {
		t.Fatalf("过期的 token 应被清空，还剩 %d 条", n)
	}
}

// 未过期的条目要保住。
func TestTokenCacheKeepsFresh(t *testing.T) {
	c := newTokenCache()
	c.mu.Lock()
	for i := 0; i < tokenCacheMax; i++ {
		c.m[string(rune('a'+i%26))+string(rune('a'+i/26))] = tokenEntry{
			tok: "fresh",
			exp: time.Now().Add(time.Minute),
		}
	}
	c.m["keep"] = tokenEntry{tok: "keep", exp: time.Now().Add(time.Hour)}
	c.pruneLocked()
	c.mu.Unlock()

	if _, ok := c.m["keep"]; !ok {
		t.Fatal("未过期的 token 不该被清掉")
	}
}
