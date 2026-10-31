package main

import (
	"context"
	"errors"
	"flag"
	"log"
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
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func main() {
	// 只留四个开关，其余参数都在配置文件里（见 config.example.yaml）
	cfgPath := flag.String("config", "", "配置文件路径（默认找当前目录的 config.yaml）")
	addr := flag.String("addr", "", "监听地址，覆盖配置文件")
	dataDir := flag.String("data", "", "数据目录，覆盖配置文件")
	noSeed := flag.Bool("no-seed", false, "不写入内置候选池")
	flag.Parse()

	path, required := *cfgPath, true
	if path == "" {
		path, required = "config.yaml", false
	}
	cfg, err := config.Load(path, required)
	if err != nil {
		log.Fatalf("读配置: %v", err)
	}
	// 命令行显式给了才覆盖，没给就尊重配置文件
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			cfg.Addr = *addr
		case "data":
			cfg.Data = *dataDir
		case "no-seed":
			cfg.NoSeed = *noSeed
		}
	})
	cfg.Addr = envOr("ADDR", cfg.Addr)
	cfg.Data = envOr("DATA_DIR", cfg.Data)
	cfg.Cache.Dir = envOr("CACHE_DIR", cfg.Cache.Dir)
	if err := cfg.Validate(); err != nil {
		log.Fatalf("配置不合法: %v", err)
	}

	// 探测和网关共用一套安全拨号策略：源地址是用户填的，不能让它变成内网探针。
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

	// 首次启动写入默认账号 admin/admin，并标记「必须改」：改完之前面板只放行设置页。
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

	var regAd *registryadapter.Adapter
	if cfg.Registry.Enabled {
		regAd = registryadapter.New(cfg.Registry.Upstream, cfg.Registry.MaxFileMB<<20, cfg.Registry.TagTTL.Duration)
		log.Printf("Docker 加速已开: /v2/ → %s", cfg.Registry.Upstream)
	}
	gw := gateway.New(st, gateway.Options{
		Cache:         cstore,
		MaxBytes:      cfg.Cache.MaxFileMB << 20,
		GitTTL:        cfg.Git.TTL.Duration,
		RawTTL:        cfg.Raw.TTL.Duration,
		Registry:      regAd,
		CacheMaxBytes: cfg.Cache.MaxGB << 30,
		RequireToken:  cfg.Auth.RequireToken,
		CDN:           gateway.CDNOptions{Enabled: cfg.CDN.Enabled, MaxAge: cfg.CDN.MaxAge},
		Parallel: gateway.ParOptions{
			Enabled:   cfg.Parallel.Enabled,
			MinSize:   cfg.Parallel.MinSizeMB << 20,
			ChunkSize: cfg.Parallel.ChunkMB << 20,
			Workers:   cfg.Parallel.Workers,
		},
	})
	if cfg.Auth.RequireToken {
		log.Printf("代理路径需要 Access Token（auth.require_token=true）")
	} else {
		log.Printf("代理路径匿名放行（auth.require_token=false），面板仍需登录")
	}
	// 多源调度：只有「回源」才选源，缓存命中与重新验证都不受影响。
	var sched *scheduler.Scheduler
	if cfg.Sources.Enabled {
		sched = scheduler.New(st, scheduler.Config{
			Enabled:       true,
			MaxCandidates: cfg.Sources.MaxCandidates,
			StickyTTL:     cfg.Sources.StickyTTL.Duration,
			FailCooldown:  cfg.Sources.FailCooldown.Duration,
			MaxPerSource:  cfg.Sources.MaxPerSource,
		})
		gw.SetScheduler(sched)
		log.Printf("多源调度已开：每个协议最多试 %d 个源", cfg.Sources.MaxCandidates)
	}
	eng := runner.New(st, runner.Options{
		ArchiveAfter:  time.Duration(cfg.Sources.ArchiveDays) * 24 * time.Hour,
		CleanupAfter:  time.Duration(cfg.Sources.CleanupDays) * 24 * time.Hour,
		RecoveryEvery: time.Duration(cfg.Sources.RecoveryMinutes) * time.Minute,
	})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go eng.Loop(ctx)

	// 缓存巡检：容量、过期、半截文件、索引对账。
	// 淘汰入口传 gw.EvictTo，和写后触发的那次是同一份代码。
	jan := janitor.New(st, cstore, gw.GC(), janitor.Options{
		Interval:      cfg.Janitor.Interval.Duration,
		PartialTTL:    cfg.Janitor.PartialTTL.Duration,
		ExpiredGrace:  cfg.Janitor.ExpiredGrace.Duration,
		CacheMaxBytes: cfg.Cache.MaxGB << 30,
		WarnAt:        cfg.Cache.WarnAt,
		RecoverAt:     cfg.Cache.RecoverAt,
		Evict:         gw.EvictTo,
	})
	go jan.Run(ctx)

	// 每天扫一次公开列表，新域名进 pending 池
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := discover.Scan(ctx, st); err != nil {
					log.Printf("发现新源失败: %v", err)
				} else if n > 0 {
					log.Printf("发现 %d 个新候选", n)
					eng.Run(2)
				}
			}
		}
	}()

	// 指标收集常开（开销只是几个 map 自增），开关只管要不要暴露 /metrics。
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
		Addr: cfg.Addr,
		Handler: metrics.Middleware(lim.Wrap(api.New(st, eng, gw, jan, sched,
			api.WithMetrics(cfg.Metrics.Enabled, cfg.Metrics.RequireToken)))),
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
