package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 3.5.17 F08 可信映射管理 API。
//
// GET    /api/trusted-mappings[?host=]           查询（含当前 Generation）
// GET    /api/trusted-mapping-audit[?host=&limit=] 追加式审计
// POST   /api/trusted-mappings                   新建
// PUT    /api/trusted-mappings                   修改
// DELETE /api/trusted-mappings?host=&source_id=  删除
//
// 事务语义在 store 层：映射写入 + Generation 递增 + 审计快照同事务，
// 这里只做参数校验与错误码映射，不再补一次异步审计。

type mappingReq struct {
	RegistryHost string `json:"registry_host"`
	SourceID     int64  `json:"source_id"`
	Priority     *int   `json:"priority"`
	ValidFrom    int64  `json:"valid_from"`
	ValidUntil   int64  `json:"valid_until"`
	Enabled      *bool  `json:"enabled"`
}

// actor 返回审计用的当前账号名。同事务审计只记操作者，不记 IP：
// IP 在管理 audit 表里已有；trusted_mapping_audit 的列是冻结的。
func (s *Server) actor(r *http.Request) string {
	u := s.st.GetSetting(auth.KeyUser, auth.DefaultUser)
	if u == "" {
		return "admin"
	}
	return u
}

func (m mappingReq) toStore() store.TrustedMapping {
	out := store.TrustedMapping{
		RegistryHost: m.RegistryHost,
		SourceID:     m.SourceID,
		Priority:     100,
		ValidFrom:    m.ValidFrom,
		ValidUntil:   m.ValidUntil,
		Enabled:      true,
	}
	if m.Priority != nil {
		out.Priority = *m.Priority
	}
	if m.Enabled != nil {
		out.Enabled = *m.Enabled
	}
	return out
}

// trustedMappings 处理集合上的 GET/POST/PUT/DELETE。
func (s *Server) trustedMappings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		host := strings.TrimSpace(r.URL.Query().Get("host"))
		items, err := s.st.ListTrustedMappings(host)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		gens, err := s.st.MappingGenerations()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"items": items, "generations": gens})
	case http.MethodPost, http.MethodPut:
		var req mappingReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
		gen, err := s.st.PutTrustedMapping(req.toStore(), s.actor(r), r.Method == http.MethodPost)
		switch {
		case errors.Is(err, store.ErrMappingExists):
			writeJSON(w, 409, map[string]string{"error": "映射已存在"})
			return
		case err != nil:
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "generation": gen})
	case http.MethodDelete:
		host := strings.TrimSpace(r.URL.Query().Get("host"))
		sid, err := strconv.ParseInt(r.URL.Query().Get("source_id"), 10, 64)
		if host == "" || err != nil || sid <= 0 {
			writeJSON(w, 400, map[string]string{"error": "需要 host 与 source_id"})
			return
		}
		gen, err := s.st.DeleteTrustedMapping(host, sid, s.actor(r))
		switch {
		case errors.Is(err, store.ErrMappingMissing):
			writeJSON(w, 404, map[string]string{"error": "映射不存在"})
			return
		case err != nil:
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "generation": gen})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

// trustedMappingAudit 读取追加式审计。
func (s *Server) trustedMappingAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.st.ListTrustedMappingAudit(strings.TrimSpace(r.URL.Query().Get("host")), limit)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
