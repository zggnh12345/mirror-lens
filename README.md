<img src="docs/logo.svg" width="64" height="64" alt="Mirror Lens" align="left" hspace="12">

# Mirror Lens · 镜像加速与可信度管理

给 **Docker Hub** 和 **GitHub** 的第三方加速源做体检，然后把流量真的导过去。

一个容器、一个面板、一个 SQLite 文件，不碰你的任何配置。带登录，默认账号 `admin / admin`，登录后请立刻改密码。

两件事它都做：

- **测量**：哪些源还活着、谁快、实际下载到的字节和官方摘要对不对得上。
- **加速**：`/http/`、`/raw/`、`/git/`、`/v2/` 四个前缀直接代理，带本地缓存、断点续传和多源分片。

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

打开 `http://<你的IP>:8080`，用 `admin / admin` 登录。首次启动会写入 45 个内置候选，后台自动跑第一轮探测（约 3 分钟出完整结果）。

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

细节、验收结果和已知限制见 [docs/accelerator.md](docs/accelerator.md)。目前开了 `/raw/`、`/http/`、`/git/`、`/v2/`，缓存带定时巡检、引用计数对账和手动清理，回源带多源调度与内容校验状态（`verified` / `unverified`，校验不过不入缓存）；并行下载按阶段推进。

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
GET    /http/<目标地址>                    通用 HTTP 加速，支持 Range 直通与断点续传
                                          （原样前缀 / http(s)/host/path / 整体编码 三种写法都认）
GET    /git/<host>/<repo>.git/...          Git 克隆（info/refs）
POST   /git/<host>/<repo>.git/git-upload-pack  packfile 直通
POST   /api/password         {"old":"...","new":"..."}   只改密码
```

登录失败按 IP 限流（5 分钟 8 次）。会话令牌存在库里，重启容器不掉登录。

## 安全边界

- 不改 `daemon.json`，不改 git config，不写任何系统文件
- 除探测目标外不发起任何外部请求（无遥测、无统计上报）
- 容器以非 root 运行，`no-new-privileges`
- 密码只存 bcrypt 哈希，会话令牌是 32 字节随机数，cookie 为 `HttpOnly` + `SameSite=Lax`
- 登录是单账号，没有多用户和权限分级；用户名和密码都能改，忘了密码只能删 `settings` 表里的 `admin_password_hash` 重置回默认
- 主题只存在浏览器本地（localStorage），换设备要重选一次
- 探测目标是用户可控的 URL，和代理一样是 SSRF 面：探测现在走同一套安全拨号层，DNS 结果、每一跳重定向、Docker 的 token 地址、TLS 探测全部检查，回环/私网/链路本地/CGNAT 默认拒绝；源就在自己局域网时用 `probe.allow_private: true` 显式放开
- 代理路径默认匿名（`auth.require_token: false`），端口暴露到公网就等于开放下载代理，且没有按 IP 的限速和额度

## 已知限制

- **Docker 的自动发现是真的抓列表**：定期扫描维护 Docker 镜像站的 GitHub 仓库，新域名进待验证池，通过 L2 才转正。实测一轮能从 45 个内置源之外再挖出 6 个可用源。
- **GitHub 代理没有可靠的公开列表**，试过的几个 awesome 仓库要么 404 要么只列一两个。所以 GitHub 侧以内置池 + 手工添加为主，自动发现基本不产出。
- 探测点在哪，测出来的速度就代表哪。容器跑在代理后面时，数字不代表直连速度。
- 第三方源没有 SLA，任何"可用"都只描述此刻。

## License

MIT
