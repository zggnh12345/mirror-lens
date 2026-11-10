package api

import (
	"encoding/json"
	"net/http"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// schedOrder 读/写「调度顺序」：自动（默认）或手动前 3 源。
func (s *Server) schedOrder(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.st.GetSchedOrder())
	case http.MethodPost:
		var in store.SchedOrder
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
			return
		}
		if in.Mode != "manual" {
			in.Mode = "auto"
		}
		list, err := s.st.ListSources()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		byID := make(map[int64]store.Source, len(list))
		for _, x := range list {
			byID[x.ID] = x
		}
		out := store.SchedOrder{Mode: in.Mode, Orders: map[string][]int64{}}
		for kind, ids := range in.Orders {
			if kind != "docker" && kind != "github" {
				continue
			}
			seen := map[int64]bool{}
			var keep []int64
			for _, id := range ids {
				if len(keep) >= 3 || seen[id] {
					continue
				}
				x, ok := byID[id]
				if !ok || !x.Enabled || !x.Supports(kind) {
					writeJSON(w, 400, map[string]string{"error": "所选源不可用或已被删除"})
					return
				}
				seen[id] = true
				keep = append(keep, id)
			}
			out.Orders[kind] = keep
		}
		if err := s.st.SetSchedOrder(out); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		s.sch.InvalidateLists()
		writeJSON(w, 200, out)
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}
