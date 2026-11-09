package store

import (
	"testing"
	"time"
)

func TestColdLayerStats(t *testing.T) {
	st := openBlobTest(t)
	now := time.Now().Unix()
	day := int64(86400)

	rows := []CacheEntry{
		// 冷：7 天前最后一次命中
		{Key: "docker:blob:sha256:aaa", SHA256: "aaa", Size: 1000, CreatedAt: now - 30*day, LastHit: now - 10*day, TTL: 0},
		// 冷：从未命中
		{Key: "docker:blob:sha256:bbb", SHA256: "bbb", Size: 500, CreatedAt: now - 2*day, LastHit: 0, TTL: 0},
		// 热：昨天命中
		{Key: "docker:blob:sha256:ccc", SHA256: "ccc", Size: 300, CreatedAt: now - 5*day, LastHit: now - day, TTL: 0},
		// 非 layer：不该被算进分母
		{Key: "docker-manifest:nginx:latest", SHA256: "ddd", Size: 9000, CreatedAt: now - 30*day, LastHit: 0, TTL: 300},
	}
	for _, e := range rows {
		if err := st.CachePut(e); err != nil {
			t.Fatal(err)
		}
	}

	cold, total, coldN, totalN, err := st.ColdLayerStats(now - 7*day)
	if err != nil {
		t.Fatal(err)
	}
	if cold != 1500 || total != 1800 {
		t.Fatalf("字节不对: cold=%d total=%d", cold, total)
	}
	if coldN != 2 || totalN != 3 {
		t.Fatalf("条目数不对: cold=%d total=%d", coldN, totalN)
	}
}
