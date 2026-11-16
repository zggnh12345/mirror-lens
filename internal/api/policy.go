package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// policies 提供 Host 只读汇总与某 Host 下 Source 级策略列表。
// 多源开关仍由 config.yaml 管理，这里只读展示，不提供修改。
func (s *Server) policies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		hosts, err := s.st.ListPolicyHosts()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		var allowed []string
		if cfg := s.cfg.Load(); cfg != nil {
			allowed = cfg.Registry.MultiOriginHosts
		}
		writeJSON(w, 200, map[string]any{
			"hosts":              hosts,
			"multi_origin_hosts": allowed,
		})
		return
	}
	items, err := s.st.ListRealmPolicies(host)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"host": host, "sources": items})
}

// policyItem 读取、替换或删除一条 Source 级 Realm 策略。
// 删除即停用语义：运行时必须 fail closed，不虚构 enabled 字段。
func (s *Server) policyItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/policies/"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "id 不对"})
		return
	}
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		writeJSON(w, 400, map[string]string{"error": "缺少 host"})
		return
	}
	if r.Method != http.MethodGet {
		if _, serr := s.st.GetSource(id); serr != nil {
			writeJSON(w, 404, map[string]string{"error": "Source 不存在"})
			return
		}
		if _, herr := s.st.GetSourceHost(id, host); herr != nil {
			writeJSON(w, 400, map[string]string{"error": "host 不属于该 Source"})
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		p, err := s.st.GetRealmPolicy(id, host)
		if errors.Is(err, v2auth.ErrPolicyMissing) {
			writeJSON(w, 404, map[string]string{"error": "策略不存在"})
			return
		}
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, p)
	case http.MethodPut, http.MethodPatch:
		var in struct {
			PrimaryRealm string   `json:"primary_realm"`
			ExtraRealms  []string `json:"extra_realms"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
			return
		}
		old, _ := s.st.GetRealmPolicy(id, host)
		if err := s.st.SetRealmPolicy(id, host, in.PrimaryRealm, in.ExtraRealms); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		s.eng.Changed()
		s.audit(r, "policy_update", strconv.FormatInt(id, 10), policyAuditDetail(host, old, in.PrimaryRealm, in.ExtraRealms), true)
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		old, _ := s.st.GetRealmPolicy(id, host)
		if err := s.st.DeleteRealmPolicy(id, host); err != nil {
			if errors.Is(err, v2auth.ErrPolicyMissing) {
				writeJSON(w, 404, map[string]string{"error": "策略不存在"})
				return
			}
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		s.eng.Changed()
		s.audit(r, "policy_delete", strconv.FormatInt(id, 10), policyAuditDetail(host, old, "", nil), true)
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

// policyAudit 返回策略变更事件。source_id=0 表示全部。
func (s *Server) policyAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var sid int64
	if v := strings.TrimSpace(r.URL.Query().Get("source_id")); v != "" {
		sid, _ = strconv.ParseInt(v, 10, 64)
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.st.ListPolicyEvents(sid, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}

// policyAuditDetail 只写 Realm 与 Host，不写凭据。
func policyAuditDetail(host string, old *store.RealmPolicy, primary string, extra []string) string {
	oldDesc := "无"
	if old != nil {
		oldDesc = old.PrimaryRealm
		if len(old.ExtraRealms) > 0 {
			oldDesc += " + " + strings.Join(old.ExtraRealms, ",")
		}
	}
	newDesc := primary
	if len(extra) > 0 {
		newDesc += " + " + strings.Join(extra, ",")
	}
	if newDesc == "" {
		newDesc = "删除"
	}
	return "host=" + host + " " + oldDesc + " -> " + newDesc
}
