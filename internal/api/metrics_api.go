package api

import (
	"net/http"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// metricsView 给面板用的聚合快照（JSON）。和 /metrics 文本格式不同：
// 这个是鉴权后的内部接口，给「运行概览」卡片和图表用。
func (s *Server) metricsView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, metrics.SnapshotView())
}
