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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/alert"
	"github.com/zggnh12345/mirror-lens/internal/api"
	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/config"
	"github.com/zggnh12345/mirror-lens/internal/dblock"
	"github.com/zggnh12345/mirror-lens/internal/discover"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/gc"
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
	"github.com/zggnh12345/mirror-lens/internal/unified"
	"github.com/zggnh12345/mirror-lens/internal/version"
)

// 多源范围只来自配置；没有单独的配置总开关，也没有环境变量覆盖。
// 空列表表示没有任何 Host 使用多源。
func copyMultiOriginHosts(hosts []string) []string {
	return append([]string(nil), hosts...)
}

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

// checkUpgradeJournal 拦住未完成的离线 DB 切换。升级脚本在动正式库之前
// 写 journal，成功切换后才删除；只要它存在，正式库可能处在「旧库已移走、
// 新库未就位」的中间态，必须先用脚本的 --recover 完成或回滚。
func checkUpgradeJournal(dataDir string) error {
	jp := filepath.Join(dataDir, ".db-upgrade-journal.json")
	_, err := os.Stat(jp)
	switch {
	case err == nil:
		return fmt.Errorf("检测到未完成的数据库升级切换（%s）：已拒绝启动；"+
			"请先运行 scripts/upgrade-db.sh --recover --data %s", jp, dataDir)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("无法确认数据库升级日志状态（%s）: %w；在确认前拒绝启动", jp, err)
	}
}

// credSource 把 secret.Store 适配成 gateway 需要的只读视图。
// 适配放在装配层：gateway 不 import secret，secret 也不需要知道 gateway。
type credSource struct{ s *secret.Store }

func (c credSource) CredentialView(host string) (gateway.CredentialView, error) {
	snap, err := c.s.Snapshot(host)
	switch {
	case errors.Is(err, secret.ErrCredentialPending):
		return gateway.CredentialView{}, gateway.ErrCredPending
	case errors.Is(err, secret.ErrNotReconciled):
		return gateway.CredentialView{}, gateway.ErrCredNotReconciled
	case errors.Is(err, secret.ErrCorrupt):
		return gateway.CredentialView{}, gateway.ErrCredQuarantined
	case err != nil:
		return gateway.CredentialView{}, err
	}
	return gateway.CredentialView{
		Host:          snap.Host,
		Generation:    snap.Generation,
		Digest:        snap.Digest,
		HasCredential: snap.HasCredential,
		Username:      snap.Credential.Username,
		Password:      snap.Credential.Password,
		Mode:          snap.Credential.Mode,
		TokenMethod:   snap.Credential.TokenMethod,
		TokenAuth:     snap.Credential.TokenAuth,
	}, nil
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
	showVersion := flag.Bool("version", false, "打印版本并退出（不读配置、不连数据库、不启动服务）")
	flag.Parse()

	// 版本查询必须发生在任何配置/数据目录/数据库动作之前。
	if *showVersion {
		fmt.Println(version.Info())
		return
	}

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
	// 配置迁移必须在 Load 之前：Load 保持纯读取，迁移只在这里发生一次。
	if rep, err := config.MigrateFile(path); err != nil {
		log.Fatalf("配置迁移: %v", err)
	} else {
		if rep.Migrated {
			log.Printf("配置迁移完成：parallel.switch_min_mbps → parallel.switch_floor_mib_per_sec；备份 %s", rep.BackupPath)
		}
		if rep.Warning != "" {
			log.Printf("配置迁移警告: %s", rep.Warning)
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
	// 启动即记录版本与构建信息：排障时先确认镜像版本。
	log.Printf("%s", version.Info())

	// 探测与网关共用安全拨号：源地址是用户填的，不能变成内网探针。
	probe.Configure(cfg.Probe.AllowPrivate, cfg.Network.IPFamily)
	if cfg.Probe.AllowPrivate {
		log.Printf("探测已允许内网地址（probe.allow_private=true），仅限自建源场景")
	}

	if err := os.MkdirAll(cfg.Data, 0o755); err != nil {
		log.Fatalf("创建数据目录: %v", err)
	}
	// 数据库单实例锁必须早于任何打开/创建数据库的动作。锁文件固定在数据
	// 目录，不依赖数据库 inode；服务、离线迁移和恢复脚本共用同一把锁。
	dbLock, lockErr := dblock.Acquire(filepath.Join(cfg.Data, dblock.Name))
	if lockErr != nil {
		log.Fatalf("数据目录已被占用: %v", lockErr)
	}
	defer dbLock.Close()
	dbPath := filepath.Join(cfg.Data, "mirror-lens.db")
	// 恢复脚本的切换日志存在即说明上次切换未完成；带着混合的 DB/key 启动
	// 会把「能打开」误当成「数据完整」，必须在打开数据库、甚至生成密钥前拦住。
	if err := checkRestoreJournal(cfg.Data); err != nil {
		log.Fatalf("%v", err)
	}
	if err := checkUpgradeJournal(cfg.Data); err != nil {
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
		regAd.MultiOriginHosts = copyMultiOriginHosts(cfg.Registry.MultiOriginHosts)
		log.Printf("Registry 加速已开: /v2/ → %s（docker=%v ghcr=%v 多源范围=%d 禁用Host=%d）",
			cfg.Registry.Upstream, cfg.Registry.Docker, cfg.Registry.GHCR,
			len(regAd.MultiOriginHosts), len(cfg.Registry.DisabledHosts))
	}
	if !cfg.GitHub.Enabled {
		log.Printf("GitHub 加速已关: /gh/ /raw/ /git/ 返回 404")
	}
	if cfg.Network.IPFamily != "auto" {
		log.Printf("回源 IP 族: 仅 %s", cfg.Network.IPFamily)
	}
	policyResolver := store.NewStorePolicy(st)

	// 仓库凭据：3.5.15 起同时服务 Host 探测与 Gateway 私有认证。
	// 文件级损坏才是致命错误；单 Host 损坏只隔离该 Host，其余继续 Ready。
	var hostCreds *secret.Store
	var cacheEpoch int64
	// R04：凭据文件存在但没有密钥 = 无法解密，必须 fail closed。
	// 自动生成新密钥会让全部记录解不开（转 Quarantined），还会把
	// 密钥路径覆盖掉，等于悄悄销毁现有凭据。
	credPath := filepath.Join(cfg.Data, "registry_creds.enc")
	if _, serr := os.Stat(credPath); serr == nil {
		if _, kerr := os.Stat(filepath.Join(cfg.Data, "secret.key")); kerr != nil {
			log.Fatalf("凭据文件存在但 secret.key 缺失，拒绝自动生成新密钥（请恢复原密钥或先备份并删除 %s）", credPath)
		}
	}
	hostCreds, cerr := secret.NewCredStore(credPath, mustSecretKey(cfg.Data), st)
	if cerr != nil {
		log.Printf("仓库凭据存储初始化失败：Host 探测与私有认证都不启动: %v", cerr)
		hostCreds = nil
	} else {
		n, quarantined, rerr := hostCreds.ReconcileAll()
		if rerr != nil {
			log.Printf("仓库凭据对账失败（文件级损坏），私有认证 fail closed: %v", rerr)
		} else {
			log.Printf("仓库凭据对账完成：变更 %d 条，隔离 %d 个 Host", n, len(quarantined))
		}
		for _, q := range quarantined {
			log.Printf("凭据 Host 已隔离: %s（%s）", q.Host, q.Reason)
		}
	}

	// 缓存命名空间：3.5.15 首次启动递增一次，让升级前的旧键空间不可达。
	// 标记位保证只切一次，不会每次重启都清空缓存。
	var epochBumped bool
	if e, bumped, eerr := st.InitCacheEpoch(); eerr != nil {
		log.Fatalf("初始化 Registry 缓存 epoch 失败: %v", eerr)
	} else {
		cacheEpoch = e
		epochBumped = bumped
		if bumped {
			log.Printf("Registry 缓存 epoch 已切换到 %d（历史条目不可达）", e)
		}
	}
	if regAd != nil {
		regAd.Epoch = func() int64 { return atomic.LoadInt64(&cacheEpoch) }
	}

	// 私有认证准入：Host 必须是代码 allowlist 子集，且必须有有效 Access Token。
	privAuth := gateway.PrivateAuthOptions{
		Enabled:      cfg.Registry.PrivateAuthEnabled,
		Hosts:        map[string]bool{},
		RequireToken: cfg.Auth.RequireToken,
	}
	if hostCreds != nil {
		privAuth.Creds = credSource{s: hostCreds}
	}
	if cfg.Registry.PrivateAuthEnabled {
		if !cfg.Auth.RequireToken {
			log.Fatalf("registry.private_auth_enabled=true 必须同时 auth.require_token=true")
		}
		if hostCreds == nil {
			log.Fatalf("私有认证已开启但凭据存储不可用")
		}
		toks, terr := st.ListTokens()
		if terr != nil {
			log.Fatalf("读取 Access Token 失败: %v", terr)
		}
		valid := false
		for _, tk := range toks {
			if !tk.Revoked {
				valid = true
				break
			}
		}
		if !valid {
			log.Fatalf("私有认证已开启但没有任何有效 Access Token，拒绝启动")
		}
		for _, h := range cfg.Registry.PrivateAuthHosts {
			if !secret.CredentialHostAllowed(h) {
				log.Fatalf("registry.private_auth_hosts 含未授权 Host %q（代码 allowlist: %v）",
					h, secret.CredentialCapableHosts())
			}
			privAuth.Hosts[h] = true
		}
		// 私有认证必须跑在 epoch>=1 的键空间上；epoch=0 与 3.5.14c 相同，
		// 无法隔离升级前的私有内容。
		e, eerr := st.EnsureCacheEpoch(1)
		if eerr != nil {
			log.Fatalf("私有认证需要 epoch>=1，切换失败: %v", eerr)
		}
		// 私有 Host 集合变化（新增/删除）会让「原本公共的内容」变成私有：
		// 必须递增 epoch 让旧键不可达，并留审计。
		hosts := append([]string(nil), cfg.Registry.PrivateAuthHosts...)
		sort.Strings(hosts)
		sig := strings.Join(hosts, ",")
		prev, perr := st.MetaGet(metaPrivateHosts)
		if perr != nil {
			log.Fatalf("读取私有 Host 记录失败: %v", perr)
		}
		// 首次开启（prev 为空）也必须切：此前缓存的公共内容在开启后
		// 属于私有 Host，不能继续命中。只有「本进程刚切过 epoch」才跳过。
		if prev != sig && (prev != "" || !epochBumped) {
			e, eerr = st.BumpCacheEpoch()
			if eerr != nil {
				log.Fatalf("私有 Host 集合变化，递增缓存 epoch 失败: %v", eerr)
			}
			st.AuditCacheEpoch(e, "system", "private_hosts_changed: "+prev+" -> "+sig)
			log.Printf("私有 Host 集合变化（%s → %s），缓存 epoch 递增到 %d", prev, sig, e)
		}
		if perr := st.MetaSet(metaPrivateHosts, sig); perr != nil {
			log.Printf("记录私有 Host 集合失败: %v", perr)
		}
		atomic.StoreInt64(&cacheEpoch, e)
		log.Printf("私有认证已开：%v（Access Token 必需，缓存绕过生效）", cfg.Registry.PrivateAuthHosts)
	} else if len(cfg.Registry.PrivateAuthHosts) > 0 {
		log.Printf("registry.private_auth_hosts 已配置但 private_auth_enabled=false，不生效")
	}

	// 3.5.18 18a：把旧 B 配置一次性迁移为新三字段（settings）。幂等；
	// 迁移失败不得改变 B 启停状态——失败即拒绝启动，避免静默缺省。
	if _, merr := st.MigrateBConfig(cfg.Registry.ReplicaMode, "system", ""); merr != nil {
		log.Fatalf("B 配置迁移失败，拒绝启动: %v", merr)
	}

	// 3.5.17 G：全服务 RequestScope 并发容量。N 取 limits.global_concurrency
	// （limits.enabled=false 时仍作为内部硬上限，不得无限大），W 取分片 worker 数。
	gw := gateway.New(st, gateway.Options{
		Pool: unified.NewPool(unified.GlobalCapacity(cfg.Limits.GlobalConcurrency, cfg.Parallel.Workers, 2, 2)),
		// 3.5.17 部署期全局回退（T62）：配置显式关掉时整条请求都不进新框架。
		UnifiedOff:        cfg.Registry.UnifiedFramework != nil && !*cfg.Registry.UnifiedFramework,
		ReplicaMode:       cfg.Registry.ReplicaMode,
		Cache:             cstore,
		MaxBytes:          cfg.Cache.MaxFileMB << 20,
		GitTTL:            cfg.Git.TTL.Duration,
		RawTTL:            cfg.Raw.TTL.Duration,
		Registry:          regAd,
		Policies:          policyResolver,
		TokenCacheMax:     cfg.Registry.TokenCacheMax,
		CacheMaxBytes:     cfg.Cache.MaxGB << 30,
		CacheMinFreeBytes: cfg.Cache.MinFreeGB << 30,
		RequireToken:      cfg.Auth.RequireToken,
		PrivateAuth:       privAuth,
		// 与探测共用放行策略：自建局域网源要能回源，否则面板允许探测、
		// 真正下载时又被拦下，等于配置无效。
		Guard: safehttp.New(safehttp.Options{MaxRedirects: 6, AllowPrivate: cfg.Probe.AllowPrivate, IPFamily: cfg.Network.IPFamily}),
		// 三个独立加速开关：总开关关闭时 regAd 为 nil，/v2/ 整体不挂。
		DockerOff:     !cfg.Registry.Docker,
		GHCROff:       !cfg.Registry.GHCR,
		DisabledHosts: cfg.Registry.DisabledHosts,
		GitHubOff:     !cfg.GitHub.Enabled,
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
			SwitchFloorMiBPerSec: cfg.Parallel.SwitchFloorMiBPerSec,
			SwitchMinFileMiB:     cfg.Parallel.SwitchMinFileMiB,
			SwitchMaxSentMiB:     config.NormalizeMaxSentMiB(cfg.Parallel.SwitchMaxSentMiB),
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
		Race: gateway.RaceOptions{
			After: time.Duration(cfg.Registry.RaceAfterMS) * time.Millisecond,
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
	// Pending 记录自愈：每分钟对账一次，不依赖人工点按钮或重启。
	if hostCreds != nil {
		go retryPendingCreds(ctx, hostCreds)
	}
	// 旧 epoch 物理清理：分批删索引，孤儿文件由常规回收处理。
	if cacheEpoch > 0 {
		go sweepOldEpoch(ctx, st, gw.GC(), func() int64 { return atomic.LoadInt64(&cacheEpoch) })
	}
	defer stop()
	go eng.Loop(ctx)

	if hostCreds != nil {
		go runner.NewHostProbeRunner(runner.HostProbeOptions{
			Store:        st,
			Creds:        hostCreds,
			Repos:        parseHostProbeRepos(os.Getenv("ML_HOSTPROBE_REPOS")),
			AllowedHosts: cfg.Registry.MultiOriginHosts,
			Interval:     hostProbeInterval(),
			ScanInterval: hostProbeScanInterval(),
			Timeout:      time.Duration(cfg.Probe.TimeoutSec) * time.Second,
			Concurrency:  cfg.Probe.Concurrency,
			RecoveryTTL:  st.RecoveryTTL(),
			Policies:     policyResolver,
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
		BackupCreds:   filepath.Join(cfg.Data, "registry_creds.enc"),
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
		api.WithConfig(cfg, path), api.WithAlerts(alerts), api.WithCredentials(hostCreds))

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

// metaPrivateHosts 记录上一次生效的私有 Host 集合，用于检测集合变化。
const metaPrivateHosts = "registry_private_hosts"

// retryPendingCreds 周期性对账：文件已写、DB 未提交的 Pending 记录会自愈。
func retryPendingCreds(ctx context.Context, cs *secret.Store) {
	retryPendingCredsEvery(ctx, cs, time.Minute)
}

type credReconciler interface {
	ReconcileAll() (int, []secret.Status, error)
}

// retryPendingCredsEvery 允许测试用短周期验证 ticker 确实在重试；
// 生产入口固定一分钟，不把测试节奏带进运行时。
func retryPendingCredsEvery(ctx context.Context, cs credReconciler, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, quarantined, err := cs.ReconcileAll()
			if err != nil {
				log.Printf("凭据后台对账失败: %v", err)
				continue
			}
			if n > 0 || len(quarantined) > 0 {
				log.Printf("凭据后台对账：变更 %d 条，隔离 %d 个 Host", n, len(quarantined))
			}
		}
	}
}

// sweepOldEpoch 分批清理早于当前 epoch 的 Registry 缓存索引和物理文件。
// 每批 500 行、批间 100ms，避免和正常回源抢 IO。
func sweepOldEpoch(ctx context.Context, st *store.Store, gc *gc.GC, epoch func() int64) {
	total := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		cur := epoch()
		keys, err := st.CacheEpochKeysBefore(cur, 500)
		if err != nil {
			log.Printf("旧 epoch 缓存清理失败（已删 %d 条）: %v", total, err)
			st.Audit(store.AuditEvent{
				Action: "cache_epoch_sweep",
				Target: "registry_cache_epoch",
				Detail: fmt.Sprintf("epoch=%d deleted=%d error=%v", cur, total, err),
				OK:     false,
			})
			// 1.6 B.3/D-15-12：清理失败必须继续重试，不能静默停掉。
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		if len(keys) == 0 {
			break
		}
		res, derr := gc.DeleteKeys(keys)
		if derr != nil {
			log.Printf("旧 epoch 缓存删除失败（已删 %d 条）: %v", total, derr)
			st.Audit(store.AuditEvent{
				Action: "cache_epoch_sweep",
				Target: "registry_cache_epoch",
				Detail: fmt.Sprintf("epoch=%d deleted=%d error=%v", cur, total, derr),
				OK:     false,
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		total += res.Entries
		if err := st.MetaSet(fmt.Sprintf("registry_cache_epoch_sweep_progress:%d", cur), strconv.Itoa(total)); err != nil {
			log.Printf("记录旧 epoch 清理进度失败: %v", err)
		}
		if res.Entries == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	st.Audit(store.AuditEvent{
		Action: "cache_epoch_sweep",
		Target: "registry_cache_epoch",
		Detail: fmt.Sprintf("epoch=%d deleted=%d complete", epoch(), total),
		OK:     true,
	})
	if total > 0 {
		log.Printf("旧 epoch 缓存索引已清理 %d 条（当前 epoch=%d）", total, epoch())
	}
}
