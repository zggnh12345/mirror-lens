package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// syncTrusted 把「勾了信任」的源推给网关，改完立刻生效。
func (s *Server) syncTrusted() error {
	list, err := s.st.ListTrustedRelays()
	if err != nil {
		return err
	}
	out := make([]gateway.TrustedRelay, 0, len(list))
	for _, t := range list {
		out = append(out, gateway.TrustedRelay{
			ID: t.ID, Name: t.Name, URL: t.URL,
			PinHost: t.PinHost, PinIPv4: t.PinIPv4, PinIPv6: t.PinIPv6,
			PinState: t.PinState, GraceEnd: time.Unix(t.PinGraceUntil, 0),
		})
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
	gs, err := s.st.ListGroups()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	pins, err := s.st.ListTrustedRelays()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	byID := make(map[int64]store.TrustedRelay, len(pins))
	for _, p := range pins {
		byID[p.ID] = p
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		pin := byID[x.ID]
		out = append(out, map[string]any{
			"id":               x.ID,
			"name":             x.Name,
			"base_url":         x.BaseURL,
			"status":           x.Status,
			"enabled":          x.Enabled,
			"trusted":          x.Trusted,
			"group_id":         x.GroupID,
			"pin_host":         pin.PinHost,
			"pin_ipv4":         pin.PinIPv4,
			"pin_ipv6":         pin.PinIPv6,
			"pin_state":        pin.PinState,
			"pin_at":           pin.PinAt,
			"pin_confirmed_at": pin.PinConfirmedAt,
			"pin_grace_until":  pin.PinGraceUntil,
		})
	}
	writeJSON(w, 200, map[string]any{"sources": out, "groups": gs})
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
		Trusted *bool   `json:"trusted"`
		PinIPv4 *string `json:"pin_ipv4"`
		PinIPv6 *string `json:"pin_ipv6"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Trusted == nil && in.PinIPv4 == nil && in.PinIPv6 == nil) {
		writeJSON(w, 400, map[string]string{"error": "需要 trusted 或 pin_ipv4/pin_ipv6 字段"})
		return
	}

	// 手动钉 IP：管理员显式确认，直接进入 confirmed；非法地址整体拒绝，
	// 避免把半截地址写进拨号白名单。
	if in.PinIPv4 != nil || in.PinIPv6 != nil {
		v4, v6 := "", ""
		if in.PinIPv4 != nil {
			v4 = strings.TrimSpace(*in.PinIPv4)
		}
		if in.PinIPv6 != nil {
			v6 = strings.TrimSpace(*in.PinIPv6)
		}
		if v4 != "" && len(store.RelayPinIPs(v4)) != strings.Count(v4, ",")+1 {
			writeJSON(w, 400, map[string]string{"error": "pin_ipv4 含非法地址"})
			return
		}
		if v6 != "" && len(store.RelayPinIPs(v6)) != strings.Count(v6, ",")+1 {
			writeJSON(w, 400, map[string]string{"error": "pin_ipv6 含非法地址"})
			return
		}
		ok, err := s.st.UpdateRelayPin(id, v4, v6)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "源不存在或未勾选信任"})
			return
		}
		if err := s.syncTrusted(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "note": "已按手工地址钉住（confirmed），立即生效"})
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
