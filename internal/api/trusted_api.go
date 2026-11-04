package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/gateway"
)

// syncTrusted 把「勾了信任」的源推给网关，改完立刻生效。
func (s *Server) syncTrusted() error {
	list, err := s.st.ListTrustedRelays()
	if err != nil {
		return err
	}
	out := make([]gateway.TrustedRelay, 0, len(list))
	for _, t := range list {
		out = append(out, gateway.TrustedRelay{ID: t.ID, Name: t.Name, URL: t.URL})
	}
	s.gw.SetTrusted(out)
	return nil
}

// trusted 列出所有已有源及各自的信任标记——面板直接勾选，不用手输地址。
func (s *Server) trusted(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	list, err := s.st.ListSources()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, map[string]any{
			"id":       x.ID,
			"name":     x.Name,
			"base_url": x.BaseURL,
			"status":   x.Status,
			"enabled":  x.Enabled,
			"trusted":  x.Trusted,
		})
	}
	writeJSON(w, 200, map[string]any{"sources": out})
}

// trustedItem 只改一个源的信任标记：PATCH {"trusted": true|false}。
func (s *Server) trustedItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/trusted/"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "id 不合法"})
		return
	}
	if r.Method != http.MethodPatch {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var in struct {
		Trusted *bool `json:"trusted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Trusted == nil {
		writeJSON(w, 400, map[string]string{"error": "需要 trusted 字段"})
		return
	}
	ok, err := s.st.SetSourceTrusted(id, *in.Trusted)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "源不存在"})
		return
	}
	if err := s.syncTrusted(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if *in.Trusted {
		writeJSON(w, 200, map[string]any{"ok": true, "note": "已加入竞速白名单，立即生效"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "note": "已移出白名单"})
}
