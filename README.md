<img src="docs/logo.svg" width="64" height="64" alt="Mirror Lens" align="left" hspace="12">

# Mirror Lens · 镜像加速与可信度管理

给 **Docker Hub** 和 **GitHub** 的第三方加速源做体检，然后把流量真的导过去。

一个容器、一个面板、一个 SQLite 文件，不碰你的任何配置。带登录，首次启动用 `admin / admin` 登录后会强制设置新账号和密码，改完之前面板其它接口一律拒绝。

两件事它都做：

- **测量**：哪些源还活着、谁快、实际下载到的字节和官方摘要对不对得上。
- **加速**：`/http/`、`/raw/`、`/gh/`、`/git/`、`/v2/` 五个前缀直接代理，带本地缓存、断点续传和多源分片。

> 默认按「只在自己网络里用」设计：不开 Token、只监听容器端口。要暴露到公网，先看[安全边界](#安全边界)。

## 为什么需要它

公开的加速源寿命以月计，而且**死法很安静**：

- 域名过期 → `NXDOMAIN`
- 换成落地页 → `/v2/` 返回 `200 text/html`，docker 拉取时才发现不对
- 被限流 → `429`，间歇性失败
- 证书过期 → 客户端直接报 TLS 错误
- 内容被换 → 最危险，HTTP 层面完全正常

大多数"镜像站列表"是几个月前抄的，早就不准了。这个项目不信任任何列表，只信任**真实拉下来的字节**。

## 探测怎么做

分三级，频率不同，成本不同：

| 级别 | 频率 | Docker | GitHub |
|---|---|---|---|
| L1 存活 | 3 分钟 | `GET /v2/` + 证书 | `GET raw 文件` |
| L2 可用 | 15 分钟 | 完整 token 流程 + 真 manifest | 校验 raw 文件 SHA-256 |
| L3 质量 | 6 小时 | 下载 layer 测速 + digest 比对 | 下载真实 release + SHA-256 + 测速 |

### 判定规则

**Docker 源必须走完整认证流程。** 只测 `/v2/` 会漏掉两类假货：返回 HTML 落地页的，和发得出 token 但 manifest 永远 401 的。只有拿到含 `schemaVersion` 的 JSON 才算可用。

**内容一致性用 digest 共识，不用证书。** 同一个镜像从官方 `registry-1.docker.io` 和各镜像站各取一次 manifest，比对 `Docker-Content-Digest`。官方拿不到时退化为多源众数（至少 2 票）。不一致的源标红为「内容校验失败」，不会出现在导出结果里。TLS 只能证明连接没被中间人截断，证明不了镜像站给你的是原始内容。

**GitHub 代理用固定样本校验。** 内置两个永不变化的文件（`octocat/Hello-World` 的 README、`jq-1.6` 的 linux 二进制），SHA-256 写死在代码里。哈希不符 = 内容被替换；下载中断 = 太慢，不算投毒，两者分开标记。

**连续 3 次 L2 失败才判死。** 单次抖动只降级，不删源。

## 快速开始

```bash
docker run -d --name mirror-lens \
  -p 8080:8080 \
  -v $(pwd)/data:/data \
  --restart unless-stopped \
  ghcr.io/zggnh12345/mirror-lens:latest
```

或者 compose：

```bash
curl -O https://raw.githubusercontent.com/zggnh12345/mirror-lens/main/docker-compose.yml
docker compose up -d
```

打开 `http://<你的IP>:8080`，用 `admin / admin` 登录，随即会要求设置新的用户名和密码（至少 8 位）。首次启动会写入 45 个内置候选，后台自动跑第一轮探测（约 3 分钟出完整结果）。

登录后点右上角齿轮进设置页：改用户名和密码、调探测频率、切白天/黑暗/跟随系统。密码用 bcrypt 存在 SQLite 里，改密码会踢掉其他设备上已登录的会话。

> 用 bind mount 挂 `./data` 时，容器内是 uid 10001，需要先 `sudo chown -R 10001:10001 ./data`，否则启动会直接报"数据目录不可写"。嫌麻烦就改用 named volume，权限自动继承。

从源码构建：

```bash
docker build -t mirror-lens .
```

## 面板

一个列表，Docker 和 GitHub 混在一起，每行用**打勾**直接标出它支持什么：

- `✓ Docker` 蓝色 / `✓ GitHub` 紫色 = 实测支持
- `— Docker` 灰色 = 实测不支持（探测过，明确不行）
- `? Docker` 灰色 = 还没测出来（新加入的源）

左边圆点是这个源**现在活不活**：绿=可用、黄=不稳定、红=失效/被拦、灰=待验证。能力和存活是两件事——一个站可以「支持 Docker，但今天挂了」。

- **＋ 添加自定义加速**：只填地址，保存后自动识别它支持 Docker Hub 还是 GitHub，两个都支持就两个都打勾，识别过程在弹窗里实时显示结果
- **立即检测**：跑一轮 L2（快）
- **自动发现**：扫描公开列表仓库，新域名进待验证池
- **导出配置**：只输出通过校验的源，按速度排序，Docker 给 `daemon.json` 片段，GitHub 给 `git insteadOf` 命令
- 点任意一行看最近 60 次探测的柱状图（柱高=延迟，颜色=结果）
- 右侧开关：停用的源不再探测，但保留历史

### 设置（右上角齿轮，独立页面）

不是弹窗，点进去是完整一页，分三块：

- **账号**：改用户名（3-32 位，字母数字 `_ - .`）、改密码（至少 8 位），都要先输当前密码；改完密码其他设备的登录立即失效
- **外观**：白天 / 黑暗 / 跟随系统，存本地，跟随系统时会跟着手机切换
- **探测节奏**：L1 / L2 / L3 各自的间隔、登录有效期、导出条数上限，保存后下一轮生效

## 加速器（v2.2 预览）

除了监测，现在还可以当加速代理用：在原始地址前加上本站前缀，不需要装客户端。

```
原始  https://raw.githubusercontent.com/zggnh12345/mirror-lens/main/README.md
加速  http://<你的地址>:8080/raw/zggnh12345/mirror-lens/main/README.md
```

任意 HTTP 也能走，原样在地址前加 `/http/` 即可：

```
原始  https://example.com/path/file.iso
加速  http://<你的地址>:8080/http/https://example.com/path/file.iso
```

反代会把 `//` 折叠成 `/`（Traefik 还会拒收 `%2F`），所以下面这几种写法都认，效果一样：

```
/http/https://example.com/path/file.iso     原样前缀，推荐
/http/https/example.com/path/file.iso       scheme/host/path
/http/https%3A%2F%2Fexample.com%2Fpath%2Ffile.iso   整体百分号编码
```

GitHub Release 资产和源码包走 `/gh/`——这一条会经过镜像源调度，也是它和 `/http/` 的区别：

```
原始  https://github.com/cli/cli/releases/download/v2.40.0/gh_2.40.0_linux_amd64.tar.gz
加速  http://<你的地址>:8080/gh/cli/cli/releases/download/v2.40.0/gh_2.40.0_linux_amd64.tar.gz

原始  https://github.com/octocat/Hello-World/archive/refs/tags/v1.0.tar.gz
加速  http://<你的地址>:8080/gh/octocat/Hello-World/archive/refs/tags/v1.0.tar.gz
```

`releases/latest/download/<文件>`、`archive/refs/heads/<分支>.zip`、`archive/<ref>.tar.gz` 也都认。
release 资产缓存 24 小时、分支快照 1 小时（分支会动），两者都支持 Range 与断点续传。

Git 仓库也能克隆，Token 放在用户名位：

```bash
# 默认匿名：直接 clone；开了 require_token 才把 Token 放用户名位
git clone http://<你的地址>:8080/git/github.com/jqlang/jq.git
```

只代理读（`info/refs` + `upload-pack`），push 直接 403；packfile 直通不缓存，`info/refs` 缓存 30 秒。

代理路径默认**匿名放行**，不用先建 Token（面板登录仍然要密码）。想收紧就在配置里打开：

```yaml
auth:
  require_token: true
```

打开后所有代理路径都要 Access Token，下面几段里的 `<token>` 才成为必填。

Docker 镜像走同一端口的 `/v2/`：

```bash
# 客户端声明明文源（HTTP + 裸 IP 时 docker 只认这个办法）
# /etc/docker/daemon.json:  { "insecure-registries": ["192.0.2.10:8080"] }

# 开了 auth.require_token 才需要这步；用户名随意，密码位填 Access Token（不是面板密码）
docker login 192.0.2.10:8080 -u x -p <token>
docker pull 192.0.2.10:8080/library/alpine:3.20
```

只读，一个实例对一个上游（默认 Docker Hub，要 ghcr 就再起一个实例）；layer 按 digest 缓存并边下边校验 SHA-256，tag 5 分钟重验、digest 永久。

首次请求边下边发并写入本地缓存，第二次直接读盘；分支内容过期后带 ETag 重新验证，304 就继续用旧的。传到一半断了不要紧，下次请求会从断点接着下。

```bash
# 设置页 → 加速器 → 新建 Token（明文只显示一次）
curl -H "Authorization: Bearer ml_xxx" \
  http://192.168.1.10:8080/raw/zggnh12345/mirror-lens/main/README.md
```

- Token 只存 SHA-256，吊销立即失效；`git` / `curl` 也能用 Basic（用户名随意，密码位填 Token；git 也可以把 Token 放在用户名位、密码留空）。
- 只允许出公网：内网、回环、链路本地、CGNAT 一律拒绝；重定向逐跳复查，防 SSRF 与 DNS Rebinding。
- 缓存按内容寻址（`sha256`），同一文件只存一份；单文件上限默认 256 MB，超了只透传不落盘。
- 断点续传用 `.part` + `If-Range`：上游内容变了就从头下，绝不把两个版本拼在一起。
- 客户端自带 `Range` 时直通上游，不先缓存整份再切片。
- 回源时在可用镜像站之间按探测评分挑一个，失败自动换下一个（`X-ML-Source` 标出这次是谁供的货）；TTL 到期重新验证一律回官方源。
- 代理流量单独记账（`proxy` / `cache_hit` / `cache_fill`），不参与源站评分。

细节、验收结果和已知限制见 [docs/accelerator.md](docs/accelerator.md)。目前开了 `/raw/`、`/gh/`、`/http/`、`/git/`、`/v2/`，缓存带定时巡检、引用计数对账和手动清理，回源带多源调度与内容校验状态（`verified` / `unverified`，校验不过不入缓存）；并行下载按阶段推进。

## 升级

v1 的库可以直接给 v2 用：启动时自动把 `(base_url, kind)` 两行结构合并成一行 + 两个能力标记，历史记录保留并补上类型。迁移在事务里做，失败就回滚不写库。

## 配置

参数写在 `config.yaml` 里，全部字段都有默认值，不写也能跑：

```yaml
addr: ":8080"
data: "/data"
cache:
  max_file_mb: 256
  max_gb: 100
raw:
  ttl: 5m
git:
  ttl: 30s
registry:
  enabled: true
  upstream: "registry-1.docker.io"
  max_file_mb: 4096
  tag_ttl: 5m
```

优先级：命令行（`-config` / `-addr` / `-data` / `-no-seed`）> 环境变量（`ADDR` / `DATA_DIR` / `CACHE_DIR`）> 配置文件 > 默认值。只写想改的字段即可，带注释的完整模板见 [config.example.yaml](config.example.yaml)。

容器里把配置放数据目录就能自动读到：`./data/config.yaml`（镜像的工作目录就是 `/data`）。

## API

除 `/login`、`/api/login`、`/healthz` 外都要带登录 cookie。

```
POST   /api/login            {"user":"admin","pass":"..."}   → Set-Cookie: ml_session
POST   /api/setup            {"user":"...","pass":"..."}     → 仅首次（默认密码状态）可用
POST   /api/logout
GET    /api/me               当前账号 + 是否还在用默认密码
GET    /healthz              公开，给容器健康检查用

GET    /api/sources
POST   /api/sources          {"url":"docker.1panel.live","name":"可选"}   自动识别支持类型
PATCH  /api/sources/{id}     {"enabled":false}
DELETE /api/sources/{id}
GET    /api/series/{id}      最近 60 个检测点
POST   /api/probe            {"tier":1|2|3}
POST   /api/discover
GET    /api/export
GET    /api/settings
PUT    /api/settings         {"l2_minutes":20,"export_limit":4,...}
POST   /api/account          {"old":"...","user":"新用户名","new":"新密码"}   两项都可选，需当前密码

GET    /api/tokens           列出 Token（只有前缀，没有明文）
POST   /api/tokens           {"name":"我的笔记本"}   → 返回明文，只此一次
DELETE /api/tokens/{id}      吊销
GET    /api/accel/stats      缓存条目数/占用、近 7 天流量账、最近条目

GET    /raw/<owner>/<repo>/<ref>/<path>    加速入口，Bearer Token 或 Basic 认证（不走 cookie）
GET    /gh/<owner>/<repo>/releases/...     GitHub Release 资产与源码包，走镜像源调度
GET    /http/<目标地址>                    通用 HTTP 加速，支持 Range 直通与断点续传
                                          （原样前缀 / http(s)/host/path / 整体编码 三种写法都认）
GET    /git/<host>/<repo>.git/...          Git 克隆（info/refs）
POST   /git/<host>/<repo>.git/git-upload-pack  packfile 直通
POST   /api/password         {"old":"...","new":"..."}   只改密码
```

登录失败按 IP 限流（5 分钟 8 次）。会话令牌存在库里，重启容器不掉登录。

## 限流

两层，都在 HTTP 入口，不碰调度器：

- 全局：20 请求/秒、突发 100、并发 200
- 匿名每 IP：300 次/分、突发 60、并发 8
- 带 Access Token：每 Token 1200 次/分、突发 200、并发 32

被限流回 `429`，带 `Retry-After`（按补充速度算，客户端照着重试就行）和 `X-ML-Limit`（`global` / `ip` / `token`，说明是哪一层拦的）。

几个刻意的选择：

- **大文件下载只算一个请求**。速率限制数的是客户端发起的请求次数；服务端拆出去的分片是内部行为，不会让一个 2 GB 下载变成几百次请求。但下载期间一直占着一个并发名额——否则「并发」只统计了开始的那一下。
- **`/healthz` 不受限**。容器自己的探活被 429，会显示成不健康，等于自己制造故障。
- **限流和并发繁忙不计入源站健康**。被限流是「让你慢点」，不是「源站坏了」。
- **默认不信任何转发头**。`X-Forwarded-For` 谁都能写，信了等于把限流键交给客户端。部署在 nginx / Traefik / Caddy 后面时，把反代自己的网段填进 `limits.trusted_proxies`（CIDR 或单个地址，如 `127.0.0.1/32`、`10.0.0.0/8`）；只有来自这些地址的请求才读转发头，并从右往左取第一个非可信地址——左边客户端可以随便伪造。
- 所有数字在 `limits.*` 下，`limits.enabled: false` 整层关闭。

## 源站生命周期

每个源有一个状态，探测结果自动推动它变化：

```
pending ──探测通过──▶ usable ⇄ degraded ──连续 3 次失败──▶ dead
   │                    │                                  │
   └────首次失败────────┴──────────┐                       │
                                   ▼                       ▼
                             blocked（摘要不符） ──14 天──▶ archived ──180 天──▶ 删除
```

- **pending**：刚添加或刚手动恢复，还没验证过，不参与调度。
- **usable / degraded**：可用 / 部分探测失败。只有这两个状态会接流量。
- **dead**：连续 3 次探测失败。仍会以 `sources.recovery_minutes`（默认 60 分钟）的间隔低频复探——不探测就永远恢复不了。
- **blocked**：内容摘要与官方不一致（缓存污染、被替换、被劫持）。这是安全状态，不是网络故障：要**连续两次** tier≥2 校验都干净才自动放回，避免「偶尔返回正确内容」的源在可用/封禁之间反复横跳。
- **archived**：dead/blocked 维持 `sources.archive_days`（默认 14 天）后自动归档，也可手动归档。归档源不再探测、不参与调度，但记录和检测历史都保留。
- **清理**：归档满 `sources.cleanup_days`（默认 180 天）后删除源及其检测记录；设为 `0` 或负数则永不自动删除。删除只针对 archived，其它状态再久也留着。

人工只能做三件事：**归档**、**封禁**、**恢复（退回 pending 复检）**。不能手动置 `usable`——那等于绕过探测和校验。面板里展开任意源即可操作，`PATCH /api/sources/{id}` 传 `{"status":"archived"|"blocked"|"pending"}` 也行。

### 一次请求里的换源规则

上游状态码决定换不换下一台源、以及要不要罚这台源：

- **404 / 410**：资源未命中。换下一台，但**不冷却、不扣健康分**——镜像少同步一个 Release 文件不代表它坏了。单独记在 `source_miss_total`。
- **429**：冷却当前源，换下一台；全部源都 429 时把最后一个 429 连同 `Retry-After` 原样返回。
- **5xx**：换下一台，并按失败记账、进冷却。
- **其他 4xx**（401/403/416 等）：直通路径直接返回；缓存和大文件路径会再试下一台源（镜像要鉴权、防盗链时换源往往还有救），都不行才把错误给客户端。
- **连不上 / 超时**：换下一台，按失败记账。
- **全部未命中**：返回 404。只要中间出现过超时、429 或 5xx，结果就是不确定的，按原来的错误返回，不会伪装成「资源不存在」。

缓存链路、大文件流式、`Range`/`HEAD` 直通三条路对 404/410、429、5xx 的处理一致。

状态变化时间（`status_at`）只在状态真的变化时刷新：否则一个常年 dead 的源每轮探测都会把「坏了多久」清零，永远到不了归档条件。老库升级时 `status_at` 从升级那一刻起算，不会把历史死源一次性归档。

## 动态带宽调度

固定并发是个折中：定 4 路，对 5 Mbps 的源是互相抢带宽（比单路还慢），对
1 Gbps 的源又喂不饱。并发该开几路由实测吞吐决定，不是拍脑袋。

`sources.dynamic: true`（默认）时，每台源的并发上限在 `1..max_per_source`
之间自动调整：

- **排队了、吞吐没退步** → 加一路。说明还有余量。
- **吞吐比滑动平均掉了 40%** → 退一路。说明已经在互相踩。
- **失败 / 429 / 503** → 上限减半，先把压力撤掉，别把源打死。

只在有实测样本时动手：没有字节数或耗时的上报（预检、304）不改上限，避免
凭空猜出来的并发比固定值更糟。样本来源是单次回源的整包耗时和每个分片的
实际字节数——后者最准。

调大 `max_per_source` 才有意义：默认 4 意味着最多也就 4 路。源站扛得住时
可以设 8，动态控制会自己找到合适的位置；源站一有 429 就会退回去。

观测：`mirrorlens_source_concurrency_limit{source_id="12"}` 是当前上限，
和 `mirrorlens_source_inflight` 一起看——上限贴着占用说明在扩容，上限掉到 1
说明这台源最近在挨打。

关掉动态（`dynamic: false`）就是老行为：每台源固定 `max_per_source` 路。

## 监控

`/metrics` 输出 Prometheus 文本格式，`metrics.enabled: false` 可关。默认公开；公网部署可把 `metrics.require_token` 打开，抓取端用 `Authorization: Bearer <Access Token>`。

指标（标签都是封闭集合，**没有 URL、IP、文件名**）：

- `mirrorlens_http_requests_total{scene,method,code}` — 请求数
- `mirrorlens_http_request_duration_seconds{scene}` — 请求耗时直方图
- `mirrorlens_http_response_bytes_total{scene}` — 下载流量（真正写给客户端的字节）
- `mirrorlens_cache_requests_total{scene,result}` — 缓存查询结果（`hit` / `miss` / `pass`）
- `mirrorlens_download_throughput_bytes_per_second{scene}` — 端到端吞吐（只统计未命中缓存的响应；命中时那是磁盘速度）
- `mirrorlens_source_requests_total{kind,result}` — 源站成功/失败
- `mirrorlens_source_http_responses_total{kind}` — 真正拿到 HTTP 响应的回源次数（429 率的分母；连不上、超时、DNS 失败都不计）。`Range`/`HEAD` 的直通请求也计入
- `mirrorlens_source_429_total{kind}` — 上游 429 次数
- `mirrorlens_source_miss_total{kind,code}` — 源上没有这个资源的次数（`code` 只有 404/410）。未命中不算源站故障：不进退避、不拉低健康分，但照样计入 429 率的分母
- `mirrorlens_source_fetch_duration_seconds{kind,mode}` — 回源**完整**耗时：`single` 整次回源、`stream` 首字节+传输、`parallel` 整次并行填充、`chunk` 单段
- `mirrorlens_source_ttfb_seconds{kind,mode}` — 首字节耗时：`stream` 从发出请求到收到响应头
- `mirrorlens_source_inflight{source_id}` — 源站此刻并发占用
- `mirrorlens_ratelimit_rejected_total{layer}` — 入口限流拒绝（`global` / `ip` / `token`）

常用查询：

```promql
# 缓存命中率
sum(rate(mirrorlens_cache_requests_total{result="hit"}[5m])) by (scene)
  / sum(rate(mirrorlens_cache_requests_total{result=~"hit|miss"}[5m])) by (scene)

# 源站成功率
sum(rate(mirrorlens_source_requests_total{result="ok"}[5m])) by (kind)
  / sum(rate(mirrorlens_source_requests_total[5m])) by (kind)

# 未命中占比（镜像池里有多少请求其实没有这个文件）
sum(rate(mirrorlens_source_miss_total[5m])) by (kind)
  / sum(rate(mirrorlens_source_http_responses_total[5m])) by (kind)

# 上游 429 占比（分母只算真实 HTTP 响应：连不上/超时没有状态码，算进去会把 429 率稀释掉；
# 直通的 Range/HEAD 请求同样计入分子分母，429 也会让该源进冷却）
sum(rate(mirrorlens_source_429_total[5m])) by (kind)
  / sum(rate(mirrorlens_source_http_responses_total[5m])) by (kind)

# P95 请求耗时
histogram_quantile(0.95, sum(rate(mirrorlens_http_request_duration_seconds_bucket[5m])) by (le, scene))

# 出站流量（字节/秒）
sum(rate(mirrorlens_http_response_bytes_total[5m]))
```

抓取配置：

```yaml
scrape_configs:
  - job_name: mirror-lens
    static_configs:
      - targets: ["mirror-lens:8080"]
```

## CDN 边缘缓存

网关本身已经能把「这份内容能缓存多久」说清楚，前面挂一层 CDN（Cloudflare、
Fastly、Varnish、nginx proxy_cache 都行）就能让重复下载不出你的机器。

打开开关：

```yaml
cdn:
  enabled: true
  max_age: 300      # 边缘 TTL 上限，秒；0 = 沿用条目 TTL
```

响应头按内容类型分三档，不需要你手工配：

- **摘要寻址**（`/v2/.../blobs/sha256:...`）：`Cache-Control: public, max-age=31536000, immutable`。
  字节由哈希决定，缓存一年也不会错。
- **标签寻址**（manifest、release 资产、raw 文件）：跟随条目 TTL，且不超过
  `cdn.max_age`，并带 `stale-while-revalidate=60`、`stale-if-error=86400`。
  manifest 额外带 `Vary: Accept`——manifest list 和单架构镜像必须是两份缓存。
- **引用类**（git `info/refs`）：`no-cache`，每次都要回源问。

缓存命中时带 `Age`（这份内容在本地放了多久），边缘据此扣减自己的 TTL。
开了 `cdn.enabled` 还会发 `Cache-Tag` / `Surrogate-Key`，值是 `ml-<类型>`
（`ml-docker`、`ml-gh`、`ml-raw`、`ml-http`），清理时按类型整类清即可。
错误响应一律 `no-store`，一次源站故障不会被边缘放大成所有人的故障。

### Cloudflare 最小配置

1. DNS 记录指向你的机器，橙色云朵打开。
2. Cache Rules 加一条：主机名匹配你的域名，路径匹配 `/v2/*`、`/gh/*`、`/raw/*`、
   `/http/*`，动作 **Cache Everything**，Edge TTL 选 *Use origin cache control*。
3. 再加一条更高优先级的 Bypass：路径 `/api/*`、`/metrics`、`/panel*`。
   （面板和接口带会话，缓存了会串号。）
4. 清理：`Cache-Tag: ml-docker` 支持按标签清理（企业版）；免费版用
   *Purge by URL* 或整站清理。
5. 限流：把 Cloudflare 的回源网段填进 `limits.trusted_proxies`，否则所有请求
   看起来都来自同一个 IP，客户端一层限流会把整个 CDN 当成一个用户。

### 注意

- CDN 免费版对 `Range` 请求通常整份回源再切片，首次下载不会更快；收益在
  第二次及之后。
- 开了 CDN 就别把 `auth.require_token` 设成 true：边缘不会带你的 Token。
  需要鉴权的内容不要放进 CDN 路径。
- `max_age` 别设太大。Docker blob 不受它影响（永远 immutable），受影响的
  是 release 资产——同名覆盖发新版时，边缘最多还会旧 `max_age` 秒。

## 安全边界

- 不改 `daemon.json`，不改 git config，不写任何系统文件
- 除探测目标外不发起任何外部请求（无遥测、无统计上报）
- 容器以非 root 运行，`no-new-privileges`
- 密码只存 bcrypt 哈希，会话令牌是 32 字节随机数，cookie 为 `HttpOnly` + `SameSite=Lax`
- 登录是单账号，没有多用户和权限分级；用户名和密码都能改，忘了密码只能删 `settings` 表里的 `admin_password_hash` 重置回默认
- 主题只存在浏览器本地（localStorage），换设备要重选一次
- 探测目标是用户可控的 URL，和代理一样是 SSRF 面：探测现在走同一套安全拨号层，DNS 结果、每一跳重定向、Docker 的 token 地址、TLS 探测全部检查，回环/私网/链路本地/CGNAT 默认拒绝；源就在自己局域网时用 `probe.allow_private: true` 显式放开
- 代理路径默认匿名（`auth.require_token: false`），端口暴露到公网就等于开放下载代理；有入口限流（见下）但那是防刷量，不是认证

## 已知限制

- **Docker 的自动发现是真的抓列表**：定期扫描维护 Docker 镜像站的 GitHub 仓库，新域名进待验证池，通过 L2 才转正。实测一轮能从 45 个内置源之外再挖出 6 个可用源。
- **GitHub 代理没有可靠的公开列表**，试过的几个 awesome 仓库要么 404 要么只列一两个。所以 GitHub 侧以内置池 + 手工添加为主，自动发现基本不产出。
- 探测点在哪，测出来的速度就代表哪。容器跑在代理后面时，数字不代表直连速度。
- 第三方源没有 SLA，任何"可用"都只描述此刻。

## License

MIT
