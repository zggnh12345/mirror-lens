# Mirror Lens 安全部署要点

面向自建部署（NAS / 单节点 Docker）的运维与安全清单。只讲跟本项目强相关的部分；
通用加固（宿主防火墙、无人登录、镜像来源验证）不在范围内。

## 1. 凭据与令牌

- **管理员 Access Token 只存哈希**。配 `bcrypt` 哈希入库，不落明文、不写日志、不进 `/metrics`。
  面板登录、API 调用、换源探测都用这个 Token；忘了用 `admin` 用户重置，不回明文。
- **换源用的仓库凭据与 NAS 账密隔离**。Docker Hub / GitHub 的只读令牌单独配置，
  不要复用宿主 SSH 私钥或管理员密码。令牌通过**环境变量**注入容器，不写进镜像层、不进 git。
- **本仓库不保存任何真实令牌**。文档/代码/日志里出现的 token 一律以 `[REDACTED]` 占位。

## 2. 日志红线（已实现并测过）

下列内容**永不写入日志**：

- `Authorization`、`Token` 明文
- `Cookie`
- 私有仓库访问凭据
- 带敏感查询参数的完整 URL（回源地址的查询串常是签名，如 `?token=` / `X-Amz-Signature=`）

实现保证（见 `internal/logging`）：

- 结构化日志字段固定为 `request_id / kind / cache_key / source / status / error_code / error`，
  **没有 URL、token、cookie 字段**。
- 回源地址进日志前**整段砍掉查询串**（`SanitizeURL`）；缓存键里残留的签名同样被截掉（`SanitizeKey`）。
- 测试 `TestLoggwRedactsCacheKey` 断言：缓存键里的 `token` / `X-Amz-Signature` **不会**出现在输出。

## 3. 指标暴露（`/metrics`）

- 默认开启（`metrics.enabled: true`），但**建议只对内网或带 Token 暴露**
  （`metrics.require_token: true` 时，抓取要带 Access Token 或登录态）。公网裸奔会泄露访问模式。
- 指标标签是**封闭集合**：`scene / method / code / kind / mode / status`，
  **禁止** `url / ip / filename / cache_key / token / request_id` 这类高基数或敏感标签。
- `/metrics` 是只读快照，**每次抓取不会递归扫描缓存目录**——缓存盘占用、条目数、
  剩余空间、各协议源站数由 janitor 周期刷新后直接读，避免百万文件时抓取卡死。

## 4. 就绪探针（`/readyz` vs `/healthz`）

- `/healthz`：存活探针，进程在就 200。给编排器判断「要不要杀掉重启」。
- `/readyz`：就绪探针，**只看本机依赖**——数据库可写、缓存目录可用。
  - 数据库「可写」用 `BEGIN IMMEDIATE` + 空更新 + 回滚验证，不是 `Ping`：
    只读挂载 / 磁盘满时 `Ping` 照样通，但下次写缓存会炸。
  - 缓存目录检查只 `stat` + 看剩余空间，**不建临时文件**（K8s 一秒一探针，建文件会刷满目录）。
  - **公网源站不可达不算未就绪**：源站挂了 Mirror Lens 仍能服务缓存、仍能换源，
    把它算成就绪失败只会让编排器把好好实例摘掉。
  - 依赖坏时返回 503，错误信息是固定文案（不泄露内部路径 / SQL）。

编排建议：`livenessProbe` → `/healthz`，`readinessProbe` → `/readyz`，二者都走内网，不要挂公网。

## 5. 反向代理（Traefik）

- 用**独立子域名**（如 `mirror.example.com`），**不要**挂在 `/dockerhub/` 这类路径下——
  前缀代理会和上游路径解析打架，独立域名最省心。
- TLS 在网关层终止；入口限流放在 Mirror Lens 这一层（已有 `ratelimit_rejected_total` 可观测），
  不要只靠运维侧。
- **信任边界**：Mirror Lens 默认**不信任任何上游代理**（`trusted_proxies` 为空）。
  如果前面是 Traefik，再显式把它加进 `trusted_proxies`，否则 `X-Forwarded-*` 会被忽略，
  避免客户端伪造真实 IP / 绕过限流。

## 6. 出站守卫（SSRF 防护）

回源请求统一走 `internal/safehttp`：

- 解析后的 IP 落在**私网 / 链路本地 / 回环 / 保留段**直接拦掉，不允许访问内网资源。
- 拒绝 `http://user:pass@host` 这类带凭据的 URL，避免凭据外泄给第三方源。
- 这套守卫有独立回归测试（`internal/safehttp/ssrf_test.go`），改动网络代码先跑它。
