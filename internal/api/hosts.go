package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// sourceHosts 处理 /api/sources/{id}/hosts 及其子路径。
// 路由由 sourceItem 在解析 id 之前分流，避免把 hosts 当成 id。
func (s *Server) sourceHosts(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] != "hosts" {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "id 不对"})
		return
	}
	if _, err := s.st.GetSource(id); err != nil {
		writeJSON(w, 404, map[string]string{"error": "Source 不存在"})
		return
	}
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			hosts, err := s.st.GetSourceHosts(id)
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]any{"source_id": id, "hosts": hosts})
		case http.MethodPost:
			var in struct {
				Host         string          `json:"host"`
				PrimaryRealm string          `json:"primary_realm"`
				ExtraRealms  json.RawMessage `json:"extra_realms"`
			}
			body := http.MaxBytesReader(w, r.Body, 8192)
			raw, err := io.ReadAll(body)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
				return
			}
			if unknown := firstUnknownField(raw, []string{"host", "primary_realm", "extra_realms"}, nil); unknown != "" {
				writeJSON(w, 400, map[string]string{"error": "unknown_field", "field": unknown})
				return
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
				return
			}
			if strings.TrimSpace(in.Host) == "" {
				writeJSON(w, 400, map[string]string{"error": "缺少 host", "field": "host"})
				return
			}
			if strings.TrimSpace(in.PrimaryRealm) == "" {
				writeJSON(w, 400, map[string]string{"error": "primary_realm is required", "field": "primary_realm"})
				return
			}
			var extra []string
			if rawEqualNull(in.ExtraRealms) {
				writeJSON(w, 400, map[string]string{"error": "extra_realms 不接受 null", "field": "extra_realms"})
				return
			}
			if len(in.ExtraRealms) > 0 {
				if in.ExtraRealms[0] != '[' {
					writeJSON(w, 400, map[string]string{"error": "extra_realms 必须是数组", "field": "extra_realms"})
					return
				}
				if err := json.Unmarshal(in.ExtraRealms, &extra); err != nil {
					writeJSON(w, 400, map[string]string{"error": "extra_realms 元素必须是字符串", "field": "extra_realms"})
					return
				}
				if len(extra) > 3 {
					writeJSON(w, 400, map[string]string{"error": "extra_realms 最多 3 个", "field": "extra_realms"})
					return
				}
			}
			if err := s.st.AddSourceHost(id, in.Host, in.PrimaryRealm, extra); err != nil {
				status := 400
				if errors.Is(err, store.ErrSourceNotFound) {
					status = 404
				}
				writeJSON(w, status, map[string]string{"error": err.Error()})
				return
			}
			s.eng.Changed()
			s.audit(r, "source_host_add", strconv.FormatInt(id, 10), in.Host, true)
			writeJSON(w, 200, map[string]any{"ok": true})
		default:
			writeJSON(w, 405, map[string]string{"error": "method"})
		}
		return
	}

	host, err := url.PathUnescape(parts[2])
	if err != nil || strings.TrimSpace(host) == "" {
		writeJSON(w, 400, map[string]string{"error": "host 不对"})
		return
	}
	if len(parts) == 3 {
		if r.Method != http.MethodDelete {
			writeJSON(w, 405, map[string]string{"error": "method"})
			return
		}
		if err := s.st.RemoveSourceHost(id, host); err != nil {
			status := 400
			if errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrHostCapsNotFound) {
				status = 404
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		s.eng.Changed()
		s.audit(r, "source_host_remove", strconv.FormatInt(id, 10), host, true)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if len(parts) != 4 {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	switch parts[3] {
	case "enabled":
		if r.Method != http.MethodPut {
			writeJSON(w, 405, map[string]string{"error": "method"})
			return
		}
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
			return
		}
		if err := s.st.SetSourceHostEnabled(id, host, in.Enabled); err != nil {
			status := 400
			if errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrHostCapsNotFound) {
				status = 404
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		s.eng.Changed()
		action := "source_host_disable"
		if in.Enabled {
			action = "source_host_enable"
		}
		s.audit(r, action, strconv.FormatInt(id, 10), host, true)
		writeJSON(w, 200, map[string]any{"ok": true})
	case "reset":
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "method"})
			return
		}
		if err := s.st.ResetSourceHostVerdict(id, host); err != nil {
			status := 400
			if errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrHostCapsNotFound) {
				status = 404
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		s.eng.Changed()
		s.audit(r, "source_host_reset", strconv.FormatInt(id, 10), host, true)
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}
