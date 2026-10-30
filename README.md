# Mirror Lens · 镜像加速监测

给 **Docker Hub** 和 **GitHub** 的第三方加速源做体检：哪些还活着、谁快、有没有被篡改。

一个容器、一个面板、一个 SQLite 文件，不碰你的任何配置。

> **它不代理流量。** 只负责测量和推荐，配置片段自己复制粘贴。

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

打开 `http://<你的IP>:8080`。首次启动会写入 45 个内置候选，后台自动跑第一轮探测（约 3 分钟出完整结果）。

> 用 bind mount 挂 `./data` 时，容器内是 uid 10001，需要先 `sudo chown -R 10001:10001 ./data`，否则启动会直接报"数据目录不可写"。嫌麻烦就改用 named volume，权限自动继承。

从源码构建：

```bash
docker build -t mirror-lens .
```

## 面板

- **Docker Hub / GitHub 两个标签页**，互不干扰
- 状态色条：绿=可用、黄=不稳定、红=已失效、紫=内容校验失败、灰=待验证
- 每行显示延迟、实测速度、24 小时可用率、证书剩余天数、最近错误
- **检测全部**：跑一轮 L2（快）
- **测速 + 校验**：跑一轮 L3（慢，要下真实文件）
- **发现新源**：扫描公开列表仓库，新域名进待验证池
- **导出配置**：只输出通过校验的源，按速度排序

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ADDR` | `:8080` | 监听地址 |
| `DATA_DIR` | `/data` | SQLite 存放目录 |

命令行参数 `-addr`、`-data`、`-no-seed` 优先级更高。

## API

```
GET    /api/health
GET    /api/sources?kind=docker|github
POST   /api/sources          {"kind":"docker","base_url":"https://...","name":"可选"}
PATCH  /api/sources/{id}     {"enabled":false}
DELETE /api/sources/{id}
GET    /api/series/{id}      最近 120 个检测点
POST   /api/probe            {"kind":"all|docker|github","tier":1|2|3}
POST   /api/discover
GET    /api/export?kind=docker|github
```

没有鉴权。**只监听内网，不要暴露到公网**——它会让任何人往你的库里加探测目标。

## 安全边界

- 不改 `daemon.json`，不改 git config，不写任何系统文件
- 除探测目标外不发起任何外部请求（无遥测、无统计上报）
- 容器以非 root 运行，`no-new-privileges`
- 探测目标是用户可控的 URL，等于一个 SSRF 面：所以只该在内网跑

## 已知限制

- **Docker 的自动发现是真的抓列表**：定期扫描维护 Docker 镜像站的 GitHub 仓库，新域名进待验证池，通过 L2 才转正。实测一轮能从 45 个内置源之外再挖出 6 个可用源。
- **GitHub 代理没有可靠的公开列表**，试过的几个 awesome 仓库要么 404 要么只列一两个。所以 GitHub 侧以内置池 + 手工添加为主，自动发现基本不产出。
- 探测点在哪，测出来的速度就代表哪。容器跑在代理后面时，数字不代表直连速度。
- 第三方源没有 SLA，任何"可用"都只描述此刻。

## License

MIT
