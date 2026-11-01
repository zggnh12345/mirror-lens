package gateway

import "log"

// enforceQuota 异步跑容量检查：淘汰是后台活儿，不放请求路径上。
func (g *Gateway) enforceQuota() {
	if g.cacheMaxBytes <= 0 || g.st == nil || g.cache == nil {
		return
	}
	go func() {
		if _, err := g.EvictTo(g.cacheMaxBytes); err != nil {
			log.Printf("缓存淘汰失败: %v", err)
		}
	}()
}

// EvictTo 按最久未访问淘汰到 limit 以内。用量以 blobs 实际大小为准，
// 索引的 size 可能过期；tmp/、partial/ 归 Janitor，不在淘汰范围。
func (g *Gateway) EvictTo(limit int64) (int64, error) {
	g.quotaMu.Lock()
	defer g.quotaMu.Unlock()

	if limit <= 0 {
		return 0, nil
	}
	used, err := g.cache.TotalSize()
	if err != nil {
		return 0, err
	}
	var freed int64
	for used > limit {
		olds, err := g.st.CacheOldest(64)
		if err != nil {
			return freed, err
		}
		if len(olds) == 0 {
			break
		}
		// 一次只删一条再重新量：批删会一口气删到远低于上限
		// （3 个 100 字节条目、上限 250，批删清掉 300）。
		// 走 gc.DeleteKeys：删索引+引用计数删文件，与手动清理同一条路。
		res, err := g.gc.DeleteKeys([]string{olds[0].Key})
		if err != nil {
			return freed, err
		}
		freed += res.Bytes
		if res.Entries == 0 {
			break // 删不动了，别死循环
		}
		if used, err = g.cache.TotalSize(); err != nil {
			break
		}
	}
	if freed > 0 {
		log.Printf("缓存超过上限 %d MB，淘汰了 %d MB", limit>>20, freed>>20)
	}
	return freed, nil
}
