package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// putEntry 造一条缓存记录 + 一份真实文件，返回 key。
func putEntry(t *testing.T, st *store.Store, cs *cache.Store, key string, body []byte, at int64) string {
	t.Helper()
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	tmp, err := cs.CreateTemp()
	if err != nil {
		t.Fatalf("临时文件: %v", err)
	}
	if _, err := tmp.Write(body); err != nil {
		t.Fatalf("写入: %v", err)
	}
	tmp.Close()
	if _, err := cs.Commit(tmp.Name(), sha); err != nil {
		t.Fatalf("提交: %v", err)
	}
	if err := st.CachePut(store.CacheEntry{
		Key: key, SHA256: sha, Size: int64(len(body)),
		ContentType: "application/octet-stream", CreatedAt: at, TTL: 300,
	}); err != nil {
		t.Fatalf("写记录: %v", err)
	}
	return key
}

func TestQuotaEvictsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("库: %v", err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}
	now := time.Now().Unix()
	old := putEntry(t, st, cs, "k-old", bytes.Repeat([]byte("a"), 100), now-300)
	mid := putEntry(t, st, cs, "k-mid", bytes.Repeat([]byte("b"), 100), now-200)
	newKey := putEntry(t, st, cs, "k-new", bytes.Repeat([]byte("c"), 100), now-100)

	// 上限 250：只够留两个，最老的必须走
	gw := New(st, Options{Cache: cs, MaxBytes: 1 << 20, CacheMaxBytes: 250,
		Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})
	if freed, err := gw.EvictTo(250); err != nil || freed != 100 {
		t.Fatalf("应释放 100 字节，实际 %d (%v)", freed, err)
	}
	if e, _ := st.CacheGet(old); e != nil {
		t.Fatal("最久没访问的没被淘汰")
	}
	if e, _ := st.CacheGet(mid); e == nil {
		t.Fatal("次新的不该被淘汰")
	}
	if e, _ := st.CacheGet(newKey); e == nil {
		t.Fatal("最新的不该被淘汰")
	}
	if _, used, _ := st.CacheTotals(); used != 200 {
		t.Fatalf("剩余应是 200，实际 %d", used)
	}
}

// 命中过的条目算"最近用过"，不该先被淘汰。
func TestQuotaRespectsLastHit(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "t.db"))
	defer st.Close()
	cs, _ := cache.New(filepath.Join(dir, "cache"))
	now := time.Now().Unix()
	old := putEntry(t, st, cs, "k-old", bytes.Repeat([]byte("a"), 100), now-300)
	putEntry(t, st, cs, "k-new", bytes.Repeat([]byte("b"), 100), now-100)
	st.CacheHit(old) // 刚刚被用过

	gw := New(st, Options{Cache: cs, MaxBytes: 1 << 20, CacheMaxBytes: 150,
		Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})
	gw.EvictTo(150)

	if e, _ := st.CacheGet(old); e == nil {
		t.Fatal("刚命中过的被淘汰了，LRU 没按 last_hit 走")
	}
	if e, _ := st.CacheGet("k-new"); e != nil {
		t.Fatal("没被访问过的反而留着")
	}
}

// 两个键指向同一份内容时，淘汰其中一个不能把文件删了。
// 物理占用只算一份，所以这里再放一个独立内容，让淘汰真的会往下走。
func TestQuotaKeepsSharedBlob(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "t.db"))
	defer st.Close()
	cs, _ := cache.New(filepath.Join(dir, "cache"))
	now := time.Now().Unix()
	shared := bytes.Repeat([]byte("z"), 100)
	a := putEntry(t, st, cs, "k-a", shared, now-300)
	other := putEntry(t, st, cs, "k-other", bytes.Repeat([]byte("y"), 100), now-250)
	b := putEntry(t, st, cs, "k-b", shared, now-200)

	gw := New(st, Options{Cache: cs, MaxBytes: 1 << 20, CacheMaxBytes: 150,
		Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})
	if _, err := gw.EvictTo(150); err != nil {
		t.Fatalf("淘汰: %v", err)
	}

	if e, _ := st.CacheGet(a); e != nil {
		t.Fatal("k-a 该被淘汰")
	}
	if e, _ := st.CacheGet(other); e != nil {
		t.Fatal("k-other 该被淘汰（不然用量压不下去）")
	}
	e, _ := st.CacheGet(b)
	if e == nil {
		t.Fatal("k-b 该留着")
	}
	if _, err := cs.Open(e.SHA256); err != nil {
		t.Fatal("k-b 还在但文件被删了，共用内容没保护好")
	}
}
