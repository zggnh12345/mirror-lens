package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/zggnh12345/mirror-lens/internal/auth"
)

// 3.5.17 B17 / B20：跨站副本的两档模式与参选勾选。
//
// GET  /api/cross-registry  返回 {mode, configured, selected, starred}
// PUT  /api/cross-registry  {"mode":"sequential"|"race"|"off"} 或
//
//	{"selected":[source_id,...]}
//
// 两档语义（B.9 冻结）：默认「顺序回退」，竞速必须显式开启。切换只影响
// 跨站层，不改 A 的 Host 开关、Source 星标或 source_host_caps 探测结果。
func (s *Server) crossRegistry(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		starred, err := s.st.StarredSourceIDs()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		ids := make([]int64, 0, len(starred))
		for id := range starred {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		configured := s.st.CrossRegistryMode()
		mode := configured
		if mode == "" {
			// 未设置时按 B17 默认「顺序回退」展示；部署期配置由网关决定实际生效值。
			mode = "sequential"
			if s.gw != nil {
				mode = s.gw.EffectiveReplicaMode()
			}
		}
		writeJSON(w, 200, map[string]any{
			"mode":       mode,
			"configured": configured,
			"selected":   s.st.CrossSelectedSources(),
			"starred":    ids,
		})
	case http.MethodPut:
		var in struct {
			Mode     *string  `json:"mode"`
			Selected *[]int64 `json:"selected"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
			return
		}
		if in.Mode == nil && in.Selected == nil {
			writeJSON(w, 400, map[string]string{"error": "需要 mode 或 selected"})
			return
		}
		actor := s.st.GetSetting(auth.KeyUser, auth.DefaultUser)
		ip := auth.ClientIP(r)
		if in.Mode != nil {
			if err := s.st.SetCrossRegistryMode(*in.Mode, actor, ip); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			if s.gw != nil {
				// 模式切换必须即时生效，不等快照 TTL。
				s.gw.InvalidateReplicaMode()
			}
		}
		if in.Selected != nil {
			if err := s.st.SetCrossSelectedSources(*in.Selected, actor, ip); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, 200, map[string]any{
			"ok":       true,
			"mode":     s.st.CrossRegistryMode(),
			"selected": s.st.CrossSelectedSources(),
		})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}
