package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// tokens 管理 Access Token：明文只返回一次，库里只存哈希。
func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.st.ListTokens()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"tokens": list})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
		body.Name = strings.TrimSpace(body.Name)
		if body.Name == "" {
			body.Name = "未命名"
		}
		if len(body.Name) > 40 {
			writeJSON(w, 400, map[string]string{"error": "名称最多 40 个字"})
			return
		}
		plain, tok, err := s.st.CreateToken(body.Name)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{
			"token":  plain,
			"id":     tok.ID,
			"prefix": tok.Prefix,
			"note":   "这串只显示这一次，关掉就找不回来了",
		})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, 405, map[string]string{"error": "方法不对"})
	}
}

func (s *Server) tokenItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/tokens/"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "id 不对"})
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		writeJSON(w, 405, map[string]string{"error": "方法不对"})
		return
	}
	if err := s.st.RevokeToken(id); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// kindStat 是单协议今日流量（字节）：hit 是发给客户端的量，
// fill 是从上游拉的量。Range 命中 10 MB 可能对应填充 100 MB，不能混。
type kindStat struct {
	Hit    int64   `json:"hit"`
	Fill   int64   `json:"fill"`
	Bypass int64   `json:"bypass"`
	Rate   float64 `json:"hit_rate"`
}

func (s *Server) accelStats(w http.ResponseWriter, r *http.Request) {
	n, bytes, err := s.st.CacheTotals()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	traffic, err := s.st.TrafficByDay(7)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	recent, err := s.st.CacheList(20)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	today := time.Now().Format("2006-01-02")
	kinds := map[string]*kindStat{}
	var total kindStat
	for _, row := range traffic {
		if row.Day != today {
			continue
		}
		k := row.Kind
		if k == "" {
			k = "unknown" // 历史记录没有 kind，单独列
		}
		ks := kinds[k]
		if ks == nil {
			ks = &kindStat{}
			kinds[k] = ks
		}
		switch row.Traffic {
		case store.TrafficHit:
			ks.Hit += row.Bytes
			total.Hit += row.Bytes
		case store.TrafficFill:
			ks.Fill += row.Bytes
			total.Fill += row.Bytes
		case store.TrafficBypass:
			ks.Bypass += row.Bytes
			total.Bypass += row.Bytes
		}
	}
	for _, ks := range kinds {
		ks.Rate = hitRate(ks.Hit, ks.Fill)
	}
	total.Rate = hitRate(total.Hit, total.Fill)

	cache := map[string]any{"entries": n, "bytes": bytes}
	if s.gw != nil {
		cs := s.gw.CacheStore()
		if disk, err := cs.DiskUsage(); err == nil {
			cache["disk"] = disk
		}
		if free, err := cs.FSFree(); err == nil {
			cache["free"] = free // 文件系统剩余，与配额无关
		}
		if vc, err := s.st.CacheVerifyCounts(); err == nil {
			cache["verify"] = vc
		}
		cache["verify_failed"] = s.gw.VerifyFailed()
		cache["parallel"] = s.gw.ParallelStats()
	}
	if s.gw != nil && s.gw.CacheLimit() > 0 {
		cache["limit"] = s.gw.CacheLimit()
	}

	out := map[string]any{
		"cache":   cache,
		"traffic": traffic,
		"today":   map[string]any{"all": total, "kinds": kinds},
		"recent":  recent,
	}
	if s.jan != nil {
		out["janitor"] = s.jan.Last()
	}
	if s.sch != nil && s.sch.Enabled() {
		out["scheduler"] = s.sch.Snapshot([]string{"raw", "release", "git", "docker"})
	}
	writeJSON(w, 200, out)
}

// hitRate 分母只含 hit+fill，BYPASS 既非命中也不算回源，
// 混进去会把大文件透传误算成「缓存没用」。
func hitRate(hit, fill int64) float64 {
	if hit+fill == 0 {
		return 0
	}
	return float64(hit) / float64(hit+fill)
}

// accelPurge 清缓存，只动 cache_entries 与 blobs，
// Token、登录、探测历史、源站配置、流量账都不碰。
func (s *Server) accelPurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, 405, map[string]string{"error": "方法不对"})
		return
	}
	var body struct {
		Scope string `json:"scope"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body)
	scope := strings.ToLower(strings.TrimSpace(body.Scope))
	if scope == "" {
		scope = "all"
	}
	switch scope {
	// scope 用 gh 而不是 release：键前缀是 gh:，
	// 清理按键前缀走，与调度的 kind 是两套命名。
	case "all", "raw", "gh", "http", "git", "docker":
	default:
		writeJSON(w, 400, map[string]string{"error": "scope 只能是 all / raw / gh / http / git / docker"})
		return
	}

	var entries []store.CacheEntry
	var err error
	if scope == "all" {
		entries, err = s.st.CacheAll(0)
	} else {
		entries, err = s.st.CacheKeysByPrefix(scope + ":")
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	res, err := s.gc.DeleteKeys(keys)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	who := s.st.GetSetting(auth.KeyUser, auth.DefaultUser)
	log.Printf("清空缓存: 范围=%s 操作人=%s 条目=%d 释放=%dMB 待重试=%d",
		scope, who, res.Entries, res.Bytes>>20, res.Pending)
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"scope":   scope,
		"entries": res.Entries,
		"blobs":   res.Blobs,
		"bytes":   res.Bytes,
		"pending": res.Pending,
	})
}
