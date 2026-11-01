// Package api exposes the HTTP API and the embedded web panel.
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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/discover"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/gc"
	"github.com/zggnh12345/mirror-lens/internal/janitor"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// Version 面板与 /api/me 显示的版本号。
const Version = "2.6.0-dev"

//go:embed web
var webFS embed.FS

type Server struct {
	st  *store.Store
	eng *runner.Engine
	gw  *gateway.Gateway
	gc  *gc.GC
	jan *janitor.Janitor
	sch *scheduler.Scheduler

	metricsOn    bool
	metricsToken bool
}

// Option 是 New 的可选开关，避免为了一个端点改所有调用点的签名。
type Option func(*Server)

// WithMetrics 打开 /metrics；requireToken 时要求登录或 Access Token
// （和代理路径同一套凭据），防止把流量数字暴露到公网。
func WithMetrics(enabled, requireToken bool) Option {
	return func(s *Server) {
		s.metricsOn = enabled
		s.metricsToken = requireToken
	}
}

func New(st *store.Store, eng *runner.Engine, gw *gateway.Gateway, j *janitor.Janitor, sch *scheduler.Scheduler, opts ...Option) http.Handler {
	s := &Server{st: st, eng: eng, gw: gw, jan: j, sch: sch}
	for _, o := range opts {
		o(s)
	}
	if gw != nil {
		s.gc = gw.GC()
	}
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
	// Prometheus 抓取口。默认公开（和大多数自建服务一样），公网部署可在
	// config.yaml 里把 metrics.require_token 打开，改用面板登录或 Access Token。
	if s.metricsOn {
		mux.HandleFunc("/metrics", s.metrics)
	}
	// 样式表必须公开：登录页也要用
	mux.HandleFunc("/app.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		http.ServeFileFS(w, r, sub, "app.css")
	})

	// 图标公开：登录页与浏览器标签都要用
	mux.HandleFunc("/icon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFileFS(w, r, sub, "icon.svg")
	})

	// 只校验登录，不校验「是否已经设置过账号」。设置页和登出要用它。
	authed := func(h http.HandlerFunc) http.HandlerFunc {
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

	// 受保护：登录 + 已经设置过账号。默认密码还没换掉时，除 /api/me 之外一律
	// 不放行——只在界面上弹个提示条，等于没强制。
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return authed(func(w http.ResponseWriter, r *http.Request) {
			if s.needSetup() && r.URL.Path != "/api/me" {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					writeJSON(w, 403, map[string]any{
						"error": "首次使用请先设置账号和密码",
						"code":  "setup_required",
					})
					return
				}
				http.Redirect(w, r, "/setup", http.StatusFound)
				return
			}
			h(w, r)
		})
	}

	// 首次设置页：登录之后、改完之前唯一能打开的面板页面。
	mux.HandleFunc("/setup", func(w http.ResponseWriter, r *http.Request) {
		if !auth.Authed(r, s.st) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !s.needSetup() {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		http.ServeFileFS(w, r, sub, "setup.html")
	})
	mux.HandleFunc("/api/setup", authed(s.setup))

	mux.HandleFunc("/", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, sub, "index.html")
	}))
	mux.HandleFunc("/settings", guard(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, sub, "settings.html")
	}))
	mux.HandleFunc("/api/logout", authed(s.logout))
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
	mux.HandleFunc("/api/account", guard(s.account))

	// 加速器：/raw/ 自带 Bearer/Basic 认证（客户端不会带 cookie）
	mux.HandleFunc("/raw/", s.gw.ServeRaw)
	mux.HandleFunc("/gh/", s.gw.ServeGH)
	mux.HandleFunc("/http/", s.gw.ServeHTTPProxy)
	mux.HandleFunc("/git/", s.gw.ServeGit)
	// Docker Registry V2：客户端只会拼 /v2/...，所以挂在根路径下
	if s.gw.RegistryOn() {
		mux.HandleFunc("/v2/", s.gw.ServeRegistry)
	}
	mux.HandleFunc("/api/tokens", guard(s.tokens))
	mux.HandleFunc("/api/tokens/", guard(s.tokenItem))
	mux.HandleFunc("/api/accel/stats", guard(s.accelStats))
	mux.HandleFunc("/api/accel/purge", guard(s.accelPurge))

	return logRequests(httpProxyBypass(s.gw, noStorePanel(mux)))
}

// noStorePanel 给面板和接口加 no-store：这些响应带会话和实时状态，
// 一旦被 CDN 或浏览器缓存，轻则看到旧数据，重则串号。代理路径不经过这里，
// 它们的缓存语义由 gateway.cacheHeaders 按内容类型决定。
func noStorePanel(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/") || p == "/" || p == "/login" ||
			p == "/setup" || p == "/settings" || p == "/app.css" || p == "/icon.svg" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// httpProxyBypass 在进 ServeMux 之前截住 /http/。
//
// /http/ 后面的目标地址是整体百分号编码的，路径里必然出现 %2F；
// Go 1.22+ 的 ServeMux 不允许转义斜杠落在路径段内，会直接 404。
// 这里按 EscapedPath 判断前缀，命中就交给网关自己解析。
func httpProxyBypass(gw *gateway.Gateway, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantsHTTPProxy(r) {
			gw.ServeHTTPProxy(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func wantsHTTPProxy(r *http.Request) bool {
	return strings.HasPrefix(r.URL.EscapedPath(), "/http/")
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
		log.Printf("登录失败: %s user=%s", r.RemoteAddr, maskUser(in.User))
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

// maskUser 只留首字符和长度：日志要能看出「有人在试哪个账号」，但用户名本身
// 不该落在日志里（日志会被打包、转发、贴进 issue）。
func maskUser(u string) string {
	if u == "" {
		return "(空)"
	}
	r := []rune(u)
	return fmt.Sprintf("%c***(%d)", r[0], len(r))
}

// needSetup 表示还停在「默认密码」状态。
func (s *Server) needSetup() bool {
	return s.st.GetSetting(auth.KeyMustChg, "0") == "1"
}

// setup 首次设置账号和密码。此时会话是用默认密码换来的，所以不再要旧密码，
// 但要一次性把用户名和密码都定下来。
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	if !s.needSetup() {
		writeJSON(w, 409, map[string]string{"error": "账号已经设置过了"})
		return
	}
	var in struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
		return
	}
	in.User = strings.TrimSpace(in.User)
	if !auth.ValidUser(in.User) {
		writeJSON(w, 400, map[string]string{"error": "用户名 3-32 位，只能用字母、数字、_ - ."})
		return
	}
	if len(in.Pass) < 8 {
		writeJSON(w, 400, map[string]string{"error": "密码至少 8 位"})
		return
	}
	if in.Pass == auth.DefaultPass {
		writeJSON(w, 400, map[string]string{"error": "别再用默认密码 admin"})
		return
	}
	// 认领 + 写账号 + 写密码在一个事务里，并发设置只有一个能赢。
	if err := auth.Setup(s.st, in.User, in.Pass); err != nil {
		if errors.Is(err, auth.ErrSetupDone) {
			writeJSON(w, 409, map[string]string{"error": "账号已经设置过了"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Setup 已经作废了所有旧会话（包括此刻这个），给当前浏览器发一个新的。
	if err := auth.Login(w, s.st); err != nil {
		writeJSON(w, 500, map[string]string{"error": "已保存，但会话创建失败，请重新登录"})
		return
	}
	log.Printf("初始账号已设置: user=%s", maskUser(in.User))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	auth.Logout(w, r, s.st)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"user":        s.st.GetSetting(auth.KeyUser, auth.DefaultUser),
		"must_change": s.st.GetSetting(auth.KeyMustChg, "0") == "1",
		"version":     Version,
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
	if u, perr := url.Parse(base); perr == nil {
		if gerr := probe.CheckURL(u); gerr != nil {
			writeJSON(w, 400, map[string]string{"error": "这个地址探测不了：" + gerr.Error()})
			return
		}
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
	// 防御性撤销墓碑：sources.id 是 AUTOINCREMENT，正常不会复用，但一旦
	// 复用了而墓碑还在，新源的报告和闸门会被当成「已删除」静默丢弃。
	if s.sch != nil {
		s.sch.Revive(id)
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
			// Status 只接受人工可做的三件事：归档、封禁、恢复待检。
			// 不让手动置 usable——那等于绕过探测和校验。
			Status *string `json:"status"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
			return
		}
		// 先落状态再落开关：归档源要恢复，必须同一个请求里显式给
		// status=pending，否则 store 会拒绝「启用」（ErrArchived）。
		if in.Status != nil {
			v := *in.Status
			switch v {
			case "archived":
				if err := s.st.SetStatusWhy(id, "archived", "手动归档"); err != nil {
					writeJSON(w, 500, map[string]string{"error": err.Error()})
					return
				}
			case "blocked":
				if err := s.st.SetStatusWhy(id, "blocked", "手动封禁"); err != nil {
					writeJSON(w, 500, map[string]string{"error": err.Error()})
					return
				}
			case "pending":
				// 恢复：先退回待检，让探测重新验证，不直接当可用。
				if err := s.st.SetStatusWhy(id, "pending", "手动恢复，等待复检"); err != nil {
					writeJSON(w, 500, map[string]string{"error": err.Error()})
					return
				}
				if err := s.st.TouchProbe0(id); err != nil {
					writeJSON(w, 500, map[string]string{"error": err.Error()})
					return
				}
				go s.eng.RunSource(id)
			default:
				writeJSON(w, 400, map[string]string{"error": "status 只能是 archived / blocked / pending"})
				return
			}
		}
		if in.Enabled != nil || in.Name != nil || in.Note != nil {
			if err := s.st.UpdateSource(id, in.Enabled, in.Name, in.Note); err != nil {
				if errors.Is(err, store.ErrArchived) {
					writeJSON(w, 400, map[string]string{"error": err.Error()})
					return
				}
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		// 状态/开关改了：让调度器立刻重算候选，别等 ListTTL 过期。
		s.eng.Changed()
		if in.Enabled != nil && *in.Enabled {
			go s.eng.RunSource(id)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := s.st.DeleteSource(id); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		// 库里删干净还不够：调度器的候选缓存、熔断、并发闸门、动态吞吐和
		// 逐源指标都还记着这个 id。不一起清掉，界面会继续把它排进候选。
		s.sch.Forget(id)
		s.eng.Changed()
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

// account 同时改用户名和密码：两个字段都可选，但必须带当前密码。
func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var in struct {
		Old  string `json:"old"`
		User string `json:"user"`
		New  string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式不对"})
		return
	}
	cur := s.st.GetSetting(auth.KeyUser, auth.DefaultUser)
	if !auth.Check(s.st, cur, in.Old) {
		writeJSON(w, 403, map[string]string{"error": "当前密码不对"})
		return
	}
	in.User = strings.TrimSpace(in.User)
	if in.User == "" {
		in.User = cur
	}
	if !auth.ValidUser(in.User) {
		writeJSON(w, 400, map[string]string{"error": "用户名 3-32 位，只能用字母、数字、_ - ."})
		return
	}
	if in.New != "" && len(in.New) < 8 {
		writeJSON(w, 400, map[string]string{"error": "新密码至少 8 位"})
		return
	}
	if in.User == cur && in.New == "" {
		writeJSON(w, 400, map[string]string{"error": "用户名和密码都没改"})
		return
	}

	passChanged := in.New != ""
	if passChanged {
		if err := auth.SetPassword(s.st, in.New); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	if in.User != cur {
		if err := auth.SetUser(s.st, in.User); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}

	if passChanged {
		// 密码变了，其他设备上的会话全部作废，当前浏览器换一个新令牌
		s.st.DeleteAllSessions()
		if err := auth.Login(w, s.st); err != nil {
			writeJSON(w, 500, map[string]string{"error": "已保存，但会话创建失败，请重新登录"})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": in.User, "password_changed": passChanged})
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

// metrics 输出 Prometheus 文本格式。默认公开，require_token 打开后要求
// 面板登录或 Access Token——抓取端用 Bearer 就行。
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.metricsToken && !(auth.Authed(r, s.st) && !auth.MustChange(s.st)) {
		tok := scrapeToken(r)
		if tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Mirror Lens"`)
			writeJSON(w, 401, map[string]string{"error": "需要 Access Token"})
			return
		}
		if _, ok := s.st.VerifyToken(tok); !ok {
			writeJSON(w, 401, map[string]string{"error": "Token 无效"})
			return
		}
	}
	reg := metrics.Get()
	if reg == nil {
		writeJSON(w, 503, map[string]string{"error": "指标未启用"})
		return
	}
	reg.Handler().ServeHTTP(w, r)
}

// scrapeToken 取抓取端带来的凭据：Bearer 或 Basic 的密码位。
func scrapeToken(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	if _, p, ok := r.BasicAuth(); ok {
		return p
	}
	return ""
}
