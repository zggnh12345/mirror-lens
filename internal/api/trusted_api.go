package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// syncTrusted 把库里的白名单推给网关，改完立刻生效。
func (s *Server) syncTrusted() error {
	list, err := s.st.ListTrusted()
	if err != nil {
		return err
	}
	out := make([]gateway.TrustedRelay, 0, len(list))
	for _, t := range list {
		if !t.Enabled {
			continue
		}
		out = append(out, gateway.TrustedRelay{ID: t.ID, Name: t.Name, URL: t.URL})
	}
	s.gw.SetTrusted(out)
	return nil
}

// trusted 管白名单集合：GET 列出全部，POST 新增。
func (s *Server) trusted(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.st.ListTrusted()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if list == nil {
			list = []store.TrustedRelay{}
		}
		writeJSON(w, 200, map[string]any{"trusted": list})
	case http.MethodPost:
		var in struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
		in.URL = strings.TrimSpace(in.URL)
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeJSON(w, 400, map[string]string{"error": "地址必须是 http(s)://host/... 形式"})
			return
		}
		t, err := s.st.AddTrusted(in.Name, in.URL)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if err := s.syncTrusted(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, t)
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

// trustedItem 管单条：DELETE 删除，PATCH 启停。
func (s *Server) trustedItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/trusted/"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "id 不合法"})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		ok, err := s.st.DeleteTrusted(id)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "不存在"})
			return
		}
		s.syncTrusted()
		writeJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodPatch:
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Enabled == nil {
			writeJSON(w, 400, map[string]string{"error": "需要 enabled 字段"})
			return
		}
		ok, err := s.st.SetTrustedEnabled(id, *in.Enabled)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "不存在"})
			return
		}
		s.syncTrusted()
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}
