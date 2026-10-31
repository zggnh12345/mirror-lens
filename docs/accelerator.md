# 加速器（阶段 0 – 5）

在原始地址前加本站前缀即可加速，客户端不需要装任何东西。

```
GitHub Raw   https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path>
          →  http://<你的地址>/raw/<owner>/<repo>/<ref>/<path>

任意 HTTP    https://example.com/path/file.iso
          →  http://<你的地址>/http/https://example.com/path/file.iso

Git 仓库     https://github.com/<owner>/<repo>.git
          →  http://<你的地址>/git/github.com/<owner>/<repo>.git

Docker 镜像  registry-1.docker.io/library/alpine:3.20
          →  docker pull <你的地址>:8080/library/alpine:3.20
```

`/http/` 后面的地址有三种写法，解析结果完全一样：

| 写法 | 例子 | 说明 |
|---|---|---|
| 原样前缀 | `/http/https://example.com/a.iso` | 最好用；反代折叠 `//` 后也能认 |
| scheme/host/path | `/http/https/example.com/a.iso` | 任何反代都不会改动它 |
| 整体编码 | `/http/https%3A%2F%2Fexample.com%2Fa.iso` | 直连端口时可用 |

为什么这么宽松：Traefik 之类的反代会把路径里的 `//` 折叠成 `/`（`https://` → `https:/`），
同时默认拒收含 `%2F` 的路径（直接回 400）。两种规范写法都会被反代破坏，所以三种形态都接受。
自己反代时如果想让「整体编码」也生效，需要在入口上打开
`http.encodedCharacters.allowEncodedSlash=true` 并关闭 `http.sanitizePath`。

## 分层

```
internal/gateway             统一入口：认证 → 适配器 → 缓存 → 回源
internal/gateway/stream.go   流式填充与断点续传
internal/adapter             协议适配：看懂请求，产出执行计划
internal/adapter/raw         GitHub Raw
internal/adapter/httpadapter 通用 HTTP/HTTPS
internal/adapter/registryadapter Docker Registry V2（只读）
internal/cache               内容寻址存储（blobs/ab/<sha256>）+ 续传点（partial/）
internal/safehttp            只许出公网的 http.Client（拨号层把关）
internal/config              配置文件（yaml），带默认值
internal/store/accel.go      Token、缓存索引、流量账（同一 SQLite）
```

老包（store / auth / api / discover / runner / probe）没有搬动，监测功能照旧跑。
代理流量只写 `traffic_stats`，不进 `checks`，所以源站评分不会因为 NAS 正在拉大文件而集体变差。

## 一次请求发生了什么

1. `Authorization: Bearer` + 你的 Token，或 Basic 认证（用户名随意，密码位填 Token）；浏览器带登录 cookie 也行。
2. 适配器把路径翻译成上游 URL，并给出缓存键、TTL、是否可变、能否续传。
3. 命中且新鲜 → 直接从磁盘返回，`X-ML-Cache: HIT`。
4. 命中但过期 → 带 `If-None-Match` / `If-Modified-Since` 回源；304 继续用旧的（`HIT-REVAL`）。
5. 没命中 → **边下边发**：字节同时进客户端、磁盘临时文件和 SHA-256 计算器，不再等整个文件落盘（`MISS`）。
6. 同一个缓存键的并发请求串行填充，后来的等前一个填完直接读盘，不会两份字节抢同一个文件。

## 断点续传

中途断开（客户端 Ctrl-C、网络抖动、上游 RST）时，已下的部分留在 `cache/partial/`，旁边一个 `.json` 记着：地址、ETag、Last-Modified、总长、已下多少。

下次请求带 `Range: bytes=<已下>-` 和 `If-Range: <ETag>`：

- 上游回 206 且起点、总长都对得上 → 先把磁盘上那半截补给客户端，再接着下（`X-ML-Cache: RESUME`），完成后整份算 SHA-256 落盘。
- 上游回 200（内容变了，或者服务器不支持 Range）→ 丢掉半截，从头下。
- 上游回 416，或 `Content-Range` 对不上 → 同上，从头下。
- 没有 ETag 也没有 Last-Modified 的响应不留续传点——没有依据判断新旧，拼起来就是错的数据。

实测：100 MB 文件下到 3.98 MB 掐断，重新请求从 3,981,056 字节继续，最终 SHA-256 与直连下载一致。

## Git 克隆

```bash
# Token 放在用户名位（git 客户端的习惯写法，密码留空）
git clone http://<token>@<你的地址>:8080/git/github.com/jqlang/jq.git

# 也可以走 Basic 的用户名/密码形式，密码位填 Token
git clone http://x:<token>@<你的地址>:8080/git/github.com/zggnh12345/mirror-lens.git
```

- 默认 https 回源；要代理明文 HTTP 的私有仓库写成 `/git/http/<host>/<repo>.git`。
- 只做读：`info/refs` + `upload-pack`。`push`（`receive-pack`）直接 403，镜像站不替别人保管写权限。
- `info/refs` 缓存 30 秒（`-git-ttl`，0 关闭），缓存键里带 `Git-Protocol` 版本——v1 是引用列表、v2 是能力声明，两者字节完全不同，混用会让客户端解析失败。
- packfile 直通不缓存。它取决于客户端与服务端的协商（`have`/`want` 集合），同一仓库不同客户端拿到的包不一样，缓存命中率接近零；按 commit 缓存需要自己实现协商，第一版不做。
- 客户端会把 `fetch` 请求体 gzip 后发，`Content-Encoding` 必须一起转发，否则上游按明文解析回 400。
- dumb HTTP（`/objects/...`）拒绝，避免变成任意文件读取口。
- 子模块的 URL 写在 `.gitmodules` 里，是绝对地址，不会自动走代理。

实测：`git/git` 全量 clone（384 MB 工作区）成功，HEAD 与直连一致，代理进程峰值内存 **23.5 MB**——packfile 是边收边发的，没有整份缓冲。

## Docker 镜像

```bash
# 客户端声明这是明文源（HTTP + 裸 IP 时 docker 只认这个办法）
# /etc/docker/daemon.json:  { "insecure-registries": ["192.0.2.10:8080"] }

docker login 192.0.2.10:8080 -u x -p <token>
docker pull 192.0.2.10:8080/library/alpine:3.20
```

- 没有域名就没有证书，docker 对裸 IP 一律按 HTTPS 连，绕不过去，只能在客户端写一行 `insecure-registries`。想用域名 + 真证书，自己在前面套 nginx / caddy 即可——服务不看 `Host` 头，怎么反代都不影响。
- **一个实例对一个上游**（`registry.upstream`，默认 Docker Hub）。要 ghcr 就再起一个实例改参数：docker 客户端只会拼 `/v2/<name>/...`，路径里没有位置标"我要哪个 registry"，硬塞进去就成了不合规范的写法。
- **只读**：`GET` / `HEAD` 之外的写操作（push / delete / upload）一律 405。
- **Blob 按 digest 缓存**：键就是 `docker:blob:sha256:...`。边下边算 SHA-256，落盘前和 digest 比对，对不上就丢弃、不入缓存。digest 本身是内容哈希，命中即永久有效，不需要 TTL 也不需要 ETag。
- **Manifest 的键带 `Accept`**：同一个 tag，客户端要的是多架构索引还是单平台清单，返回字节不同，不分开存就会串味。tag 5 分钟重验（`registry.tag_ttl`），digest 永久。
- **上游认证**走标准 Bearer 挑战（`WWW-Authenticate` → 拿 realm 换 token → 重发），Hub / ghcr / quay 是同一套流程，换上游只改配置。客户端带来的 `Authorization` 不转发给上游。

实测：用 `crane`（不经过 docker daemon，`--insecure`）经代理拉 `alpine:3.20`，索引、平台清单、两个 layer 的 SHA-256 与直连逐字节一致；`redis:7-alpine` 首次 84 秒，二次 0.11 秒全命中，产物哈希相同。

## 配置

参数都在 `config.yaml`，命令行只留四个开关（`-config` / `-addr` / `-data` / `-no-seed`）：

```yaml
addr: ":8080"
data: "/data"
cache:
  max_file_mb: 256    # 普通文件超过就只透传
  max_gb: 100         # 总量上限，超了按"最久没被访问"淘汰；0 = 不限
  warn_at: 0.9        # 用量到这个比例报一次警
  recover_at: 0.85    # 回落到这以下才允许再报
janitor:
  interval: 10m
  partial_ttl: 24h    # 断了没续传的半截文件留多久
  expired_grace: 72h  # 过期后还要多久没人访问才删
raw:
  ttl: 5m
git:
  ttl: 30s
registry:
  enabled: true
  upstream: "registry-1.docker.io"
  max_file_mb: 4096   # layer 动辄上 GB，和普通文件分开算
  tag_ttl: 5m
```

优先级：命令行 > 环境变量（`ADDR` / `DATA_DIR` / `CACHE_DIR`）> 配置文件 > 内置默认。只写想改的字段，其余保持默认；文件不存在就用默认值，但显式 `-config` 指了个不存在的路径会报错（打错文件名却静默跑默认值最难查）。带注释的完整模板见 `config.example.yaml`。

## 缓存策略

| 前缀 | 键 | 版本处理 |
|---|---|---|
| `/raw/` | `raw:<owner>/<repo>/<ref>/<path>` | 40 位 commit 不可变；分支/标签 5 分钟后重验 |
| `/http/` | `http:<规范化 URL>` | 按响应头 `Cache-Control` / `Expires`；默认 5 分钟 |
| `/git/` | `git:<scheme>://<host>/<repo>` + `Git-Protocol` | 只缓存 `info/refs`，默认 30 秒；packfile 不缓存 |
| `/v2/` manifest | `docker:manifest:<upstream>/<name>:<ref>` + `Accept` | tag 5 分钟；digest 永久 |
| `/v2/` blob | `docker:blob:sha256:...` | 内容哈希，永久 |

`/http/` 的规则：

- `no-store`、`private` → 不落盘，标 `BYPASS`，每次回源。
- `no-cache` → 存，但每次都要回源验证（`HIT-REVAL`）。
- `max-age=N` / `Expires` → 按这个时间。
- 都没有 → 5 分钟。
- 文件比 `-max-file-mb` 大 → 不落盘，纯透传（`BYPASS`），但客户端照样拿到完整内容。

磁盘按内容寻址：`cache/blobs/<前两位>/<sha256>`，同一个文件只存一份。
索引里保留上游 ETag，仅用于同源条件请求——跨源不能拿 ETag 当一致性依据。

## Range 请求怎么处理

| 场景 | 行为 |
|---|---|
| 客户端带 `Range`，缓存里有 | `ServeContent` 处理，206 / 416 正确 |
| 客户端带 `Range`，缓存里没有 | 原样转发给上游，`X-ML-Cache: PASS`，**不落盘** |
| 客户端 `HEAD` | 直通，不落盘 |

带 Range 的请求本来就不需要完整文件，先缓存再切片只会白占盘。下载器自己的续传因此是端到端直通的，不会被代理拦成整份下载。

## 缓存巡检与清理（阶段 5）

后台每 `janitor.interval` 跑一轮，顺序固定：临时文件 → 半截文件 → 过期条目 → 索引对账 → 孤儿文件 → 补删 → 容量淘汰 → 水位判断。

| 项目 | 判据 | 动作 |
|---|---|---|
| `cache/tmp/*` | 超过 1 小时 | 删（写盘中断留下的） |
| `*.part` + `*.json` | 超过 `partial_ttl`，且拿不到文件锁 | 成对删 |
| 索引条目 | 过期超过 `expired_grace` 且期间没被访问 | 删索引，再按引用计数删文件 |
| 索引在、文件不在 | 每次都查 | 删索引（留着会让用户命中 502） |
| 文件在、没有索引引用 | 每次都查 | 删文件 |
| 用量 > `max_gb` | 按 `last_hit` 最旧优先 | 逐条淘汰，每删一条重新量 |

几条硬规则：

- **正在写的半截文件不碰。** `OpenPartial` 打开 `.part` 时加排他 `flock`，巡检拿不到锁就跳过。多进程部署时这把锁同样有效，不依赖 Go 内存里的 map。`.part` 与 `.json` 成对删，不会留半个。
- **共用内容不会被误删。** 删除前按 `cache_entries` 里还有几个键引用同一个 sha 来判断，不信任存下来的计数值。对账时以实际索引关系为准。
- **只扫自己的目录。** `cache/blobs` 下的遍历不跟随符号链接，跑不出 `cache/` 之外。
- **删除只有一条路。** 容量淘汰、过期清理、对账、手动清空全走 `internal/gc.DeleteKeys`：先删索引（事务），再按引用计数删文件；文件删不掉就进 `pending_blobs`，下一轮重试，不会出现"库里没了、盘上还在"。
- **告警不刷屏。** 超过 `warn_at` 报一次，回落到 `recover_at` 以下才重新武装；状态存在 `meta` 表，重启后不会重报。水位按磁盘实际占用算，不是索引里 `size` 之和。

### 手动清理

`POST /api/accel/purge`，body `{"scope":"all"}` 或 `raw` / `http` / `git` / `docker`：

```bash
curl -b cookie.txt -X POST http://nas:8080/api/accel/purge \
  -H 'Content-Type: application/json' -d '{"scope":"docker"}'
# {"ok":true,"scope":"docker","entries":12,"blobs":9,"bytes":734003200,"pending":0}
```

只动 `cache_entries` 和 `blobs`：Token、登录会话、探测历史、源站配置、流量账都不受影响。面板「加速器」卡片上有对应的三个按钮，执行完会显示删了多少条、释放多少。

### 流量统计口径

- 命中率 = `cache_hit` 字节 / (`cache_hit` + `cache_fill`) 字节。
- `BYPASS` 单独一列，不进分母——大文件透传多不代表缓存没价值。
- `cache_hit` 记的是**实际发给客户端的字节**（Range 只要 5 MB 就记 5 MB），`cache_fill` 记的是**上游拉下来的字节**。两者不是一回事，不要互相替代。
- 每次请求按 `kind` 分开记：`raw` / `http` / `git` / `docker`；阶段 4 以前的历史记录没有 kind，单独显示为「未知」，不并进任何协议。

## 安全

- 只允许 http/https，禁止 URL 里带 `user:pass@`，禁止代理自己（防环）。
- 拨号层检查真实 IP：回环、内网、链路本地、CGNAT、保留段一律拒绝，403。
  检查发生在 DNS 解析之后，先解析成公网再重解析成内网的 DNS Rebinding 也拦得住。
- 每一跳重定向都重新校验，最多 5 跳。
- 客户端带来的 `Authorization` 不会转发给上游，只发适配器自己构造的头。
- Token 只存 SHA-256，明文只在创建时返回一次；吊销立即失效。
- 上游 `Content-Length` 与实际字节数不符 → 判定坏响应，丢弃缓存，不落盘。
- 单文件上限按协议分：普通文件 256 MB，Docker layer 4 GB，超过就只透传。
- 缓存总量默认 100 GB（`cache.max_gb`），超了在后台按"最久没被访问"淘汰；共用同一份内容的多个键，删到最后一个才删文件。

## 验收

阶段 0 / 1（GitHub Raw）：

| 项 | 结果 |
|---|---|
| 管理员可创建 / 吊销 Token | ✅ `/api/tokens`，设置页有入口 |
| curl 用 Bearer 下载成功 | ✅ 与直连上游 SHA-256 一致 |
| 未认证被拒 | ✅ 401 + `WWW-Authenticate` |
| 禁私网 / 回环 / 链路本地 | ✅ 单元测试覆盖 15 个地址 |
| 重定向重新校验 | ✅ `CheckRedirect` 逐跳调用同一个检查 |
| 首次回源 / 完整性写缓存 / 二次命中 | ✅ MISS → HIT，上游只被请求一次 |
| 上游更新后重验 | ✅ 真实 GitHub 304 → `HIT-REVAL`；内容变化 → 换新 |
| 并发不产生不完整缓存 | ✅ 8 并发只回源 1 次 |
| 监测与迁移不受影响 | ✅ 45 个源照常，旧库自动补列 |

阶段 2（通用 HTTP + Range）：

| 项 | 结果 |
|---|---|
| `/http/` 编码地址下载 | ✅ jq 二进制 2,319,424 字节，SHA-256 与直连一致 |
| 边下边发 | ✅ 首字节不再等整份落盘，100 MB 文件边下边写 |
| 二次命中 | ✅ 100 MB 命中 54 ms，不回源 |
| 客户端 Range 直通 | ✅ 206 + `Content-Range`，标 `PASS`，不落盘 |
| 断点续传 | ✅ 3.98 MB 处断开，续传后 SHA-256 与直连一致 |
| 上游忽略 Range | ✅ 从头重下，内容正确（单元测试） |
| 304 重验 | ✅ `max-age=0` 时第二次回源拿到 304 |
| no-store 不缓存 | ✅ 两次请求都回源，标 `BYPASS` |
| 超上限降级透传 | ✅ 内容完整，不写缓存 |
| 并发不重复回源 | ✅ 6 并发只回源 1 次 |
| SSRF / 非 http(s) | ✅ 403 / 400 |

阶段 3（Git）：

| 项 | 结果 |
|---|---|
| `git clone` 走代理 | ✅ jq 仓库，HEAD 与直连一致 |
| 大仓库 | ✅ `git/git` 384 MB 工作区，HEAD 一致，代理峰值内存 23.5 MB |
| 浅克隆 / 增量 fetch | ✅ `--depth 1` 与后续 `git fetch` 均正常 |
| `info/refs` 二次命中 | ✅ `X-ML-Cache: HIT`，上游只被请求一次 |
| v1 / v2 不串味 | ✅ 缓存键含 `Git-Protocol`，v2 请求不复用 v1 字节 |
| packfile 不落盘 | ✅ 全量 clone 后缓存目录只有 1 个 20 KB 的 info/refs |
| gzip 请求体透传 | ✅ 转发 `Content-Encoding` 后 fetch 正常（否则上游 400） |
| push 被拒 | ✅ 403「只支持拉取，不支持 push」 |
| dumb HTTP / 路径穿越 | ✅ 400 |
| 未认证 | ✅ 401 + Basic 挑战，git 会提示输入 |
| 内网地址 | ✅ 403（SSRF 拨号层拦截） |

阶段 4（Docker Registry）：

| 项 | 结果 |
|---|---|
| 未认证 | ✅ 401 + Basic 挑战（docker login 会提示） |
| `/v2/` 探测 | ✅ 200 + `Docker-Distribution-API-Version: registry/2.0` |
| 索引字节 | ✅ SHA-256 与直连一致 |
| 多架构不串味 | ✅ 索引与平台清单各自缓存，分别与直连一致 |
| layer 逐字节 | ✅ 两个 layer 的 SHA-256 均与直连一致 |
| 二次命中 | ✅ `redis:7-alpine` 84 秒 → 0.11 秒，产物哈希相同 |
| digest 校验 | ✅ 上游字节与 digest 不符 → 不入缓存（单测） |
| push / delete | ✅ 405 |
| Bearer 换权 | ✅ 401 → 换 token → 200，token 按 scope 复用 |
| 容器部署 | ✅ 挂配置起容器，`/v2/` 正常，监测接口不受影响 |

## 多源调度（阶段 6）

开启 `sources.enabled` 后，回源不再固定打官方地址，而是在可用镜像站里按探测
评分挑一个，失败自动换下一个。

| 规则 | 行为 |
|---|---|
| 评分样本 | 只用探测记录（`checks.tier >= 2`）最近 10 条的中位数：成功率 50%、延迟 25%、速度 25% |
| 代理流量 | **不进评分**。代理请求的快慢是「用户到本机」，和「本机到源站」不是一回事 |
| 失败熔断 | 5xx / 429 / 403 / 401 / 连不上 → 记一次失败、该源降权 5 分钟，同一请求换下一个源 |
| 404 / 410 | 不换源：资源本身不存在，换谁都没有 |
| 粘性 | 成功一次后 10 分钟内优先复用这台源 |
| 重新验证 | TTL 到期后**只回官方源**。镜像站可能把旧内容当成「没变」，让它参与验证等于把过期数据固化 |
| 续传 | 已有 `.part` 时只认官方源：`If-Range`/`ETag` 绑定了原来那台服务器 |
| 缓存身份 | 与源无关，`CacheKey` 只由原始 URL 决定；换源不会产生第二份缓存 |
| 响应头 | `X-ML-Source` 标出这次是谁供的货（仅 MISS） |

源画像来自 v2 面板原有的探测池：只取 `enabled=1` 且状态为 `usable/degraded`
的源，`dead` 与 `pending` 不参与。每类协议单独排序（GitHub 前缀代理 / Docker
镜像站），互不串用。

**没有做的**：官方双取比对。见下一节的三级验证模型。

## 内容校验与调度补强（阶段 6.1）

### 三级验证：能证明什么

| 手段 | 能证明 | 不能证明 |
|---|---|---|
| Content-Length | 长度符合预期 | 内容正确。同长度的替换内容照样通过 |
| ETag / Last-Modified | 上游认为「版本没变」 | 内容哈希。ETag 可以是任意字符串 |
| SHA-256 / digest | 与**可信摘要**一致 | 摘要本身是否可信——取决于谁给的 |

结论：**算了 SHA-256 不等于验证过**。算只是得到指纹，必须与可信摘要比对才有意义。
所以缓存记录上有一个 `verify` 字段：

- `verified` — 有客户端给的摘要做参照，落盘前逐字节比对过。目前只有 Docker 按 digest
  拉 blob / 清单时满足（摘要写在 URL 里，与供货方无关）。
- `unverified` — 只确认收全了，没有可信摘要可比。raw / release / http、按 tag 拉的
  清单、git 全在这类。
- `failed` — 校验不过。**不会产生缓存记录**：坏内容一字节都不写索引，所以这个状态
  只体现在 `cache.verify_failed` 计数和日志里。

上游响应头自带摘要（`Content-Digest`、`Digest: sha-256=`、`X-Checksum-Sha256`、
`x-amz-checksum-sha256`）时会比对，不一致就拒收——但那只能挡住传输损坏，挡不住
「同一个镜像返回另一份内容并附上它自己的正确摘要」，所以它**不会**把内容升级成
`verified`。

面板「内容校验」一行显示已验证/未验证条目数与拒收次数。

### 评分归一化

三个指标都先压到 0..1 再加权：成功率 50%、延迟 `1/(1+ms/800)` 25%、速度
`min(KBps/1500, 1)` 25%。

- 成功率做贝叶斯平滑（先验 50%，权重 2）：1 次成功算 `(1+1)/(1+2)=0.67`，不会凭
  一次探测冲到第一。
- 没有速度样本时给中性 0.5，而不是 0：缺样本不等于慢。
- 连续失败不是慢慢掉分，而是直接排除（见熔断）。
- 分数相同时按源 ID 排序，顺序稳定，不会每次请求换一台。

### 协议能力

源站能力按协议分开记：`github_cap`（raw/release）、`docker_cap`、`git_cap`。
`git_cap` 默认 0——实测三个 GitHub 前缀镜像都不支持 git smart HTTP，默认去试只是
白等一次超时。某个源真的代理成功过 git，调度器会自动把 `git_cap` 置 1。

### 熔断：三态

```
CLOSED ──失败──▶ OPEN（冷却 5m，期间直接排除）
                   │ 冷却到期
                   ▼
              HALF_OPEN（只放一个探测请求）
                   ├─ 成功 → CLOSED
                   └─ 失败 → OPEN，冷却翻倍（5→10→20→40m 封顶）
```

- 半开探测排在队尾：有健康源时不必拿真实请求去赌。
- 冷却过期整整一个周期还没被探过，会给一次队首机会，避免坏源恢复后没人知道。
- 探测在飞时有标记，30 秒没回报视为卡死，允许再放一个。
- 状态按 (协议, 源) 隔离；进程重启后从 `source_events` 恢复，不会重新拿坏源试错。

### 并发填充

- 缓冲型（raw / http / release）：同 key 用 singleflight 合并，只有一个请求回源，
  其余等结果。
- 流式（Docker blob / 大文件）：同 key 一把进程内互斥锁 + `.part` 文件 flock，
  多进程部署时也只有一个人写。
- 发布是原子的：先写 `.part`，算完摘要后 `os.Rename` 到 blob 路径，再写索引。
  中途失败只留下 `.part`，Janitor 按 24h 清掉。
- 一个填充失败只影响等它的请求，不影响别的 key。

## 并行分片下载（阶段 7）

大文件按 4MB 切片，同时从多台源拉，写进同一个 `.part` 的不同偏移，最后整份校验、
原子发布。它解决的问题是「单连接跑不满带宽」，不解决「源不可信」。

**允许并行的前提（缺一条就走原来的单流）**

- 请求带可信的整份摘要（`ExpectSHA`）。目前只有 Docker 按 digest 拉 blob 满足；
  raw/release/http/git 没有可信摘要，一律不并行。
- 官方源与至少一台镜像都支持 Range，且报的总长一致。预检用 1 字节的 Range 请求，
  总长以官方源为准；镜像报的不一致就排除——总长不同意味着不是同一个对象。
- 文件 ≥ `parallel.min_size_mb`（默认 8MB），且没超过单文件上限。
- 没有正在续传的 `.part`、不是 TTL 重验、客户端自己没带 `Range`。

**怎么保证拼出来的文件是对的**

1. 每片都核对 `Content-Range` 的起点、长度、总长；对不上就换官方源重取这一片。
2. 全部收齐后整份 SHA-256 必须等于 `ExpectSHA` 才允许 `Commit`；不等就丢弃、
   记一次拒收，**不写索引**。
3. 响应头一旦写出就不能改主意：中途失败只能断开连接（客户端看到截断，而不是
   一份"成功"的坏文件）。
4. 段是乱序下载、顺序发给客户端：第 0 段边收边发，其余段按偏移依次从 `.part` 读回。

**已知代价与限制**

- 客户端要等第 0 段之后的所有段就绪才能继续读到第 1 段，慢源会拖住整条流。
- 校验失败的请求已经发出了部分字节，只能靠客户端自己的摘要校验兜底。
- 并行只对首次填充生效；续传仍是单流（乱序分片留下的半截文件不是有效前缀，
  所以中断时直接丢弃，不留 `.part`）。
- 每片多一次握手；8MB 以下收益为负，所以设了下限。
- 统计在面板「并行分片」一行：次数 / 字节 / 退回单流次数。

## 访问控制

代理路径（`/raw`、`/http`、`/git`、`/v2`）默认匿名放行，`auth.require_token: false`。
理由：这个服务的定位是「在地址前加个前缀就能用」，当公共加速站分享给别人时，
让每个使用者先找站长要 Token 会把门槛抬到没人愿意用。

需要收紧时打开 `auth.require_token: true`，此后三种形式都认：

- `Authorization: Bearer <token>`
- Basic，密码位填 Token（`docker login -u 任意 -p <token>`）
- `http://<token>@host/...`（git 客户端习惯）

面板登录（cookie 会话）与代理凭据是两条独立的路：即使 `require_token: true`，
浏览器登录面板后访问代理路径也直接放行。Token 只存 SHA-256，吊销立即生效。

匿名模式下 `/http/` 是一个开放代理，只靠 `safehttp` 挡内网、回环、链路本地和 CGNAT；
要暴露到公网，建议在反代层加 IP 白名单或限速，别只依赖服务本身。

## 已知限制（后续阶段）

- 只支持 `GET` / `HEAD`（`/git/` 另有 `POST`，只用于 upload-pack）。
- Git 只代理读；子模块与 LFS 对象不会自动走代理（LFS 的 URL 由服务端下发）。
- 淘汰在写缓存后异步触发，另有定时巡检（阶段 5）处理容量、过期、半截文件与索引对账。
- Docker 只支持一个上游、只读、不支持 `_catalog` 列表接口。
- 没有后台重验，TTL 到期靠下一次请求触发。
- 缓存命中时的 ETag 用上游的 ETag，MISS 和 HIT 一致；`/raw/` 用的是自身 SHA-256。
- 并行分片只覆盖 Docker 按 digest 拉 blob；其他协议没有可信摘要，仍是单流。
- 非 Docker 内容（raw/release/http/git）仍是 `unverified`：镜像若返回同长度但内容不同的
  字节，首次填充无法识别；缓解是 TTL 到期重验回官方源，官方双取比对未做。
