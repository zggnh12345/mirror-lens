// Package gc 是唯一允许删缓存的地方。
//
// 淘汰、过期、清空、对账都走这里：删 blob 前必须确认没有别的键引用它，
// 各模块自写删除逻辑会出「文件删了索引还在」，用户看到缓存命中却 502。
package gc

import (
	"log"
	"os"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

type GC struct {
	st *store.Store
	cs *cache.Store
}

type Result struct {
	Entries int   `json:"entries"` // 删掉的索引条目
	Blobs   int   `json:"blobs"`   // 真正删掉的物理文件
	Bytes   int64 `json:"bytes"`   // 释放的磁盘空间
	Pending int   `json:"pending"` // 删失败待重试的文件
}

func New(st *store.Store, cs *cache.Store) *GC { return &GC{st: st, cs: cs} }

// DeleteKeys 先删索引，再删不再被引用的文件。
// 顺序不能反：崩在中间时最坏只剩「文件在、没人引用」，对账会收走。
func (g *GC) DeleteKeys(keys []string) (Result, error) {
	var res Result
	entries, err := g.st.CacheDeleteMany(keys)
	if err != nil {
		return res, err
	}
	res.Entries = len(entries)
	seen := map[string]bool{}
	for _, e := range entries {
		if e.SHA256 == "" || seen[e.SHA256] {
			continue
		}
		seen[e.SHA256] = true
		n, freed, err := g.dropBlob(e.SHA256)
		res.Blobs += n
		res.Bytes += freed
		if err != nil {
			res.Pending++
		}
	}
	return res, nil
}

// dropBlob 引用归零才删文件；删不掉就排队下轮再试，
// 免得数据库删了而文件永远躺在盘上。
func (g *GC) dropBlob(sha string) (n int, freed int64, err error) {
	// 计数与删除必须原子，否则会删掉刚被新引用的文件。
	unlock := g.cs.LockBlob(sha)
	defer unlock()

	refs, err := g.st.CacheCountSHA(sha)
	if err != nil {
		return 0, 0, err
	}
	if refs > 0 {
		return 0, 0, nil // 还有别的键在用
	}
	if !g.cs.Has(sha) {
		return 0, 0, nil // 文件本来就不在
	}
	if fi, e := os.Stat(g.cs.BlobPath(sha)); e == nil {
		freed = fi.Size()
	}
	if err := g.cs.Remove(sha); err != nil {
		log.Printf("删缓存文件失败 %s: %v（已排队重试）", sha, err)
		_ = g.st.PendingBlobAdd(sha)
		return 0, 0, err
	}
	return 1, freed, nil
}

// RetryPending 重试上轮删不掉的文件；删前再看引用计数，
// 同一内容可能刚被重新下载并建了新索引。
func (g *GC) RetryPending() (n int, freed int64) {
	shas, err := g.st.PendingBlobs(0)
	if err != nil {
		return 0, 0
	}
	for _, sha := range shas {
		bn, bf := g.retryOne(sha)
		n += bn
		freed += bf
	}
	return n, freed
}

func (g *GC) retryOne(sha string) (int, int64) {
	unlock := g.cs.LockBlob(sha)
	defer unlock()

	refs, err := g.st.CacheCountSHA(sha)
	if err != nil {
		return 0, 0
	}
	if refs > 0 {
		_ = g.st.PendingBlobDone(sha)
		return 0, 0
	}
	var freed int64
	if fi, e := os.Stat(g.cs.BlobPath(sha)); e == nil {
		freed = fi.Size()
	}
	if err := g.cs.Remove(sha); err != nil {
		return 0, 0 // 下轮再说
	}
	_ = g.st.PendingBlobDone(sha)
	return 1, freed
}

// Orphans 删掉磁盘上有、没有任何索引引用的文件。
// 提交是「先 rename 落盘、再写索引」，崩在中间就留下这种文件；
// 扫描期间新提交的 blob 也可能被判成孤儿，靠观察期放过。
const orphanGrace = 5 * time.Minute

func (g *GC) Orphans() (Result, error) {
	var res Result
	refs, err := g.st.CacheRefs()
	if err != nil {
		return res, err
	}
	blobs, err := g.cs.ListBlobs()
	if err != nil {
		return res, err
	}
	now := time.Now()
	for _, sha := range blobs {
		if refs[sha] > 0 {
			continue
		}
		fi, e := os.Stat(g.cs.BlobPath(sha))
		if e != nil {
			continue // 已经不在了，下一轮再说
		}
		if now.Sub(fi.ModTime()) < orphanGrace {
			continue
		}
		// 观察期挡不住「复用已有 blob」：文件很旧而引用数暂时为 0，
		// 所以查引用和删除要在同一把 blob 锁内完成。
		unlock := g.cs.LockBlob(sha)
		n, err := g.st.CacheRef(sha)
		if err != nil || n > 0 {
			unlock()
			continue
		}
		res.Bytes += fi.Size()
		if err := g.cs.Remove(sha); err != nil {
			unlock()
			_ = g.st.PendingBlobAdd(sha)
			res.Pending++
			continue
		}
		unlock()
		res.Blobs++
	}
	return res, nil
}

// MissingFiles 返回「索引在、文件不在」的键；删索引由调用方做，
// 因为要顺带记日志和统计。
func (g *GC) MissingFiles(entries []store.CacheEntry) []string {
	var out []string
	for _, e := range entries {
		if e.SHA256 == "" || !g.cs.Has(e.SHA256) {
			out = append(out, e.Key)
		}
	}
	return out
}
