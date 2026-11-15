package metrics

import (
	"strings"
	"testing"
)

func TestTokenCacheMetrics(t *testing.T) {
	setup(t)
	TokenCacheCapacity(1024)
	TokenCacheEntries(3)
	TokenCacheHit()
	TokenCacheHit()
	TokenCacheMiss()
	TokenCacheEvicted(2)

	out := string(Get().render())
	for _, want := range []string{
		"mirrorlens_auth_cache_hits_total 2",
		"mirrorlens_auth_cache_misses_total 1",
		"mirrorlens_auth_cache_evictions_total 2",
		"mirrorlens_auth_cache_entries 3",
		"mirrorlens_auth_cache_capacity 1024",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
}
