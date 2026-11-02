package gateway

import "log"

// lowDisk 判断是否该暂停落盘。配额（max_gb）管「缓存最多占多少」，
// 这条管「别把同一块盘上的别人挤死」：两者独立的理由很直接——
// 配额 30G、盘剩 5G 时，按配额看只用了一半，按盘看已经快没了。
// statfs 是纯内存操作，每个请求量一次也不算负担。
func (g *Gateway) lowDisk() bool {
	if g.cacheMinFree <= 0 || g.cache == nil {
		return false
	}
	free, err := g.cache.FSFree()
	if err != nil {
		// 量不到就别拦：挂载异常时全站变直通，比误删更糟。
		return false
	}
	low := free < g.cacheMinFree
	// 只在跨阈值时打一行，否则每个请求刷一条日志。
	if low != g.diskLowFlag.Load() && g.diskLowFlag.CompareAndSwap(!low, low) {
		if low {
			log.Printf("缓存盘剩余 %d MB 低于底线 %d MB，暂停落盘、改为直通", free>>20, g.cacheMinFree>>20)
		} else {
			log.Printf("缓存盘剩余 %d MB，恢复落盘", free>>20)
		}
	}
	return low
}

// DiskLow 供面板/探针查询当前是否因磁盘不足处于直通状态。
func (g *Gateway) DiskLow() bool {
	if g.cacheMinFree <= 0 || g.cache == nil {
		return false
	}
	free, err := g.cache.FSFree()
	return err == nil && free < g.cacheMinFree
}
