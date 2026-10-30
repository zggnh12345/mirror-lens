// Package api exposes the panel and its JSON endpoints.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/discover"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

//go:embed web
var webFS embed.FS

type Server struct {
	st  *store.Store
	eng *runner.Engine
}

func New(st *store.Store, eng *runner.Engine) http.Handler {
	s := &Server{st: st, eng: eng}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/sources", s.listSources)
	mux.HandleFunc("POST /api/sources", s.addSource)
	mux.HandleFunc("PATCH /api/sources/{id}", s.patchSource)
	mux.HandleFunc("DELETE /api/sources/{id}", s.deleteSource)
	mux.HandleFunc("GET /api/series/{id}", s.series)
	mux.HandleFunc("POST /api/probe", s.probe)
	mux.HandleFunc("POST /api/discover", s.discover)
	mux.HandleFunc("GET /api/export", s.export)

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "busy": s.eng.Busy(), "ts": time.Now().Unix()})
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "docker" && kind != "github" {
		writeJSON(w, 400, map[string]string{"error": "kind 必须是 docker 或 github"})
		return
	}
	list, err := s.st.ListSources(kind)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []store.Source{}
	}
	writeJSON(w, 200, map[string]any{"sources": list, "busy": s.eng.Busy()})
}

func normalizeBase(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", errors.New("地址不能为空")
	}
	if !strings.HasPrefix(in, "http://") && !strings.HasPrefix(in, "https://") {
		in = "https://" + in
	}
	in = strings.TrimSuffix(in, "/")
	// 只接受 host 或 host/path，不接受带 query 的粘贴
	if strings.ContainsAny(in, " ?#") {
		return "", errors.New("地址不能包含空格、问号或井号")
	}
	return in, nil
}

func (s *Server) addSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind    string `json:"kind"`
		BaseURL string `json:"base_url"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	if body.Kind != "docker" && body.Kind != "github" {
		writeJSON(w, 400, map[string]string{"error": "kind 必须是 docker 或 github"})
		return
	}
	base, err := normalizeBase(body.BaseURL)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	}
	id, err := s.st.AddSource(store.Source{Kind: body.Kind, BaseURL: base, Name: name, Origin: "manual", Enabled: true, Status: "pending"})
	if errors.Is(err, store.ErrExists) {
		writeJSON(w, 409, map[string]string{"error": "这个地址已经在列表里"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// 加完立刻验证一次，用户不用等定时任务
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.eng.Run(body.Kind, 2)
	}()
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) patchSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "id 无效"})
		return
	}
	var body struct {
		Enabled *bool   `json:"enabled"`
		Name    *string `json:"name"`
		Note    *string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	if err := s.st.UpdateSource(id, body.Enabled, body.Name, body.Note); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "id 无效"})
		return
	}
	if err := s.st.DeleteSource(id); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "id 无效"})
		return
	}
	pts, err := s.st.Series(id, 120)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if pts == nil {
		pts = []store.Check{}
	}
	writeJSON(w, 200, map[string]any{"points": pts})
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		Tier int    `json:"tier"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body)
	if body.Kind == "" {
		body.Kind = "all"
	}
	if body.Tier < 1 || body.Tier > 3 {
		body.Tier = 2
	}
	if s.eng.Busy() {
		writeJSON(w, 202, map[string]string{"status": "already running"})
		return
	}
	go func() {
		if body.Kind == "all" || body.Kind == "docker" {
			s.eng.Run("docker", body.Tier)
		}
		if body.Kind == "all" || body.Kind == "github" {
			s.eng.Run("github", body.Tier)
		}
	}()
	writeJSON(w, 202, map[string]string{"status": "started"})
}

func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	go func() {
		// 不能用 r.Context()：handler 一返回它就被取消，抓取会全部失败。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		n, err := discover.Scan(ctx, s.st)
		if err != nil {
			log.Printf("discover: %v", err)
			return
		}
		log.Printf("discover: %d new candidates", n)
		if n > 0 {
			s.eng.Run("docker", 2)
			s.eng.Run("github", 2)
		}
	}()
	writeJSON(w, 202, map[string]string{"status": "started"})
}

// export 只输出真正可用的条目，供用户手动粘贴。
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "docker" && kind != "github" {
		writeJSON(w, 400, map[string]string{"error": "kind 必须是 docker 或 github"})
		return
	}
	list, err := s.st.ListSources(kind)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var ok []store.Source
	for _, x := range list {
		if x.Status != "usable" || !x.Enabled {
			continue
		}
		if x.Last != nil && x.Last.DigestOK == 0 {
			continue
		}
		// 测过速但慢到没有实用价值（<100 KB/s）的，不推荐。
		if x.Last != nil && x.Last.SpeedKBps > 0 && x.Last.SpeedKBps < 100 {
			continue
		}
		ok = append(ok, x)
	}
	sort.SliceStable(ok, func(i, j int) bool {
		a, b := 0.0, 0.0
		if ok[i].Last != nil {
			a = float64(ok[i].Last.SpeedKBps)
		}
		if ok[j].Last != nil {
			b = float64(ok[j].Last.SpeedKBps)
		}
		return a > b
	})

	if kind == "docker" {
		urls := make([]string, 0, len(ok))
		for _, x := range ok {
			urls = append(urls, x.BaseURL)
		}
		if len(urls) > 6 {
			urls = urls[:6]
		}
		blob, _ := json.MarshalIndent(map[string]any{"registry-mirrors": urls}, "", "  ")
		writeJSON(w, 200, map[string]any{
			"snippet": string(blob),
			"count":   len(urls),
			"note":    "写入 /etc/docker/daemon.json 后需重启 docker；本工具不会自动修改任何配置。",
		})
		return
	}

	var b strings.Builder
	for _, x := range ok {
		fmt.Fprintf(&b, "git config --global url.\"%s/https://github.com/\".insteadOf \"https://github.com/\"\n", x.BaseURL)
	}
	writeJSON(w, 200, map[string]any{
		"snippet": b.String(),
		"count":   len(ok),
		"note":    "按速度排序，第一条最快。多条 insteadOf 只有最后一条生效，挑一条执行即可。",
	})
}
