package gateway

import "log"

// enforceQuota 在写缓存之后异步跑一次容量检查。
// 不放在请求路径上：淘汰是后台活儿，别让用户等。
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

// EvictTo 按"最久没被访问"淘汰到 limit 以内，返回释放的字节数。
//
// 判断依据用 blobs 的实际大小，不用索引里的 size 之和：索引可能过期，
// 文件也可能被外部删过。tmp/ 和 partial/ 不算在内——那不是淘汰能清的，
// 归 Janitor 按时间清。
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
		// 一次只淘汰一条，删完重新量。整批删会一口气删到远低于上限——
		// 三个 100 字节的条目、上限 250，批删会把 300 全清掉。
		// 删索引 + 按引用计数删文件，和手动清理、对账走同一条路
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
