// Package api exposes the HTTP API and the embedded web panel.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/auth"
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
	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()

	// 公开：登录页、登录接口、健康检查
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if auth.Authed(r, s.st) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		http.ServeFileFS(w, r, sub, "login.html")
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "busy": s.eng.Busy(), "ts": time.Now().Unix()})
	})
	mux.HandleFunc("/api/login", s.login)

	// 受保护
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !auth.Authed(r, s.st) {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					writeJSON(w, 401, map[string]string{"error": "未登录"})
					return
				}
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, sub, "index.html")
	}))
	mux.HandleFunc("/api/logout", guard(s.logout))
	mux.HandleFunc("/api/me", guard(s.me))
	mux.HandleFunc("/api/health", guard(s.health))
	mux.HandleFunc("/api/sources", guard(s.sources))
	mux.HandleFunc("/api/sources/", guard(s.sourceItem))
	mux.HandleFunc("/api/series/", guard(s.series))
	mux.HandleFunc("/api/export", guard(s.export))
	mux.HandleFunc("/api/probe", guard(s.probe))
	mux.HandleFunc("/api/discover", guard(s.discover))
	mux.HandleFunc("/api/settings", guard(s.settings))
	mux.HandleFunc("/api/password", guard(s.password))

	return logRequests(mux)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/health" {
			log.Printf("%s %s %s", r.RemoteAddr, r.Method, auth.CleanPath(r.URL.Path))
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- 登录 ----

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	if !auth.Allowed(r) {
		writeJSON(w, 429, map[string]string{"error": "尝试次数过多，请 5 分钟后再试"})
		return
	}
	var in struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
		return
	}
	if !auth.Check(s.st, in.User, in.Pass) {
		auth.Fail(r)
		log.Printf("登录失败: %s user=%q", r.RemoteAddr, in.User)
		writeJSON(w, 401, map[string]string{"error": "用户名或密码不对"})
		return
	}
	auth.Reset(r)
	if err := auth.Login(w, s.st); err != nil {
		writeJSON(w, 500, map[string]string{"error": "无法创建会话"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "must_change": s.st.GetSetting(auth.KeyMustChg, "0") == "1"})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	auth.Logout(w, r, s.st)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"user":        s.st.GetSetting(auth.KeyUser, auth.DefaultUser),
		"must_change": s.st.GetSetting(auth.KeyMustChg, "0") == "1",
		"version":     "2.0.0",
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	n, _ := s.st.Count()
	writeJSON(w, 200, map[string]any{"ok": true, "busy": s.eng.Busy(), "ts": time.Now().Unix(), "sources": n})
}

// ---- 源 ----

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.st.ListSources()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if list == nil {
			list = []store.Source{}
		}
		writeJSON(w, 200, map[string]any{"sources": list, "busy": s.eng.Busy()})
	case http.MethodPost:
		s.addSource(w, r)
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func (s *Server) addSource(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
		return
	}
	base, err := normalizeURL(in.URL)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	}
	id, err := s.st.AddSource(store.Source{
		BaseURL: base, Name: name, Origin: "manual", Enabled: true, Status: "pending",
		DockerCap: store.CapUnknown, GithubCap: store.CapUnknown,
	})
	if err == store.ErrExists {
		writeJSON(w, 409, map[string]string{"error": "这个地址已经在列表里了"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// 立刻跑一次识别（两种类型都试），前端轮询这个源的状态即可
	go s.eng.RunSource(id)
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

func normalizeURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("地址不能为空")
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	v = strings.TrimRight(v, "/")
	if strings.ContainsAny(v, " ?#") {
		return "", fmt.Errorf("地址不能包含空格、问号或井号")
	}
	low := strings.ToLower(v)
	if !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "http://") {
		return "", fmt.Errorf("只支持 http/https 地址")
	}
	hostPart := strings.TrimPrefix(strings.TrimPrefix(low, "https://"), "http://")
	if hostPart == "" || !strings.Contains(hostPart, ".") {
		return "", fmt.Errorf("看起来不是一个域名：%s", raw)
	}
	if len(v) > 300 {
		return "", fmt.Errorf("地址太长")
	}
	return v, nil
}

func (s *Server) sourceItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/sources/"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "id 不对"})
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			Enabled *bool   `json:"enabled"`
			Name    *string `json:"name"`
			Note    *string `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
			return
		}
		if err := s.st.UpdateSource(id, in.Enabled, in.Name, in.Note); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if in.Enabled != nil && *in.Enabled {
			go s.eng.RunSource(id)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := s.st.DeleteSource(id); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/series/"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "id 不对"})
		return
	}
	pts, err := s.st.Series(id, 60)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if pts == nil {
		pts = []store.Check{}
	}
	writeJSON(w, 200, map[string]any{"points": pts})
}

// ---- 导出 ----

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListSources()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	limit := 6
	if n, err := strconv.Atoi(s.st.GetSetting("export_limit", "6")); err == nil && n > 0 && n <= 20 {
		limit = n
	}

	pick := func(kind string, max int) []store.Source {
		var out []store.Source
		for _, x := range list {
			if !x.Enabled || !x.Supports(kind) {
				continue
			}
			c := x.Last[kind]
			if c == nil || !c.OK || c.DigestOK == 0 {
				continue
			}
			// 测过速但慢到没有实用价值的，不推荐
			if c.SpeedKBps > 0 && c.SpeedKBps < 100 {
				continue
			}
			out = append(out, x)
		}
		sort.SliceStable(out, func(i, j int) bool { return speedOf(out[i], kind) > speedOf(out[j], kind) })
		if len(out) > max {
			out = out[:max]
		}
		return out
	}

	docker := pick("docker", limit)
	github := pick("github", 3)

	var dLines []string
	for _, x := range docker {
		dLines = append(dLines, "    "+strconv.Quote(x.BaseURL))
	}
	dSnippet := "（暂无可用源）"
	if len(dLines) > 0 {
		dSnippet = "{\n  \"registry-mirrors\": [\n" + strings.Join(dLines, ",\n") + "\n  ]\n}"
	}

	var gLines []string
	for _, x := range github {
		gLines = append(gLines, "git config --global url.\""+x.BaseURL+"/https://github.com/\".insteadOf \"https://github.com/\"")
	}
	gSnippet := strings.Join(gLines, "\n")
	if gSnippet == "" {
		gSnippet = "（暂无可用源）"
	}

	writeJSON(w, 200, map[string]any{
		"docker": map[string]any{"count": len(docker), "snippet": dSnippet},
		"github": map[string]any{"count": len(github), "snippet": gSnippet},
	})
}

func speedOf(x store.Source, kind string) int64 {
	if c := x.Last[kind]; c != nil {
		return c.SpeedKBps
	}
	return 0
}

// ---- 动作 ----

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tier int `json:"tier"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in)
	if in.Tier < 1 || in.Tier > 3 {
		in.Tier = 2
	}
	go s.eng.Run(in.Tier)
	writeJSON(w, 202, map[string]any{"ok": true, "tier": in.Tier})
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
			s.eng.Run(2)
		}
	}()
	writeJSON(w, 202, map[string]string{"status": "started"})
}

// ---- 设置 ----

var settingKeys = map[string]struct {
	def, min, max int
}{
	"l1_minutes":        {3, 1, 1440},
	"l2_minutes":        {15, 2, 1440},
	"l3_minutes":        {360, 10, 10080},
	"session_ttl_hours": {168, 1, 8760},
	"export_limit":      {6, 1, 20},
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		out := map[string]any{}
		for k, v := range settingKeys {
			n, _ := strconv.Atoi(s.st.GetSetting(k, strconv.Itoa(v.def)))
			if n < v.min || n > v.max {
				n = v.def
			}
			out[k] = n
		}
		out["user"] = s.st.GetSetting(auth.KeyUser, auth.DefaultUser)
		out["must_change"] = s.st.GetSetting(auth.KeyMustChg, "0") == "1"
		writeJSON(w, 200, out)
	case http.MethodPut:
		var in map[string]int
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
			return
		}
		for k, n := range in {
			v, ok := settingKeys[k]
			if !ok {
				continue
			}
			if n < v.min || n > v.max {
				writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("%s 需要在 %d-%d 之间", k, v.min, v.max)})
				return
			}
		}
		for k, n := range in {
			if _, ok := settingKeys[k]; !ok {
				continue
			}
			if err := s.st.SetSetting(k, strconv.Itoa(n)); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func (s *Server) password(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var in struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
		return
	}
	if len(in.New) < 8 {
		writeJSON(w, 400, map[string]string{"error": "新密码至少 8 位"})
		return
	}
	if !auth.Check(s.st, s.st.GetSetting(auth.KeyUser, auth.DefaultUser), in.Old) {
		writeJSON(w, 403, map[string]string{"error": "原密码不对"})
		return
	}
	if err := auth.SetPassword(s.st, in.New); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// 改完密码踢掉所有旧会话，然后给当前浏览器发一个新的
	s.st.DeleteAllSessions()
	if err := auth.Login(w, s.st); err != nil {
		writeJSON(w, 500, map[string]string{"error": "密码已改，但会话创建失败，请重新登录"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
