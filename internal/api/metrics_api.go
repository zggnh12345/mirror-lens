package api

import (
	"net/http"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// metricsView 是面板用的聚合快照（JSON），与 /metrics 文本格式不同，
// 供「运行概览」卡片和图表使用。
func (s *Server) metricsView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	view := metrics.SnapshotView()
	// 缓存占用按磁盘实时算：janitor 10 分钟才刷一次 gauge，中间清完缓存
	// 面板数字会滞后，看起来「清了没变」。面板是低频请求，重扫一次开销可接受。
	if s.gw != nil {
		if cs := s.gw.CacheStore(); cs != nil {
			if used, err := cs.DiskUsage(); err == nil {
				var blobs, free uint64
				if bl, err := cs.ListBlobs(); err == nil {
					blobs = uint64(len(bl))
				}
				if f, err := cs.FSFree(); err == nil {
					free = uint64(f)
				}
				view.Cache = metrics.CacheView{HasCache: true, Bytes: uint64(used), Blobs: blobs, Free: free}
			}
		}
	}
	// 「源站并发上限」按 3.0 的固定配额算：单源 dispatch × 当前可用源数。
	// 动态上限的 gauge 只在已下线的 AIMD 路径上报，重启后永远是空的，
	// 面板画成「-」看起来像坏了。
	if s.sch != nil {
		hard, dispatch := s.sch.DispatchCap()
		if dispatch > 0 && s.st != nil {
			if list, err := s.st.ListSources(); err == nil {
				var total uint64
				for _, x := range list {
					if !x.Enabled || (x.Status != "usable" && x.Status != "degraded") {
						continue
					}
					total += uint64(dispatch)
				}
				view.Concurrency.Limit = total
				view.Concurrency.PerSource = dispatch
				view.Concurrency.Hard = hard
			}
		}
	}
	writeJSON(w, http.StatusOK, view)
}
