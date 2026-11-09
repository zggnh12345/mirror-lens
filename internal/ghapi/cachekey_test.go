package ghapi

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

const oneAsset = `{"assets":[{"name":"Linux.tar.gz","digest":"sha256:AAAA"},{"name":"linux.tar.gz","digest":"sha256:BBBB"}]}`

// owner/repo 不区分大小写：必须共用同一份缓存，只打一次 API。
func TestCacheKeyOwnerRepoCaseInsensitive(t *testing.T) {
	var calls int32
	c := clientWith(t, oneAsset, 200, &calls)
	ctx := context.Background()

	a := c.AssetDigest(ctx, "Owner", "Repo", "v1.0.0", "Linux.tar.gz")
	b := c.AssetDigest(ctx, "owner", "repo", "v1.0.0", "Linux.tar.gz")
	if a.Result != ResultOK || b.Result != ResultOK || a.Hex != b.Hex {
		t.Fatalf("结果异常: %+v / %+v", a, b)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("owner/repo 大小写不同应命中同一缓存，实际请求 %d 次", got)
	}
}

// tag 区分大小写：v1.0.0 与 V1.0.0 必须分别请求。
func TestCacheKeyTagCaseSensitive(t *testing.T) {
	var calls int32
	c := clientWith(t, oneAsset, 200, &calls)
	ctx := context.Background()

	c.AssetDigest(ctx, "o", "r", "v1.0.0", "Linux.tar.gz")
	c.AssetDigest(ctx, "o", "r", "V1.0.0", "Linux.tar.gz")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("tag 大小写不同应各请求一次，实际 %d 次", got)
	}
}

// asset 文件名区分大小写：不能因为缓存键相同而拿到另一份资产的摘要。
func TestCacheKeyAssetCaseSensitive(t *testing.T) {
	var calls int32
	c := clientWith(t, oneAsset, 200, &calls)
	ctx := context.Background()

	a := c.AssetDigest(ctx, "o", "r", "v1.0.0", "Linux.tar.gz")
	b := c.AssetDigest(ctx, "o", "r", "v1.0.0", "linux.tar.gz")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("asset 大小写不同应各请求一次，实际 %d 次", got)
	}
	if a.Hex != "aaaa" || b.Hex != "bbbb" {
		t.Fatalf("两份资产摘要被混用: %q / %q", a.Hex, b.Hex)
	}
}

// 负缓存 2 分钟、正缓存 10 分钟：修复缓存键不能顺手改 TTL。
func TestCacheTTLDefaultsUnchanged(t *testing.T) {
	c := New("tok")
	if c.TTL != 10*time.Minute {
		t.Fatalf("正缓存 TTL = %v，应为 10m", c.TTL)
	}
	if c.NegTTL != 2*time.Minute {
		t.Fatalf("负缓存 TTL = %v，应为 2m", c.NegTTL)
	}
}

// 负结果按 NegTTL 过期后必须重新请求。
func TestNegativeCacheExpires(t *testing.T) {
	var calls int32
	c := clientWith(t, `{"message":"Not Found"}`, 404, &calls)
	c.NegTTL = 10 * time.Millisecond
	ctx := context.Background()

	c.AssetDigest(ctx, "o", "r", "v1", "a.tgz")
	c.AssetDigest(ctx, "o", "r", "v1", "a.tgz")
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("负缓存期内应只请求一次，实际 %d", got)
	}
	time.Sleep(20 * time.Millisecond)
	c.AssetDigest(ctx, "o", "r", "v1", "a.tgz")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("负缓存过期后应重新请求，实际 %d", got)
	}
}
