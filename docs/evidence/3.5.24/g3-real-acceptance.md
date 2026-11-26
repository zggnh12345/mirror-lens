# 3.5.24 G3 真实环境验收（本地 fixture，SIMULATED_PASS）

> 日期：2026-11-26
> 口径：按 §1.6，本地 `registry:2` 隔离容器 + 独立 bridge + 测试 CA 属 `SIMULATED_PASS`，非外部真实账号 `REAL_PASS`。
> 环境：`ml3524real` bridge（172.31.242.0/24）；HTTPS registry（别名 registry-1.docker.io，:443，测试 CA SAN 含该域名）；HTTP registry（172.31.242.6:5000）；候选 3.5.24（Commit 030ae7e 静态二进制）；基线 3.5.23（镜像 916a72c）。
> 生产未改动、未部署、未推送。

## 拓扑

| 组件 | 地址 | 说明 |
|---|---|---|
| HTTPS registry | 172.31.242.5:443，别名 registry-1.docker.io / mirror.test.local | registry:2 2.8.3，自签 CA（basicConstraints=CA:TRUE, keyUsage=keyCertSign） |
| HTTP registry | 172.31.242.6:5000，别名 httpreg.test.local | registry:2 2.8.3，无 TLS |
| fixture 仓库 | mltest/fixture:latest | OCI manifest `sha256:5627dfda292d73571b27dad6eff2fd422cf584fb2f5f363773fa2954e288790a`，blob `sha256:193477343537f581e3cbe372b289fcb9ee96180221824614d9de8ffedd46e251`（32B） |
| 候选 app | 172.31.242.10:8080 | 3.5.24 静态二进制，SSL_CERT_FILE=/certs/ca.crt，upstream=registry-1.docker.io，add-host 指向 fixture |
| 基线 app | 172.31.242.11:8080 | 3.5.23 镜像，同配置同 fixture |

## 结果矩阵

| 编号 | 场景 | 结果 | 证据 |
|---|---|---|---|
| G3-01 | HTTPS 源（已知 Hub 域名自动归属） | SIMULATED_PASS | 面板 POST /api/sources `{url:https://registry-1.docker.io,host:docker.io}` → 200 id=38 |
| G3-02 | HTTP 源 + 自定义端口显式登记 | SIMULATED_PASS | 测试实例 probe.allow_private=true（仅测试），`{connect_host:172.31.242.6,scheme:http,port:5000,repo_host:docker.io}` → 200 id=27 |
| G3-03 | HTTP 源私网默认拒绝（安全边界） | PASS | 默认实例（allow_private=false）同请求 → 400「172.31.242.6 属于内网地址」，F1/F2 边界生效 |
| G3-04 | HTTPS manifest 首次回源 + 缓存 | SIMULATED_PASS | 第1次 X-Ml-Cache=MISS 1462.7ms，第2/3次 HIT 2.7/2.3ms，digest 一致 |
| G3-05 | Blob Digest Key 复用 | SIMULATED_PASS | blob 首次 MISS 1380.5ms，第2/3次 HIT 17.3/3.2ms，size=32 一致 |
| G3-06 | Manifest 新 Key 首次重建成本（按 Key 单独记录） | SIMULATED_PASS | digest-ref 引用首次 MISS 1348.7ms（3.5.23 同引用 10.7ms，旧 Key 语义），不混入热缓存样本 |
| G3-07 | TLS CA 校验（不关闭校验） | SIMULATED_PASS | 候选/基线均挂 SSL_CERT_FILE=/certs/ca.crt，HTTPS 回源 200，无降级 |

## 未覆盖 / 阻塞

- IPv4/IPv6 公网真实源、外部私有账号（quay.io/gcr.io）：`BLOCKED_EXTERNAL_SOURCE`（无可用只读凭据）。
- IPv6 场景：fixture 环境未配置 IPv6 网络别名，`NOT_RUN`。
- HTTP→HTTPS 自动复制 Authorization 等跨 Origin 场景：由进程内 e2e（`policy_http_realm_e2e_test.go`）覆盖，真实进程级 `NOT_RUN`。
- 以上不记为 REAL_PASS，不伪报。
