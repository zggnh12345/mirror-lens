// Package gc 是唯一允许删缓存的地方。
//
// 容量淘汰、过期清理、手动清空、索引对账都走这里，原因很简单：删一个 blob
// 要先确认没有别的键还在引用它。如果每个模块自己写一套删除逻辑，迟早会出现
// 一个模块认为能删、另一个模块还在读的情况——文件被删了，索引还在，
// 用户看到的是"缓存命中却 502"。
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
	Pending int   `json:"pending"` // 删失败、排队等重试的文件
}

func New(st *store.Store, cs *cache.Store) *GC { return &GC{st: st, cs: cs} }

// DeleteKeys 删一批索引，再删不再被引用的物理文件。
//
// 顺序不能反：先删文件再删索引，中间崩了就是"索引在、文件没了"；
// 先删索引再删文件，最坏是"文件在、没人引用"，下一轮对账会收走。
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

// dropBlob 引用计数归零才删文件。删不掉（权限、占用）就排队，Janitor 下轮再试，
// 免得出现"数据库删了、文件永远躺在盘上"。
func (g *GC) dropBlob(sha string) (n int, freed int64, err error) {
	// 计数与删除必须原子：否则刚被新索引引用的文件会被删掉。
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
		return 0, 0, nil // 文件本来就不在，没东西可释放
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

// RetryPending 重试上轮删不掉的文件。重试前再看一次引用计数：
// 同一个内容可能刚被重新下载并建了新索引。
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

// Orphans 删掉"磁盘上有、没有任何索引引用"的文件。
// 崩溃在 Commit 与 CachePut 之间就会留下这种文件。
// orphanGrace 是孤儿文件的观察期。
//
// 提交路径是「先 rename 落盘、再写索引」，两步之间有窗口；而 refs 快照比扫盘更早，
// 所以整个扫描期间新提交的 blob 都可能被判成孤儿。刚出现的文件先放过，
// 下一轮（默认 10 分钟后）它已经有索引了，真孤儿也只是晚一轮回收。
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
		// 观察期挡不住「复用已有 blob」：新请求 Commit 时发现文件已在，
		// 直接删临时文件，随后才写索引。这中间引用数确实是 0，而 mtime 很旧。
		// 所以查询和删除必须在同一把 blob 锁里完成，提交方在锁内 rename+写索引。
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

// MissingFiles 返回"索引在、文件不在"的键，由调用方决定删索引。
// 这里不直接删：调用方通常要顺带记日志和统计。
func (g *GC) MissingFiles(entries []store.CacheEntry) []string {
	var out []string
	for _, e := range entries {
		if e.SHA256 == "" || !g.cs.Has(e.SHA256) {
			out = append(out, e.Key)
		}
	}
	return out
}
