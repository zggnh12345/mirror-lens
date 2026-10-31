package gc

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func setup(t *testing.T) (*store.Store, *cache.Store, *GC) {
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
	return st, cs, New(st, cs)
}

func put(t *testing.T, st *store.Store, cs *cache.Store, key string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	tmp, err := cs.CreateTemp()
	if err != nil {
		t.Fatalf("临时文件: %v", err)
	}
	tmp.Write(body)
	tmp.Close()
	if _, err := cs.Commit(tmp.Name(), sha); err != nil {
		t.Fatalf("提交: %v", err)
	}
	if err := st.CachePut(store.CacheEntry{
		Key: key, SHA256: sha, Size: int64(len(body)),
		ContentType: "application/octet-stream", CreatedAt: time.Now().Unix(), TTL: 300,
	}); err != nil {
		t.Fatalf("写索引: %v", err)
	}
	return sha
}

// 两个键共用一份内容：删掉第一个键，文件必须留着；删到最后一个才删文件。
func TestDeleteKeysKeepsSharedBlob(t *testing.T) {
	st, cs, g := setup(t)
	body := []byte("same content")
	sha := put(t, st, cs, "raw:o/r/main/a", body)
	put(t, st, cs, "raw:o/r/main/b", body)

	if _, err := g.DeleteKeys([]string{"raw:o/r/main/a"}); err != nil {
		t.Fatalf("删 a: %v", err)
	}
	if !cs.Has(sha) {
		t.Fatal("还有 b 引用，文件不该删")
	}
	if _, err := cs.Open(sha); err != nil {
		t.Fatalf("文件读不出来: %v", err)
	}

	res, err := g.DeleteKeys([]string{"raw:o/r/main/b"})
	if err != nil {
		t.Fatalf("删 b: %v", err)
	}
	if res.Blobs != 1 || res.Bytes == 0 {
		t.Fatalf("最后一个引用删掉后应释放文件: %+v", res)
	}
	if cs.Has(sha) {
		t.Fatal("没人引用了，文件还在")
	}
}

// 删文件失败会进队列，下一轮重试；但重试前必须再看一次引用计数——
// 同一个内容可能刚被重新下载并建了新索引，那就不能再删。
func TestPendingBlobRetried(t *testing.T) {
	st, cs, g := setup(t)
	body := []byte("stuck")
	sha := put(t, st, cs, "raw:o/r/main/a", body)

	// 场景一：索引还在，文件被排进队列（例如上一轮误判）。重试不能删。
	if err := st.PendingBlobAdd(sha); err != nil {
		t.Fatalf("入队: %v", err)
	}
	if n, _ := g.RetryPending(); n != 0 {
		t.Fatalf("还有引用，不该删，实际删了 %d 个", n)
	}
	if !cs.Has(sha) {
		t.Fatal("有引用时文件被删了")
	}
	if shas, _ := st.PendingBlobs(10); len(shas) != 0 {
		t.Fatalf("引用还在时应把队列项销掉，实际 %v", shas)
	}

	// 场景二：索引已删、文件残留。重试要把它清掉。
	if err := st.CacheDelete("raw:o/r/main/a"); err != nil {
		t.Fatalf("删索引: %v", err)
	}
	if err := st.PendingBlobAdd(sha); err != nil {
		t.Fatalf("入队: %v", err)
	}
	if n, freed := g.RetryPending(); n != 1 || freed == 0 {
		t.Fatalf("应补删 1 个文件，实际 %d（释放 %d）", n, freed)
	}
	if cs.Has(sha) {
		t.Fatal("残留文件没删掉")
	}
	if shas, _ := st.PendingBlobs(10); len(shas) != 0 {
		t.Fatalf("队列没清空: %v", shas)
	}
}
