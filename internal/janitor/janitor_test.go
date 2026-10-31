package janitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/gc"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func setup(t *testing.T, limit int64) (*store.Store, *cache.Store, *gc.GC, *Janitor) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}
	g := gc.New(st, cs)
	j := New(st, cs, g, Options{
		Interval:      10 * time.Minute,
		PartialTTL:    24 * time.Hour,
		ExpiredGrace:  72 * time.Hour,
		CacheMaxBytes: limit,
		WarnAt:        0.9,
		RecoverAt:     0.85,
	})
	return st, cs, g, j
}

// put 造一条索引 + 一份真实文件，返回 sha。
func put(t *testing.T, st *store.Store, cs *cache.Store, key string, body []byte, created int64, ttl int64) string {
	t.Helper()
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	tmp, err := cs.CreateTemp()
	if err != nil {
		t.Fatalf("临时文件: %v", err)
	}
	if _, err := tmp.Write(body); err != nil {
		t.Fatalf("写: %v", err)
	}
	tmp.Close()
	if _, err := cs.Commit(tmp.Name(), sha); err != nil {
		t.Fatalf("提交: %v", err)
	}
	if err := st.CachePut(store.CacheEntry{
		Key: key, SHA256: sha, Size: int64(len(body)),
		ContentType: "application/octet-stream", CreatedAt: created, TTL: ttl,
	}); err != nil {
		t.Fatalf("写索引: %v", err)
	}
	return sha
}

// 索引在、文件不在 → 删索引，否则用户命中后拿到 502。
func TestReconcileDropsIndexWithoutFile(t *testing.T) {
	st, cs, _, j := setup(t, 0)
	now := time.Now().Unix()
	key := "raw:o/r/main/a.txt"
	sha := put(t, st, cs, key, []byte("hello"), now, 300)
	if err := cs.Remove(sha); err != nil {
		t.Fatalf("删文件: %v", err)
	}
	s := j.Once()
	if s.Missing != 1 {
		t.Fatalf("应发现 1 条缺文件，实际 %d", s.Missing)
	}
	if e, _ := st.CacheGet(key); e != nil {
		t.Fatal("索引没被删")
	}
}

// 文件在、没索引引用 → 删文件（崩在 Commit 与 CachePut 之间会留下这种）。
//
// 但要先放过刚落的盘：提交是「先 rename、后写索引」，refs 快照又比扫盘更早，
// 扫描期间新提交的 blob 看上去就是孤儿。等过了观察期再收。
func TestReconcileDropsOrphanBlob(t *testing.T) {
	_, cs, _, j := setup(t, 0)
	sum := sha256.Sum256([]byte("orphan"))
	sha := hex.EncodeToString(sum[:])
	tmp, _ := cs.CreateTemp()
	tmp.Write([]byte("orphan"))
	tmp.Close()
	if _, err := cs.Commit(tmp.Name(), sha); err != nil {
		t.Fatalf("提交: %v", err)
	}

	if s := j.Once(); s.Orphans != 0 || !cs.Has(sha) {
		t.Fatalf("刚落盘的 blob 应留到下一轮，实际 orphans=%d", s.Orphans)
	}

	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(cs.BlobPath(sha), old, old); err != nil {
		t.Fatalf("改 mtime: %v", err)
	}
	s := j.Once()
	if s.Orphans != 1 {
		t.Fatalf("过了观察期应清 1 个孤儿，实际 %d", s.Orphans)
	}
	if cs.Has(sha) {
		t.Fatal("孤儿文件还在")
	}
}

// 过期了但最近有人访问，不能删；过期且超过宽限期没人碰才删。
func TestExpiredRespectsGrace(t *testing.T) {
	st, cs, _, j := setup(t, 0)
	now := time.Now().Unix()
	old := now - 10*86400
	cold := "raw:o/r/main/cold.txt"
	hot := "raw:o/r/main/hot.txt"
	put(t, st, cs, cold, []byte("cold"), old, 300)
	put(t, st, cs, hot, []byte("hot"), old, 300)
	st.CacheHit(hot) // 刚被用过

	s := j.Once()
	if s.Expired != 1 {
		t.Fatalf("应清 1 条过期，实际 %d", s.Expired)
	}
	if e, _ := st.CacheGet(cold); e != nil {
		t.Fatal("过期且久未访问的没被清")
	}
	if e, _ := st.CacheGet(hot); e == nil {
		t.Fatal("刚访问过的过期条目被误删")
	}
}

// 半截文件：正在被写的不能删，关掉以后才清，而且 .part 与 .json 必须成对走。
func TestPartialProtectedWhileWriting(t *testing.T) {
	_, cs, _, _ := setup(t, 0)
	key := "http:example.com/big.iso"
	f, err := cs.OpenPartial(key)
	if err != nil {
		t.Fatalf("开半截文件: %v", err)
	}
	f.Write(bytes.Repeat([]byte("x"), 1024))
	if err := cs.WritePartial(key, cache.PartialMeta{
		URL: "https://example.com/big.iso", ETag: `"v1"`, Size: 1024, Have: 1024,
		Updated: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("存进度: %v", err)
	}
	// 把它做旧，模拟"24 小时没动过"
	part, meta := cs.PartialPaths(key)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(part, old, old)
	os.Chtimes(meta, old, old)

	if n, _, _ := cs.SweepPartial(time.Now().Add(-24 * time.Hour).Unix()); n != 0 {
		t.Fatalf("正在写的半截文件被删了 %d 个", n)
	}
	if _, ok := cs.ReadPartial(key); !ok {
		t.Fatal("活动下载的半截文件不该消失")
	}

	// 写端关掉（释放文件锁）后再扫
	f.Close()
	if n, _, _ := cs.SweepPartial(time.Now().Add(-24 * time.Hour).Unix()); n != 2 {
		t.Fatalf("应删 .part 与 .json 两个文件，实际 %d", n)
	}
	if _, ok := cs.ReadPartial(key); ok {
		t.Fatal("半截文件没清干净")
	}
}

// 水位：90% 报一次，重复巡检不重复报，回落到 85% 以下才重新武装。
func TestWatermarkWarnOnceAndRearm(t *testing.T) {
	st, cs, g, j := setup(t, 1000)
	now := time.Now().Unix()
	put(t, st, cs, "raw:o/r/main/big", bytes.Repeat([]byte("a"), 950), now, 3600)

	s := j.Once()
	if !s.Warned {
		t.Fatal("950/1000 应该触发告警")
	}
	if v, _ := st.MetaGet(metaWarnKey); v != "1" {
		t.Fatalf("告警状态没落库: %q", v)
	}

	// 再跑一轮：状态不变，不应重新写库/重复打印
	if s2 := j.Once(); !s2.Warned {
		t.Fatal("第二轮不该复位")
	}

	// 清空后回落到 85% 以下 → 复位
	if _, err := g.DeleteKeys([]string{"raw:o/r/main/big"}); err != nil {
		t.Fatalf("清: %v", err)
	}
	if s3 := j.Once(); s3.Warned {
		t.Fatal("用量归零后告警应复位")
	}
	if v, _ := st.MetaGet(metaWarnKey); v != "0" {
		t.Fatalf("复位状态没落库: %q", v)
	}
}

// 并发巡检不能把同一份内容删两遍，也不能删掉别人刚建的索引。
func TestConcurrentSweepsAreSafe(t *testing.T) {
	st, cs, g, _ := setup(t, 0)
	now := time.Now().Unix()
	body := bytes.Repeat([]byte("s"), 64)
	put(t, st, cs, "raw:o/r/main/a", body, now, 300)
	put(t, st, cs, "raw:o/r/main/b", body, now, 300)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = g.DeleteKeys([]string{"raw:o/r/main/a", "raw:o/r/main/b"})
			_, _ = g.Orphans()
			_, _ = g.RetryPending()
		}()
	}
	wg.Wait()

	if _, err := os.Stat(cs.BlobPath(func() string {
		s := sha256.Sum256(body)
		return hex.EncodeToString(s[:])
	}())); err == nil {
		t.Fatal("两份索引都删了，文件应该也不在了")
	}
}

// 按协议清理只动自己那一类。
func TestPurgeScopeOnlyTouchesOneKind(t *testing.T) {
	st, cs, g, _ := setup(t, 0)
	now := time.Now().Unix()
	put(t, st, cs, "raw:o/r/main/a", []byte("raw"), now, 300)
	put(t, st, cs, "http:example.com/a", []byte("http"), now, 300)

	raws, err := st.CacheKeysByPrefix("raw:")
	if err != nil || len(raws) != 1 {
		t.Fatalf("按前缀取: %v %d", err, len(raws))
	}
	if _, err := g.DeleteKeys([]string{raws[0].Key}); err != nil {
		t.Fatalf("删: %v", err)
	}
	if e, _ := st.CacheGet("raw:o/r/main/a"); e != nil {
		t.Fatal("raw 没清掉")
	}
	if e, _ := st.CacheGet("http:example.com/a"); e == nil {
		t.Fatal("清 raw 把 http 也清了")
	}
}
