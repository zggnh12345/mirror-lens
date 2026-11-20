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
		// 3.5.18 18a：B 三字段统一走 ResolveBConfig 单一入口。
		deployMode := ""
		if s.gw != nil {
			deployMode = s.gw.DeployReplicaMode()
		}
		cfg := s.st.ResolveBConfig(deployMode)
		writeJSON(w, 200, map[string]any{
			"enabled":    cfg.Enabled,
			"mode":       cfg.Mode,
			"sort_mode":  cfg.SortMode,
			"configured": s.st.CrossRegistryMode(), // 兼容 3.5.17：原样 settings 值
			"selected":   s.st.CrossSelectedSources(),
			"starred":    ids,
		})
	case http.MethodPut:
		var in struct {
			Enabled  *bool    `json:"enabled"`
			Mode     *string  `json:"mode"`
			SortMode *string  `json:"sort_mode"`
			Selected *[]int64 `json:"selected"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
			return
		}
		if in.Enabled == nil && in.Mode == nil && in.SortMode == nil && in.Selected == nil {
			writeJSON(w, 400, map[string]string{"error": "需要 enabled / mode / sort_mode / selected"})
			return
		}
		actor := s.st.GetSetting(auth.KeyUser, auth.DefaultUser)
		ip := auth.ClientIP(r)
		if in.Enabled != nil {
			if err := s.st.SetBEnabled(*in.Enabled, actor, ip); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		if in.Mode != nil {
			// 3.5.18：新模型 mode 只接受 sequential/race；off 由 enabled=false 表达。
			if err := s.st.SetBMode(*in.Mode, actor, ip); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		if in.SortMode != nil {
			if err := s.st.SetBSortMode(*in.SortMode, actor, ip); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		if in.Selected != nil {
			if err := s.st.SetCrossSelectedSources(*in.Selected, actor, ip); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		if s.gw != nil {
			s.gw.InvalidateReplicaMode() // 切换即时生效
		}
		deployMode := ""
		if s.gw != nil {
			deployMode = s.gw.DeployReplicaMode()
		}
		cfg := s.st.ResolveBConfig(deployMode)
		writeJSON(w, 200, map[string]any{
			"ok":        true,
			"enabled":   cfg.Enabled,
			"mode":      cfg.Mode,
			"sort_mode": cfg.SortMode,
			"selected":  s.st.CrossSelectedSources(),
		})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}
