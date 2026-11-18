package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// raceHostView 是面板/API 的一行展示：三个概念分开写，不合并成「已授权竞速」。
//
//   - host_switch：Host 级人工总开关（star_race）
//   - starred：该 Host 上有多少 Source 被人为星标（人工竞速资格）
//   - capable：探测能力表里该 Host 上 Supported 且启用的 Source 数（技术能力）
//
// 竞速真正发生还要健康/恢复 TTL/调度预算都过关，所以 ready 只是「此刻有条件」。
type raceHostView struct {
	Host            string `json:"host"`
	HostSwitch      bool   `json:"host_switch"`
	StarredSources  int    `json:"starred_sources"`
	CapableSources  int    `json:"capable_sources"`
	EligibleSources int    `json:"eligible_sources"`
	Ready           bool   `json:"ready"`
	Note            string `json:"note,omitempty"`
}

// raceHosts 列出所有出现在能力表、或被人为开启过开关的 Host。
//
// 未配置过的 Host 不在这里出现：默认全关，列出来只会让人以为系统「支持竞速」。
func (s *Server) raceHosts() ([]raceHostView, error) {
	srcs, err := s.st.ListSources()
	if err != nil {
		return nil, err
	}
	switches := s.gw.StarRaceHosts()
	view := map[string]*raceHostView{}
	ensure := func(host string) *raceHostView {
		if v, ok := view[host]; ok {
			return v
		}
		v := &raceHostView{Host: host, HostSwitch: switches[host]}
		view[host] = v
		return v
	}
	for u := range switches {
		if u != "" {
			ensure(u)
		}
	}
	for i := range srcs {
		src := srcs[i]
		for _, h := range src.Hosts {
			v := ensure(h.Host)
			if h.Enabled && h.EffectiveStatus == "Supported" &&
				(h.RegistryPathMode == "native" || h.RegistryPathMode == "host_prefix") {
				v.CapableSources++
				if src.Preferred && src.Enabled && src.Status != "archived" {
					v.EligibleSources++
				}
			}
			if src.Preferred && src.Enabled && src.Status != "archived" {
				v.StarredSources++
			}
		}
	}
	out := make([]raceHostView, 0, len(view))
	for _, v := range view {
		v.Ready = v.HostSwitch && v.EligibleSources >= 2
		switch {
		case !v.HostSwitch:
			v.Note = "Host 开关关闭：不竞速"
		case v.EligibleSources < 2:
			v.Note = "合格星标源不足两个：顺序回源"
		default:
			v.Note = "条件满足：可形成双路竞速"
		}
		out = append(out, *v)
	}
	// 固定顺序，避免每次刷新面板跳动。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Host < out[j-1].Host; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// starRace 处理 /api/star-race 与 /api/star-race/{host}。
//
// GET   /api/star-race              三个概念分离的只读视图
// PUT   /api/star-race/{host}       body {"enabled":true|false}
//
// 写入是人工决策接口：探测流程永远不调用它，星标也不由探测置位。
func (s *Server) starRace(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/star-race")
	rest = strings.TrimPrefix(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		hosts, err := s.raceHosts()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"hosts": hosts})
	case rest != "" && r.Method == http.MethodPut:
		host, err := registryhost.Check(rest)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Enabled == nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对：需要 {\"enabled\":true|false}"})
			return
		}
		if err := s.st.SetStarRace(host, *in.Enabled); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		// 开关必须立刻生效：调度器候选与 Gateway 快照都不能等 TTL。
		s.gw.InvalidateStarRace()
		s.audit(r, "star_race", host, "enabled="+boolText(*in.Enabled), true)
		hosts, _ := s.raceHosts()
		writeJSON(w, 200, map[string]any{"ok": true, "host": host, "enabled": *in.Enabled, "hosts": hosts})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// starRaceAudit 在星标变化时留一条管理审计：星标是人工竞速资格，
// 变更必须可追溯（T49），但不写进 source_events 以免污染探测统计。
func (s *Server) starRaceAudit(r *http.Request, id int64, before, after bool) {
	if before == after {
		return
	}
	s.audit(r, "source_star", strconv.FormatInt(id, 10), "race_star="+boolText(after), true)
}
