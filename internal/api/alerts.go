package api

import (
	"net/http"
)

// alertsList 返回最近的 error 级日志，供面板铃铛使用。
func (s *Server) alertsList(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "unread": 0})
		return
	}
	items, unread := s.alerts.Snapshot(50)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "unread": unread})
}

// alertsSeen 点开铃铛后把现有条目标为已读，红点清零。
func (s *Server) alertsSeen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	if s.alerts != nil {
		s.alerts.MarkSeen()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// alertsClear 清空告警环。
func (s *Server) alertsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	if s.alerts != nil {
		s.alerts.Clear()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
