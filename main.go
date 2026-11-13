package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/alert"
	"github.com/zggnh12345/mirror-lens/internal/api"
	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/config"
	"github.com/zggnh12345/mirror-lens/internal/discover"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/ghapi"
	"github.com/zggnh12345/mirror-lens/internal/janitor"
	"github.com/zggnh12345/mirror-lens/internal/limits"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/secret"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// checkRestoreJournal 在启动最早期拦住未完成的恢复切换。
// journal 只在 DB/key 成对校验通过后才由 restore.sh 删除，所以「存在」就
// 意味着配对状态未知；此时允许启动会把混合状态当成正常数据继续写。
//
// 只有明确的「文件不存在」才放行：权限错误、I/O 错误、路径不是目录等
// 情况无法确认状态，按 fail-closed 处理。
func checkRestoreJournal(dataDir string) error {
	jp := filepath.Join(dataDir, ".restore-journal.json")
	_, err := os.Stat(jp)
	switch {
	case err == nil:
		return fmt.Errorf("检测到未完成的恢复切换（%s）：已拒绝启动以免 DB/key 配对不一致；"+
			"请先运行 scripts/restore.sh --recover --data %s 完成或回滚该次恢复", jp, dataDir)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("无法确认恢复切换日志状态（%s）: %w；在确认前拒绝启动", jp, err)
	}
}

// mustSecretKey 加载或创建数据目录里的密钥文件。Host 凭据与设置页 Token
// 共用同一把密钥，AEAD 的 AAD 已经区分用途。
func mustSecretKey(dir string) []byte {
	key, err := secret.LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	if err != nil {
		log.Printf("读取凭据密钥失败: %v", err)
		return nil
	}
	return key
}

// hostProbeScanInterval 是捞到期恢复任务的节拍，同时是恢复延迟上界。
// 可用 ML_HOSTPROBE_SCAN 覆盖。
func hostProbeScanInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ML_HOSTPROBE_SCAN")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("ML_HOSTPROBE_SCAN=%q 无效，用默认 30s", v)
	}
	return 30 * time.Second
}

// hostProbeInterval 默认 6 小时；失败源靠 Runner 内部退避重试，不受这个
// 周期约束。可用 ML_HOSTPROBE_INTERVAL 覆盖。
func hostProbeInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ML_HOSTPROBE_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("ML_HOSTPROBE_INTERVAL=%q 无效，用默认 6h", v)
	}
	return 6 * time.Hour
}

// parseHostProbeRepos 解析 ML_HOSTPROBE_REPOS="quay.io=org/repo,..."。
func parseHostProbeRepos(v string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		host, repo, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		host, repo = strings.TrimSpace(host), strings.TrimSpace(repo)
		if host != "" && repo != "" {
			out[host] = repo
		}
	}
	return out
}

func main() {
	// 只留四个开关，其余参数在配置文件里（见 config.example.yaml）
	cfgPath := flag.String("config", "", "配置文件路径（默认找当前目录的 config.yaml）")
	addr := flag.String("addr", "", "监听地址，覆盖配置文件")
	dataDir := flag.String("data", "", "数据目录，覆盖配置文件")
	noSeed := flag.Bool("no-seed", false, "不写入内置候选池")
	flag.Parse()

	path, required := *cfgPath, true
	if path == "" {
		// 容器里配置固定放 /config/config.yaml（compose 挂载点）。
		// 判据是「目录在不在」而不是「文件在不在」：首次启动文件还不存在，
		// 若按文件判断就会回落到工作目录 /data，把配置写进数据卷。
		if fi, err := os.Stat("/config"); err == nil && fi.IsDir() {
			path, required = "/config/config.yaml", false
		} else {
			path, required = "config.yaml", false
		}
	}
	cfg, err := config.Load(path, required)
	if err != nil {
		log.Fatalf("读配置: %v", err)
	}
	// 文件不存在时直接落一份完整模板：默认值都写出来，照着改就行。
	// 放在环境变量/命令行覆盖之前，模板里是纯默认值，不会被部署参数固化。
	if wrote, werr := config.WriteTemplate(path, cfg); werr != nil {
		log.Printf("生成配置模板失败: %v（目录需对容器内 uid 10001 可写：chown -R 10001:10001 ./config）", werr)
	} else if wrote {
		log.Printf("已生成完整配置模板: %s（改完需重启生效）", path)
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
	// 告警环只收 error 级：单个请求的回源失败仍留在普通日志里。
	alerts := alert.New(200)
	if err := logging.SetupWith(cfg.Logging, func(h slog.Handler) slog.Handler {
		return alert.Wrap(h, alerts)
	}); err != nil {
		log.Fatalf("初始化日志: %v", err)
	}

	// 探测与网关共用安全拨号：源地址是用户填的，不能变成内网探针。
	probe.Configure(cfg.Probe.AllowPrivate, cfg.Network.IPFamily)
	if cfg.Probe.AllowPrivate {
		log.Printf("探测已允许内网地址（probe.allow_private=true），仅限自建源场景")
	}

	if err := os.MkdirAll(cfg.Data, 0o755); err != nil {
		log.Fatalf("创建数据目录: %v", err)
	}
	dbPath := filepath.Join(cfg.Data, "mirror-lens.db")
	// 恢复脚本的切换日志存在即说明上次切换未完成；带着混合的 DB/key 启动
	// 会把「能打开」误当成「数据完整」，必须在打开数据库、甚至生成密钥前拦住。
	if err := checkRestoreJournal(cfg.Data); err != nil {
		log.Fatalf("%v", err)
	}
	// 必须在 store.Open（会创建空库）之前判定：已有库但缺 key 时 fail closed。
	startupKey, kerr := secret.LoadKeyForStartup(cfg.Data, dbPath)
	if kerr != nil {
		log.Fatalf("启动被拒绝: %v", kerr)
	}
	// 设置页保存的 Token 是密文；密钥在数据目录里，跟着数据卷走。
	// 解不开时按「未配置」继续跑：不能因为一个可选凭据让服务起不来。
	if cfg.GitHub.TokenEnc != "" {
		if plain, derr := secret.Decrypt(startupKey, cfg.GitHub.TokenEnc); derr != nil {
			log.Printf("解密 GitHub Token 失败，按未配置处理: %v", derr)
		} else {
			cfg.GitHub.TokenPlain = plain
			log.Printf("已加载设置页保存的 GitHub Token（Release 摘要 API 可用）")
		}
	}
	cstore, err := cache.New(cfg.CacheDir())
	if err != nil {
		log.Fatalf("初始化缓存: %v", err)
	}
	// 清掉上次崩溃留下的半截文件（1 小时前的）
	if n, err := cstore.SweepTemp(time.Now().Add(-time.Hour).Unix()); err == nil && n > 0 {
		log.Printf("清理了 %d 个残留临时文件", n)
	}
	st, err := store.Open(dbPath)
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
			logging.Errorf("写入候选池失败: %v", err)
		} else if n > 0 {
			log.Printf("已写入 %d 个内置候选源", n)
		}
	}
	// 老库升级出 site_name 列后是空串：按内置映射补一次名字，面板才有的显示。
	if n, err := discover.BackfillSiteNames(st); err != nil {
		logging.Errorf("补站点名失败: %v", err)
	} else if n > 0 {
		log.Printf("已补 %d 个源的站点名", n)
	}

	var regAd *registryadapter.Adapter
	if cfg.Registry.Enabled {
		regAd = registryadapter.New(cfg.Registry.Upstream, cfg.Registry.MaxFileMB<<20, cfg.Registry.TagTTL.Duration)
		log.Printf("Registry 加速已开: /v2/ → %s（docker=%v ghcr=%v）", cfg.Registry.Upstream, cfg.Registry.Docker, cfg.Registry.GHCR)
	}
	if !cfg.GitHub.Enabled {
		log.Printf("GitHub 加速已关: /gh/ /raw/ /git/ 返回 404")
	}
	if cfg.Network.IPFamily != "auto" {
		log.Printf("回源 IP 族: 仅 %s", cfg.Network.IPFamily)
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
		// 与探测共用放行策略：自建局域网源要能回源，否则面板允许探测、
		// 真正下载时又被拦下，等于配置无效。
		Guard: safehttp.New(safehttp.Options{MaxRedirects: 6, AllowPrivate: cfg.Probe.AllowPrivate, IPFamily: cfg.Network.IPFamily}),
		// 三个独立加速开关：总开关关闭时 regAd 为 nil，/v2/ 整体不挂。
		DockerOff: !cfg.Registry.Docker,
		GHCROff:   !cfg.Registry.GHCR,
		GitHubOff: !cfg.GitHub.Enabled,
		GHMetadata: func() *ghapi.Client {
			tok := cfg.GitHub.Token()
			if tok == "" {
				return nil
			}
			return ghapi.New(tok)
		}(),
		CDN: gateway.CDNOptions{Enabled: cfg.CDN.Enabled, MaxAge: cfg.CDN.MaxAge},
		Parallel: gateway.ParOptions{
			Enabled:              cfg.Parallel.Enabled,
			MinSize:              cfg.Parallel.MinSizeMB << 20,
			RangeTTL:             cfg.Parallel.RangeTTL.Duration,
			DegradeMBps:          cfg.Parallel.DegradeMBps,
			DegradeHold:          time.Duration(cfg.Parallel.DegradeHoldMin) * time.Minute,
			WatchMBps:            cfg.Parallel.WatchMBps,
			WatchWindow:          time.Duration(cfg.Parallel.WatchSec) * time.Second,
			SwitchMinMBps:        cfg.Parallel.SwitchMinMBps,
			SwitchMinFileMiB:     cfg.Parallel.SwitchMinFileMiB,
			SwitchMaxSentMiB:     cfg.Parallel.SwitchMaxSentMiB,
			SwitchMax:            cfg.Parallel.SwitchMax,
			SwitchGap:            time.Duration(cfg.Parallel.SwitchGapSec) * time.Second,
			ChunkSize:            cfg.Parallel.ChunkMB << 20,
			Workers:              cfg.Parallel.Workers,
			TailVerify:           !cfg.Parallel.DisableTailVerify,
			PreflightConcurrency: cfg.Sources.PreflightConcurrency,
			RelayConcurrency:     cfg.Sources.RelayConcurrency,
			GitConcurrency:       cfg.Sources.GitConcurrency,
		},
		Private: gateway.PrivateOptions{
			BreakAfter:  cfg.Private.BreakAfter,
			BreakFor:    time.Duration(cfg.Private.BreakSeconds) * time.Second,
			FallbackSem: cfg.Private.FallbackConcurrency,
		},
		Hedge: gateway.HedgeOptions{
			Enabled: cfg.Hedge.Enabled,
			After:   time.Duration(cfg.Hedge.AfterMS) * time.Millisecond,
			Grace:   time.Duration(cfg.Hedge.GraceMS) * time.Millisecond,
		},
	})
	if cfg.Auth.RequireToken {
		log.Printf("代理路径需要 Access Token（auth.require_token=true）")
	} else {
		log.Printf("代理路径匿名放行（auth.require_token=false），面板仍需登录")
	}
	// 路由层 fail-closed 屏蔽集：进程内只创建一次，Store / Scheduler /
	// Runner 共享同一指针。Store 未注入 Guard 时 API 直调路径会静默放行，
	// 所以装配失败必须直接终止启动。
	guard := store.NewRecoveryGuard()
	if gerr := st.SetRecoveryGuard(guard); gerr != nil {
		log.Fatalf("Recovery Guard 装配失败: %v", gerr)
	}
	if st.RecoveryGuard() != guard {
		log.Fatalf("Recovery Guard 未共享同一实例")
	}
	st.SetRecoveryTTL(recoveryTTL())
	log.Printf("成功结论有效期 %v（ML_RECOVERY_TTL）", st.RecoveryTTL())

	// 多源调度：只有回源才选源，缓存命中不受影响。
	var sched *scheduler.Scheduler
	if cfg.Sources.Enabled {
		// 速度项饱和点：默认 12MB/s（生产 P75）。1.5MB/s 的老值会让 77% 的源同分。
		speedCap := 12000.0
		if v := strings.TrimSpace(os.Getenv("ML_SPEED_CAP_KBPS")); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				speedCap = f
			} else {
				log.Printf("ML_SPEED_CAP_KBPS=%q 无效，用默认 12000", v)
			}
		}
		sched = scheduler.New(st, scheduler.Config{
			Enabled:       true,
			MaxCandidates: cfg.Sources.MaxCandidates,
			StickyTTL:     cfg.Sources.StickyTTL.Duration,
			FailCooldown:  cfg.Sources.FailCooldown.Duration,
			MaxPerSource:  cfg.Sources.MaxPerSource,
			Dynamic:       cfg.Sources.Dynamic,
			Headroom:      cfg.Sources.Headroom,
			Admission:     cfg.Sources.Admission,
			WaitTimeout:   cfg.Sources.WaitTimeout.Duration,
			SpeedCapKBps:  speedCap,
		})
		gw.SetScheduler(sched)
		log.Printf("多源调度已开：每个协议最多试 %d 个源，速度饱和点 %.0f KB/s", cfg.Sources.MaxCandidates, speedCap)
	}
	probe.SetSampleCap(int64(cfg.Probe.SampleMiB) << 20)
	eng := runner.New(st, runner.Options{
		ArchiveAfter:  time.Duration(cfg.Sources.ArchiveDays) * 24 * time.Hour,
		CleanupAfter:  time.Duration(cfg.Sources.CleanupDays) * 24 * time.Hour,
		RecoveryEvery: time.Duration(cfg.Sources.RecoveryMinutes) * time.Minute,
		Interval:      time.Duration(cfg.Probe.IntervalSec) * time.Second,
		Concurrency:   cfg.Probe.Concurrency,
		Timeout:       time.Duration(cfg.Probe.TimeoutSec) * time.Second,
	})
	if sched != nil {
		// 源状态变化时立刻作废候选缓存。
		eng.OnChange(sched.Invalidate)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go eng.Loop(ctx)

	// 仓库凭据与 Host 探测：对账成功才允许启动探测 Runner。
	// 对账失败只关掉 Host 探测，不影响现有回源（阶段 4 仍不接业务路径）。
	hostCreds, cerr := secret.NewCredStore(filepath.Join(cfg.Data, "registry_creds.enc"),
		mustSecretKey(cfg.Data), st)
	if cerr != nil {
		log.Printf("仓库凭据存储初始化失败，Host 探测不启动: %v", cerr)
		hostCreds = nil
	} else if _, rerr := hostCreds.Reconcile(); rerr != nil {
		log.Printf("仓库凭据对账失败，Host 探测不启动: %v", rerr)
		hostCreds = nil
	}
	if hostCreds != nil {
		go runner.NewHostProbeRunner(runner.HostProbeOptions{
			Store:        st,
			Creds:        hostCreds,
			Repos:        parseHostProbeRepos(os.Getenv("ML_HOSTPROBE_REPOS")),
			Interval:     hostProbeInterval(),
			ScanInterval: hostProbeScanInterval(),
			Timeout:      time.Duration(cfg.Probe.TimeoutSec) * time.Second,
			Concurrency:  cfg.Probe.Concurrency,
			RecoveryTTL:  st.RecoveryTTL(),
			Guard:        guard,
			// 事务提交不会刷新调度器内存缓存，失败与成功都必须显式失效。
			Invalidate: func() {
				if sched != nil {
					sched.Invalidate()
				}
			},
		}).Run(ctx)
	}

	// 数据库快照目录：留空跟 data 走，宿主上就是 <挂载目录>/backup。
	backupDir := ""
	backupKeep := cfg.Backup.Keep
	if backupKeep <= 0 {
		backupKeep = 7
	}
	if cfg.Backup.Enabled {
		backupDir = cfg.Backup.Dir
		if backupDir == "" {
			backupDir = filepath.Join(cfg.Data, "backup")
		}
		log.Printf("数据库每日快照: %s（保留 %d 份）", backupDir, backupKeep)
	}

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
		BackupDir:     backupDir,
		BackupKeep:    backupKeep,
		BackupKey:     filepath.Join(cfg.Data, "secret.key"),
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
				logging.Errorf("发现新源失败: %v", err)
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

	// 登录限流和 Cookie Secure 也按同一份可信代理判定走，
	// 否则 Traefik 后面所有登录都共用它的地址。
	auth.SetTrustedProxies(cfg.Limits.TrustedProxies)

	lim := limits.New(cfg.Limits)
	if lim.Enabled() {
		log.Printf("入口限流已开: 全局 %.0f/s 并发 %d | 每 IP %d/分 并发 %d | 每 Token %d/分 并发 %d | 可信代理 %v",
			cfg.Limits.GlobalRPS, cfg.Limits.GlobalConcurrency,
			cfg.Limits.IPPerMinute, cfg.Limits.IPConcurrency,
			cfg.Limits.TokenPerMinute, cfg.Limits.TokenConcurrency, cfg.Limits.TrustedProxies)
	} else {
		log.Printf("入口限流已关闭（limits.enabled=false）")
	}

	// 访问控制套在限流器外层：被 ACL 拒绝的请求不占配额，避免刷 403 把管理员限流掉。
	app := api.New(st, eng, gw, jan, sched,
		api.WithMetrics(cfg.Metrics.Enabled, cfg.Metrics.RequireToken),
		api.WithConfig(cfg, path), api.WithAlerts(alerts))

	srv := &http.Server{
		Addr:              cfg.Addr,
		ErrorLog:          log.New(logging.Writer(slog.Default(), slog.LevelWarn), "", 0),
		Handler:           metrics.Middleware(app.Access(lim.Wrap(app))),
		ReadHeaderTimeout: 10 * time.Second,
		// 默认 1MB 头部足够被慢速客户端刷爆；面板与代理的头部都很小。
		MaxHeaderBytes: 64 << 10,
		// Go 的零值回落到 ReadHeaderTimeout（10s），突发拉取会反复重连；
		// 90s 与常见反代的 keep-alive 对齐。
		IdleTimeout: 90 * time.Second,
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

// recoveryTTL 是成功结论的有效期。候选准入、调度缓存与 Runner 写入共用
// 同一个值，避免「查询按 24h 判、Runner 按 6h 写」的口径分裂。
func recoveryTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("ML_RECOVERY_TTL"))
	if v == "" {
		return store.RecoveryDefaultTTL
	}
	d, err := time.ParseDuration(v)
	// cutoff 是秒级（now - TTL 向下取整），小于 1s 的正数会被截断成 0，
	// 语义上等于「全部失效」，因此直接拒绝并回落默认值。
	if err != nil || d < time.Second {
		log.Printf("ML_RECOVERY_TTL=%q 无效（最小 1s），用默认 %v", v, store.RecoveryDefaultTTL)
		return store.RecoveryDefaultTTL
	}
	return d
}
