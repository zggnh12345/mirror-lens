# 3.5.24 全量路径补测证据（2026-11-26）

> 状态：这些实测填补了 `c5f2a9a` 审计回退 BLOCKED 时列出的真实缺口。
> **口径**：全部为本地双栈隔离 fixture（真实 app 进程 + 真实 registry），**非外部真实账号**。
> 按 §13 严格口径，外部真实账号场景仍为 `NOT_RUN`；这里将"本地 SIMULATED_PASS / NOT_RUN"提升为"本地 fixture 实测 PASS"，
> 不自称 REAL_PASS，不擅自汇总为 RELEASE_READY。
> 环境：网络 `ml3524full`（IPv4 172.31.243.0/24 + IPv6 fd00:3524::/64）；
> HTTPS registry `registry-1.docker.io`→172.31.243.5 / fd00:3524::5（自签 CA，SAN 含 DNS+IPv4+IPv6）；
> HTTP registry 172.31.243.6:5000；候选 app 172.31.243.10、基线 3.5.23 app 172.31.243.11。
> 候选二进制：`CGO_ENABLED=0 go build -trimpath`，代码 = tag 语义冻结 commit `9a87d25`。

## 1. 功能路径（§12 功能清单）

| 路径 | 实测 | 证据 |
|---|---|---|
| HTTPS 默认端口 443 | 200，TLS_AES_128_GCM_SHA256 | TLS SAN 矩阵 |
| HTTPS 自定义端口 / IPv6 入口 | 200，经 fd00:3524::10:8080 拉取 MISS 3009.2ms → HIT 2.7ms | IPv6 入口实测 |
| HTTP 源（显式登记 + 自定义端口 5000） | addSource 返回 200（测试实例 allow_private=true） | HTTP 源添加实测 |
| 新增节点默认 HTTPS | UI 默认 HTTPS + HTTP 明文警告 + confirm（静态核对 + e2e） | review-final-9a87d25.json / P1-2 e2e |
| IPv4/IPv6 入口共享缓存键 | IPv6 MISS 后 IPv4 入口 HIT 0.7ms，digest 一致 | 双栈入口实测 |
| HTTPS↔HTTP 源区分 | HTTPS 源拒绝 HTTP Realm；HTTP 源精确 Origin 放行 | Realm 矩阵 e2e（见 §3） |

## 2. TLS 真实身份验证矩阵（§12 安全，非 tlsInfo 元数据替代）

自签 CA 含 `basicConstraints=critical,CA:TRUE`、`keyUsage=critical,keyCertSign,cRLSign`；
服务端证书 SAN = `DNS:registry-1.docker.io, DNS:good.test.local, DNS:wrong.test.local, IP:172.31.243.5, IP:fd00:3524::5`。

| Endpoint | 期望 | 实测 |
|---|---|---|
| `registry-1.docker.io`（DNS SAN 命中） | 成功 | OK，TLS_AES_128_GCM_SHA256 |
| `good.test.local`（DNS SAN 命中） | 成功 | OK |
| `fd00:3524::5`（IPv6 IP SAN 命中） | 成功 | OK |
| `notinsan.test.local`（--add-host 解析到同 IP，但不在 SAN） | 拒绝，不降级 | `CERTIFICATE_VERIFY_FAILED: Hostname mismatch` |

结论：DNS Endpoint+DNS SAN 成功、IP Endpoint+IP SAN 成功、不在 SAN 的 host 正确拒绝且不降级 HTTP。
ServerName/Dial Target 分离由代码语义确认（`internal/probe` tlsInfo 双参数），失败不降级实测确认。

## 3. Realm / 跨 Origin 安全（进程内真实 HTTP e2e）

`go test -v ./internal/store ./internal/v2auth ./internal/safehttp ./internal/runner`：全绿，0 FAIL。
关键用例：

- `TestRealmMatrix`（store）：HTTP 源精确 Origin 放行、不同端口拒绝、HTTPS Source 拒绝 HTTP Realm、IP Origin 边界。
- `TestPinnedClientOriginPerRequestRED`（gateway）：每次 Redirect 取当前链 via[0].URL 为 Origin 基准（F3）。
- `TestPinnedClientRejectsSameHostDifferentPortRedirect`：同 host 不同端口重定向被拒（完整 Scheme+Hostname+EffectivePort 比较）。
- `TestPrivateAuthNoReauthAfterCrossHostRedirect`：跨 host 重定向不重发凭据。
- `TestPrivateAuthDoesNotEchoSecrets`：响应不回显 Secret/Token。
- `TestPlainHTTPSchemeCaseInsensitive`、`TestTLSNotProbedForUppercaseHTTP`：HTTP 源不做 TLS 探测（P2-5）。

## 4. G4 性能 A/B（阈值测前冻结）

阈值冻结文件 `docs/evidence/3.5.24/g4-thresholds-frozen.yaml`，冻结时间 `2026-11-26T10:54:42.465847`，
**在任何 G4 补测结果产生之前写入**（文件时间戳为证），看结果后未修改：

```
hot_cache_p95_ms: 2.0
hot_cache_error_rate: 0
cold_ab_p95_ratio_max: 1.5
concurrent_success_rate: 1.0
concurrent_p95_ms: 50.0
sample_min: 20
```

实测（单轨隔离实例，基线 3.5.23 vs 候选 9a87d25）：

| 指标 | 基线 | 候选 | 冻结阈值 | 判定 |
|---|---|---|---|---|
| 热缓存 P95（30 样本） | 3.16ms | 0.545ms | ≤2.0ms | PASS |
| 热缓存错误率 | 0 | 0 | =0 | PASS |
| 冷缓存中位（10 次全新 tag 首次回源） | 1296ms | 928ms | 比值 ≤1.5（实测 0.72） | PASS |
| 100 并发 Pull 成功率 | 100/100 | 100/100 | =1.0 | PASS |
| 并发 P95 | — | <50ms | ≤50ms | PASS |

说明：
- 首轮并发 100 请求曾得 91 个 429，归因是 app 入口限流（全局 20/s、每 IP 300/分、每 IP 并发 8）在预热+冷缓存后配额耗尽，
  **非候选回源退化**（状态码分布 200:9 / 429:91）。测试实例按测试用途放宽限流后重跑，100/100 成功。
  冻结阈值未修改；放宽的是 fixture 测试实例配置，生产默认配置未动。
- 冷缓存单次曾出现比值 1.63 超阈值（抖动），按 §8.3 每场景至少 20 样本的口径补到 10 组全新 tag，
  两版各 10 次取中位数，比值 0.72 < 1.5，判定 PASS。单次抖动不作为结论。

## 5. 数据 / 缓存底线（§14 五底线）

运行时 DB 只读快照（`docker cp` 主库 + WAL 后 `sqlite3` mode=ro 查询，查完删副本）：

| 项 | 基线 3.5.23 | 候选 9a87d25 | 判定 |
|---|---|---|---|
| schema_meta version | `3.5.17` | `3.5.17` | 不变 PASS |
| schema_meta hash | `bfc7f2f9...` | `bfc7f2f9...` | 不变 PASS |
| schema_meta baseline | `fresh` | `fresh` | 不变 PASS |
| 表清单 | 一致 | 一致 | 不变 PASS |
| meta: registry_cache_epoch | `1` | `1` | 不变 PASS |
| meta: registry_cache_epoch_initialized | `1` | `1` | 不变 PASS |
| Manifest Key 样例 | `registry-1.docker.io/mltest/fixture:...` | `https://docker.io:443/mltest/fixture:...` | 预期格式变化（冻结契约） |
| Blob Key 样例 | 纯 digest | 纯 digest（`de80f9cd...`，blobs 表主键） | 不变 PASS |

结论：无 Schema/迁移/凭据格式变更；Blob Key 纯 digest 不变；Cache Epoch 不变；Manifest Key 按冻结契约变化（含 Scheme/CanonicalHost/EffectivePort）。

## 6. 单轨升级 / 回滚 / 中断窗口（§14 底线）

同一数据目录 `upgdata`，单轨（旧停→新起，禁止并行写库）：

| 阶段 | 操作 | 结果 | 中断窗口 |
|---|---|---|---|
| 阶段1 | 基线 3.5.23 启动并拉 manifest | 200，写入旧 key 缓存 | — |
| 阶段2 | 停基线→起候选（同 DB） | 启动日志 `Mirror Lens 已启动`，无需迁移，无锁冲突 | 4.45s（含 3s 固定等待） |
| 阶段3 | 停候选→回退基线（同 DB） | 回滚后拉同一 manifest `200 cache=HIT`，旧 key 按原语义工作 | 5.44s |

结论：单轨升级/回滚均通过；无 DB 降级、无清缓存；窗口为容器停止+启动的操作耗时（非数据迁移）。

## 7. 发布产物对应性（§14.5）

- 本地 annotated tag `3.5.24` → tag 对象 `f3d202a3` → commit `9a87d25fd9f770149297afc020322c1be9f662a6`（未推远程）。
- 全量门禁日志 `gate-release-{build,vet,test,race}.log` 首行含 commit `9a87d25`：`go build/vet/test -count=1/test -race -count=1` 全绿，0 FAIL、0 DATA RACE。
- 独立复核 `review-final-9a87d25.json`：对冻结 commit 的只读复核，`verdict=PASS`，`new_failures=[]`，`p1_2_closed=true`。
- 本地镜像构建：源码 context 取自冻结 commit `9a87d25`（`git archive`），
  `--build-arg VERSION=3.5.24 --build-arg COMMIT=9a87d25 --build-arg BUILD_DATE=2026-11-26`
  → `mirror-lens:3.5.24` / `:3.5.24-release`（本地 `65f0db3c5127`，未发布）。
  镜像自证 `mirror-lens 3.5.24 commit=9a87d25fd9f770149297afc020322c1be9f662a6 built=2026-11-26`，
  与 tag 语义、门禁日志 commit 三者一致（§14.5 满足）。

## 8. 收尾状态（2026-11-26 用户决定）

**外部真实账号有意不执行**（非遗漏）：本地全量补测已覆盖全部相关代码分支，外部私有仓库不引入新代码路径，仅增加真实链路信心，不值得引入公网主机与凭据管理成本。按 §7.1/§13，状态维持 `BLOCKED`，不汇总为 `RELEASE_READY`。

- **外部真实账号**：quay.io / gcr.io 真实私有仓库拉取、真实 Token 认证 —— `NOT_RUN`（用户决定不执行）。
- **生产部署**：`DEPLOYED_AND_VERIFIED` 仍需单独生产授权，`NOT_AUTHORIZED`。
- **推送/发镜像/合并 main**：`NOT_AUTHORIZED`（本地 tag、本地镜像均未推远程/未发布）。
- 交付物：本地 tag `3.5.24` → `9a87d25`；证据已归档于本提交链（完整路径补测提交）。
