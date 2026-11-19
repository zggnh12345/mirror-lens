package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 3.5.17 跨 Registry 副本映射管理 API。
//
// GET    /api/registry-replicas[?authority=]                     查询（含代次）
// GET    /api/registry-replica-audit[?authority=&limit=]         追加式审计
// POST   /api/registry-replicas                                  新建
// PUT    /api/registry-replicas                                  修改
// DELETE /api/registry-replicas?authority=&repository=&replica=  删除
//
// 映射写入 + Generation 递增 + 审计快照在 store 层同一事务完成。
// 这是「仓库→仓库」映射，不代替 trusted_source_mappings（Host→Source）。

type replicaReq struct {
	AuthorityRegistry   string `json:"authority_registry"`
	AuthorityRepository string `json:"authority_repository"`
	ReplicaRegistry     string `json:"replica_registry"`
	ReplicaRepository   string `json:"replica_repository"`
	Priority            *int   `json:"priority"`
	ValidFrom           int64  `json:"valid_from"`
	ValidUntil          int64  `json:"valid_until"`
	Enabled             *bool  `json:"enabled"`
}

func (r replicaReq) toStore() store.RegistryReplica {
	out := store.RegistryReplica{
		AuthorityRegistry:   r.AuthorityRegistry,
		AuthorityRepository: r.AuthorityRepository,
		ReplicaRegistry:     r.ReplicaRegistry,
		ReplicaRepository:   r.ReplicaRepository,
		Priority:            100,
		ValidFrom:           r.ValidFrom,
		ValidUntil:          r.ValidUntil,
		Enabled:             true,
	}
	if r.Priority != nil {
		out.Priority = *r.Priority
	}
	if r.Enabled != nil {
		out.Enabled = *r.Enabled
	}
	return out
}

func (s *Server) registryReplicas(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		authority := strings.TrimSpace(r.URL.Query().Get("authority"))
		items, err := s.st.ListRegistryReplicas(authority)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		gen := uint64(0)
		if authority != "" {
			g, err := s.st.ReplicaGeneration(authority)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			gen = g
		}
		writeJSON(w, 200, map[string]any{"items": items, "generation": gen})
	case http.MethodPost, http.MethodPut:
		var req replicaReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
		gen, err := s.st.PutRegistryReplica(req.toStore(), s.actor(r), r.Method == http.MethodPost)
		switch {
		case errors.Is(err, store.ErrReplicaExists):
			writeJSON(w, 409, map[string]string{"error": "副本映射已存在"})
			return
		case err != nil:
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "generation": gen})
	case http.MethodDelete:
		q := r.URL.Query()
		authority := strings.TrimSpace(q.Get("authority"))
		repository := strings.TrimSpace(q.Get("repository"))
		replica := strings.TrimSpace(q.Get("replica"))
		if authority == "" || repository == "" || replica == "" {
			writeJSON(w, 400, map[string]string{"error": "需要 authority、repository 与 replica"})
			return
		}
		gen, err := s.st.DeleteRegistryReplica(authority, repository, replica, s.actor(r))
		switch {
		case errors.Is(err, store.ErrReplicaMissing):
			writeJSON(w, 404, map[string]string{"error": "副本映射不存在"})
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

func (s *Server) registryReplicaAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.st.ListRegistryReplicaAudit(strings.TrimSpace(r.URL.Query().Get("authority")), limit)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
