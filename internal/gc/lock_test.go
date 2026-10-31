package gc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 提交方（网关）持锁期间，回收必须让路：这就是「查引用 + 删文件」的原子性来源。
// 观察期只能挡住「新文件」，挡不住「复用已有 blob」——那种情况文件很旧、引用还是 0。
func TestOrphansWaitsForBlobLock(t *testing.T) {
	st, cs, g := setup(t)

	body := []byte("reused blob")
	sha := put(t, st, cs, "raw:o/r/main/a", body)

	// 制造「文件很旧、但马上会被新索引引用」的状态：删索引、把 mtime 推老。
	if _, err := st.CacheDeleteMany([]string{"raw:o/r/main/a"}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cs.BlobPath(sha), old, old); err != nil {
		t.Fatal(err)
	}

	// 模拟网关正在提交：拿锁 → 写索引 → 放锁。
	unlock := cs.LockBlob(sha)
	done := make(chan Result, 1)
	go func() {
		res, err := g.Orphans()
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()

	select {
	case res := <-done:
		t.Fatalf("持锁期间不该完成回收（orphans=%d）", res.Blobs)
	case <-time.After(150 * time.Millisecond):
	}

	if err := st.CachePut(store.CacheEntry{
		Key: "raw:o/r/main/b", SHA256: sha, Size: int64(len(body)),
		ContentType: "text/plain", CreatedAt: time.Now().Unix(), TTL: 300,
	}); err != nil {
		t.Fatal(err)
	}
	unlock()

	select {
	case res := <-done:
		if res.Blobs != 0 {
			t.Fatalf("已有新索引引用，不该删，实际删了 %d 个", res.Blobs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("放锁后回收没继续")
	}
	if !cs.Has(sha) {
		t.Fatal("被新索引引用的 blob 被删掉了")
	}
}

// 正在写的临时文件，哪怕 mtime 很旧也不能被清掉；注销后才允许回收。
func TestSweepTempSkipsActive(t *testing.T) {
	_, cs, _ := setup(t)

	f, err := cs.CreateTemp()
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	f.WriteString("downloading")
	f.Close()

	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}

	if n, err := cs.SweepTemp(time.Now().Add(-time.Hour).Unix()); err != nil || n != 0 {
		t.Fatalf("活动中的临时文件不该被删：n=%d err=%v", n, err)
	}
	if _, err := os.Stat(name); err != nil {
		t.Fatal("文件已经不在了")
	}

	cs.ForgetTemp(name)
	if n, err := cs.SweepTemp(time.Now().Add(-time.Hour).Unix()); err != nil || n != 1 {
		t.Fatalf("注销后应被回收：n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Clean(name)); !os.IsNotExist(err) {
		t.Fatal("文件应该已经删掉")
	}
}
