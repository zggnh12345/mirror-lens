package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/api"
	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/config"
	"github.com/zggnh12345/mirror-lens/internal/discover"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/janitor"
	"github.com/zggnh12345/mirror-lens/internal/limits"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func main() {
	// 只留四个开关，其余参数在配置文件里（见 config.example.yaml）
	cfgPath := flag.String("config", "", "配置文件路径（默认找当前目录的 config.yaml）")
	addr := flag.String("addr", "", "监听地址，覆盖配置文件")
	dataDir := flag.String("data", "", "数据目录，覆盖配置文件")
	noSeed := flag.Bool("no-seed", false, "不写入内置候选池")
	flag.Parse()

	path, required := *cfgPath, true
	if path == "" {
		// 容器里配置固定放 /config/config.yaml（compose 挂载点）；
		// 找不到再回退当前目录的 config.yaml，方便本地跑。
		if _, err := os.Stat("/config/config.yaml"); err == nil {
			path = "/config/config.yaml"
		} else {
			path, required = "config.yaml", false
		}
	}
	cfg, err := config.Load(path, required)
	if err != nil {
		log.Fatalf("读配置: %v", err)
	}
	// 三层优先级：配置文件 < 环境变量 < 命令行。顺序就是优先级，
	// 反过来写（先 flag 再 env）会让环境变量悄悄盖掉显式参数。
	applyOverrides(cfg,
		func(name string) bool {
			seen := false
			flag.Visit(func(f *flag.Flag) {
				if f.Name == name {
					seen = true
				}
			})
			return seen
		},
		os.Getenv, *addr, *dataDir, *noSeed)
	if err := cfg.Validate(); err != nil {
		log.Fatalf("配置不合法: %v", err)
	}
	// 日志要最先初始化，否则最早的几条还是默认样式。
	if err := logging.Setup(cfg.Logging); err != nil {
		log.Fatalf("初始化日志: %v", err)
	}

	// 探测与网关共用安全拨号：源地址是用户填的，不能变成内网探针。
	probe.Configure(cfg.Probe.AllowPrivate)
	if cfg.Probe.AllowPrivate {
		log.Printf("探测已允许内网地址（probe.allow_private=true），仅限自建源场景")
	}

	if err := os.MkdirAll(cfg.Data, 0o755); err != nil {
		log.Fatalf("创建数据目录: %v", err)
	}
	cstore, err := cache.New(cfg.CacheDir())
	if err != nil {
		log.Fatalf("初始化缓存: %v", err)
	}
	// 清掉上次崩溃留下的半截文件（1 小时前的）
	if n, err := cstore.SweepTemp(time.Now().Add(-time.Hour).Unix()); err == nil && n > 0 {
		log.Printf("清理了 %d 个残留临时文件", n)
	}
	st, err := store.Open(filepath.Join(cfg.Data, "mirror-lens.db"))
	if err != nil {
		log.Fatalf("打开数据库: %v", err)
	}
	defer st.Close()

	// 首次启动写入 admin/admin 并标记必须改密：改完前只放行设置页。
	if err := auth.EnsureAdmin(st); err != nil {
		log.Fatalf("初始化账号: %v", err)
	}
	if st.GetSetting(auth.KeyMustChg, "0") == "1" {
		log.Printf("当前仍是默认密码 admin：登录后必须先设置新的账号和密码，其它接口一律拒绝")
	}

	if !cfg.NoSeed {
		if n, err := discover.Seed(st); err != nil {
			log.Printf("写入候选池失败: %v", err)
		} else if n > 0 {
			log.Printf("已写入 %d 个内置候选源", n)
		}
	}
	// 老库升级出 site_name 列后是空串：按内置映射补一次名字，面板才有的显示。
	if n, err := discover.BackfillSiteNames(st); err != nil {
		log.Printf("补站点名失败: %v", err)
	} else if n > 0 {
		log.Printf("已补 %d 个源的站点名", n)
	}

	var regAd *registryadapter.Adapter
	if cfg.Registry.Enabled {
		regAd = registryadapter.New(cfg.Registry.Upstream, cfg.Registry.MaxFileMB<<20, cfg.Registry.TagTTL.Duration)
		log.Printf("Docker 加速已开: /v2/ → %s", cfg.Registry.Upstream)
	}
	gw := gateway.New(st, gateway.Options{
		Cache:             cstore,
		MaxBytes:          cfg.Cache.MaxFileMB << 20,
		GitTTL:            cfg.Git.TTL.Duration,
		RawTTL:            cfg.Raw.TTL.Duration,
		Registry:          regAd,
		CacheMaxBytes:     cfg.Cache.MaxGB << 30,
		CacheMinFreeBytes: cfg.Cache.MinFreeGB << 30,
		RequireToken:      cfg.Auth.RequireToken,
		CDN:               gateway.CDNOptions{Enabled: cfg.CDN.Enabled, MaxAge: cfg.CDN.MaxAge},
		Parallel: gateway.ParOptions{
			Enabled:    cfg.Parallel.Enabled,
			MinSize:    cfg.Parallel.MinSizeMB << 20,
			ChunkSize:  cfg.Parallel.ChunkMB << 20,
			Workers:    cfg.Parallel.Workers,
			TailVerify: !cfg.Parallel.DisableTailVerify,
		},
	})
	if cfg.Auth.RequireToken {
		log.Printf("代理路径需要 Access Token（auth.require_token=true）")
	} else {
		log.Printf("代理路径匿名放行（auth.require_token=false），面板仍需登录")
	}
	// 多源调度：只有回源才选源，缓存命中不受影响。
	var sched *scheduler.Scheduler
	if cfg.Sources.Enabled {
		sched = scheduler.New(st, scheduler.Config{
			Enabled:       true,
			MaxCandidates: cfg.Sources.MaxCandidates,
			StickyTTL:     cfg.Sources.StickyTTL.Duration,
			FailCooldown:  cfg.Sources.FailCooldown.Duration,
			MaxPerSource:  cfg.Sources.MaxPerSource,
			Dynamic:       cfg.Sources.Dynamic,
		})
		gw.SetScheduler(sched)
		log.Printf("多源调度已开：每个协议最多试 %d 个源", cfg.Sources.MaxCandidates)
	}
	eng := runner.New(st, runner.Options{
		ArchiveAfter:  time.Duration(cfg.Sources.ArchiveDays) * 24 * time.Hour,
		CleanupAfter:  time.Duration(cfg.Sources.CleanupDays) * 24 * time.Hour,
		RecoveryEvery: time.Duration(cfg.Sources.RecoveryMinutes) * time.Minute,
	})
	if sched != nil {
		// 源状态变化时立刻作废候选缓存。
		eng.OnChange(sched.Invalidate)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go eng.Loop(ctx)

	// 缓存巡检：容量、过期、半截文件、索引对账。
	// 淘汰入口用 gw.EvictTo，与写后触发走同一份代码。
	jan := janitor.New(st, cstore, gw.GC(), janitor.Options{
		Interval:      cfg.Janitor.Interval.Duration,
		PartialTTL:    cfg.Janitor.PartialTTL.Duration,
		ExpiredGrace:  cfg.Janitor.ExpiredGrace.Duration,
		CacheMaxBytes: cfg.Cache.MaxGB << 30,
		WarnAt:        cfg.Cache.WarnAt,
		RecoverAt:     cfg.Cache.RecoverAt,
		Evict:         gw.EvictTo,
	})
	if cfg.Cache.MaxGB > 0 {
		msg := fmt.Sprintf("缓存上限 %d GB", cfg.Cache.MaxGB)
		if cfg.Cache.MinFreeGB > 0 {
			msg += fmt.Sprintf("，磁盘剩余低于 %d GB 时只透传", cfg.Cache.MinFreeGB)
		}
		log.Printf("%s", msg)
	}
	go jan.Run(ctx)

	// 启动 45 秒后扫一轮，之后每天一次；新域名进 pending 池
	go func() {
		scan := func() {
			if n, err := discover.Scan(ctx, st); err != nil {
				log.Printf("发现新源失败: %v", err)
			} else if n > 0 {
				log.Printf("发现 %d 个新候选", n)
				eng.Run(2)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(45 * time.Second):
		}
		scan()

		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				scan()
			}
		}
	}()

	// 指标收集常开（几个 map 自增），开关只管是否暴露 /metrics。
	metrics.Enable()

	lim := limits.New(cfg.Limits)
	if lim.Enabled() {
		log.Printf("入口限流已开: 全局 %.0f/s 并发 %d | 每 IP %d/分 并发 %d | 每 Token %d/分 并发 %d | 可信代理 %v",
			cfg.Limits.GlobalRPS, cfg.Limits.GlobalConcurrency,
			cfg.Limits.IPPerMinute, cfg.Limits.IPConcurrency,
			cfg.Limits.TokenPerMinute, cfg.Limits.TokenConcurrency, cfg.Limits.TrustedProxies)
	} else {
		log.Printf("入口限流已关闭（limits.enabled=false）")
	}

	srv := &http.Server{
		Addr:     cfg.Addr,
		ErrorLog: log.New(logging.Writer(slog.Default(), slog.LevelWarn), "", 0),
		Handler: metrics.Middleware(lim.Wrap(api.New(st, eng, gw, jan, sched,
			api.WithMetrics(cfg.Metrics.Enabled, cfg.Metrics.RequireToken),
			api.WithConfig(cfg, path)))),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("Mirror Lens 已启动: http://%s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Print("已退出")
}

// applyOverrides 把环境变量和命令行叠到配置上：先 env，后 flag。
// set 用来判断某个 flag 是否真的被写了（flag 默认值是空串，不能靠非空判断）。
func applyOverrides(cfg *config.Config, set func(string) bool, getenv func(string) string,
	addr, data string, noSeed bool) {
	if v := getenv("ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := getenv("DATA_DIR"); v != "" {
		cfg.Data = v
	}
	if v := getenv("CACHE_DIR"); v != "" {
		cfg.Cache.Dir = v
	}
	if set("addr") {
		cfg.Addr = addr
	}
	if set("data") {
		cfg.Data = data
	}
	if set("no-seed") {
		cfg.NoSeed = noSeed
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
