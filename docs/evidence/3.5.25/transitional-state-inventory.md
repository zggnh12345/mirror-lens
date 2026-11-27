# 过渡态普查（G0，基线 84b013d）

## ML_HOSTPROBE_* 三个环境变量

| 变量 | 入口 | 行为 | 默认值 |
|---|---|---|---|
| `ML_HOSTPROBE_SCAN` | main.go:130-140 hostProbeScanInterval() | 无效值日志 :137 | 30s（:139） |
| `ML_HOSTPROBE_INTERVAL` | main.go:142-152 hostProbeInterval() | 无效值日志 :149 | 6h（:151） |
| `ML_HOSTPROBE_REPOS` | main.go:154-168 parseHostProbeRepos（``host=repo,...``） | 解析至 :167 | 无 |

装配点 main.go:597-616：Repos 取 os.Getenv(:601)、AllowedHosts 取 cfg.Registry.MultiOriginHosts(:602)、Interval/ScanInterval 取上述两函数(:603-604)。

## Runner 内部第二套默认值

`internal/runner/hostprobe.go:110-116`：defaultHostProbeInterval=6h(:112)、defaultHostProbeScan=30s(:115)。`NewHostProbeRunner`(:118-147) 在 o.Interval<=0 用 :119-121、o.Timeout<=0 用 25s(:122-124)、Concurrency<=0 用 2(:125-127)、RecoveryTTL<=0 用 store.RecoveryDefaultTTL(:128-130)、o.ScanInterval<=0 用 :131-133。`HostProbeOptions` 定义 :36-50。缺映射时 jobs 跳过并打日志提示 ML_HOSTPROBE_REPOS（:318-320）；filterAllowed 按 AllowedHosts 过滤(:325-338)。

## 正式配置现状

`internal/config/config.go:356-365` Probe 只有 allow_private/interval_sec/concurrency/timeout_sec/sample_mib 五项，无 host_probe_* 字段；config.example.yaml 无 host_probe 项。

## 3.5.25 冻结迁移口径（任务书）

删除全部 ML_HOSTPROBE_* 声明/读取/解析/覆盖/测试、旧 YAML key alias 与空字段回落编译列表；新增 probe.host_probe_repos/host_probe_interval_sec(21600)/host_probe_scan_sec(30) 进 Probe 结构；两个默认值来自 3.5.24 上述函数与常量；已有配置缺字段 fail-fast，不从环境变量/编译默认静默补值；主 Runner 不得保留第二套生产配置来源；lscr.io 加入 multi_origin_hosts 需人工核对生产配置（不在自动授权内）。
