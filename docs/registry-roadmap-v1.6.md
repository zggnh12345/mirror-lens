# Mirror Lens Registry 后续版本路线图 v1.6

**状态：** 修订草案，待冻结  
**基准日期：** 2026-11-15  
**基线：** 3.5.10（`b4b0b22`）  
**合并来源：**
- `docs/3.5.4-convergence-roadmap.md`
- `docs/3.5.8-registry-phase1-design.md`
- `docs/3.5.9-public-source-matrix.md`
- `docs/3.5.10-source-realm-design`
- `docs/registry-roadmap-v1.3.md`
- `docs/registry-roadmap-v1.4.md`
- `docs/registry-roadmap-v1.5.md`
- 三轮路线图审查意见

> 本文只做版本安排、边界和验收口径，不代表下列版本已经实现。
> 版本号是规划占位，不承诺发布日期。每个版本必须单独验收，未通过不得进入下一版本。

## 1. 本版修正摘要

v1.6 相对 v1.5 的核心修正：

1. `3.5.10` 已被多能力标签修复占用；原 Source Realm 计划顺延为 **3.5.11**。
2. `3.5.12` / `3.5.13` 不再写成“从零实现第一批 Registry”，改为**既有实现缺口闭环与验收补齐**。（2026-11-16 后续修订：两者已合并为 3.5.12，3.5.13 编号退役，见 1.1）
3. `3.5.11` 的 Realm / Token 身份从第一天就采用 **Source + Host**，但不提前实现多 Host UI 和候选调度。
4. `3.5.14` 拆成 **3.5.14a（回填 + 投影 + 新写路径）** 和 **3.5.14b（只切换权威读路径）**。
5. `source_host_caps` 成为**所有逻辑 Registry Host 的能力权威**，Docker Hub / GHCR 不再作为例外。
6. 认证身份改为**每次 attempt 的 request context**，不再要求静态 `adapter.Plan` 携带 `SourceID`。
7. `ghcr_prefix` 纳入 `source_host_caps` 的兼容投影模型，冻结为布尔，不做枚举。
8. `probe_status` 从三态改为四态，补上 `Mismatch`，与现有 `RegistryCapMismatch=2` 对齐。
9. `3.5.14a` 同时包含新写路径；`3.5.14b` 只切读路径，不含旧读路径和 dual-read 开关。
10. 离线迁移增加 `3.5.7 → 3.5.11 → 3.5.12 → 3.5.14a` 路径和 `backfillHostCaps` 步骤，不能只复用旧的三基线迁移。
11. `3.5.15` 收窄为 CredStore + 标准私有 Registry 认证；GitLab PAT 独立 3.5.15a。
12. `3.5.16` 的公开 Manifest 竞速依赖 3.5.11 + 3.5.14b；私有竞速才依赖 3.5.15。
13. 撤回“流式 P0 未关闭”的错误判断；保留为 G0 回归门禁。
14. 增加 Host 验收矩阵、探测四态、迁移验收和双用途源回归。
15. 3.5.11 拆成 **11a（身份与 resolver 基础）/ 11b（策略表与切换）/ 11c（编辑与清理）**；3.5.11 只在 11b 验收后算完成。
16. 3.5.11 的“单事务”只覆盖运行时数据库写入；Token Cache 采用 generation 版本化失效，Schema 迁移是离线迁移 + 原子发布，二者都不纳入单事务承诺。
17. 运行时多表写入统一 `BEGIN IMMEDIATE` 单事务；`SetCapsLive` / `CommitHostProbe` 属于 3.5.14a，不混进 3.5.11；迁移工具仍保持“离线迁移 + 原子发布”，不改造成大事务。
18. 补充 3.5.11a 实施基线：当前代码基线是 `b4b0b22`，分支从当前 `HEAD` 开；11a 请求路径补 `headOnce` / `rangeReq` / `openUpstream` / resume / `watch.go`；Probe 的 `Realms` 换 resolver 但 `CredentialSnapshot` 不动；Token Cache 容量、fail closed 语义和 D1–D8 一并写入。

### 1.1 2026-11-16 修订

1. 原 **3.5.12 + 3.5.13 合并为 3.5.12**；3.5.13 编号退役，不单独发布，3.5.14 及后续版本不改号。
2. 3.5.12 定位为 **合并验收 + 定点修复**，正式纳入 12c（11c 欠账）、12-F1（Host 关闭隔离）、12-F2（调度缓存 0 窗口验证）、12-S（Source 就绪）、12-T（候选到回退链路测试）。
3. **3.5.11c 原计划未实施**。3.5.11 以 11b 验收为完成标志；历史记录不追记为已完成，11c 调整并入 3.5.12 作为独立工作批次 12c。
4. 离线迁移链改为 `3.5.7 → 3.5.11 → 3.5.12 → 3.5.14a`。
5. 12-S 是**按 Host** 的门禁：`12-S(host)` 未通过，不启动该 Host 的真实回源验收；无公共 Source 的 Host 标记“外部 Source 阻塞”，不阻塞其他 Host 与整体发布。
6. 生产基线以 2026-11-16 实测为准（见 5 的 3.5.12-A 基线表），不以 Git 标签推断部署状态。
7. **12c 收敛为 Source 级 Realm 策略管理 + Host 只读汇总**，不新增 Host 级策略模型，不改 Schema；不存在“策略启停”字段，停用语义为删除策略行并 fail closed。
8. **12-S 必须先收窄生产 `multi_origin_hosts` 再逐 Host 加源**。生产当前未显式配置该字段，默认 8 个 Host 全开；先建源再验收不构成灰度。
9. **12-F1 只过滤真正的探测执行入口**（`jobs()` / `ListDueHostProbes`）。`reconcileRepos` 不执行探测、不写健康状态，不纳入白名单过滤。
10. **12-F2 不含 `repo_host` 修改**：当前没有修改 `repo_host` 的 API，`SetRepoHost` 仅为 Store 方法；该能力留到 3.5.14 一源多 Host 阶段统一设计。

## 2. 当前基线

### 2.1 已完成版本

| 版本 | 内容 | 状态 |
|---|---|---|
| 3.5.4 | 隐式 Registry 身份收敛、Canonical 前置 | 已发布 |
| 3.5.5 | Canonical Schema、离线升级 | 已发布 |
| 3.5.8 | 第一批公开 Registry 多源调度 | 已发布 |
| 3.5.9 | 多源开关、只读标签、鉴权回退修正 | 已发布 |
| 3.5.10 | 多能力标签修复（HUB/GHCR/RAW） | 已发布并部署 |
| 3.5.11 | Source 自有 Realm 与认证隔离 | 已发布并部署（11b 验收完成；11c 并入 3.5.12） |

### 2.2 3.5.8 / 3.5.9 已经完成的部分

`3.5.8` 第一批范围：

- `gcr.io`
- `<location>-docker.pkg.dev`
- `registry.k8s.io`
- `quay.io`
- `mcr.microsoft.com`
- `registry.gitlab.com`
- `public.ecr.aws`
- `cgr.dev`

`lscr.io` 明确不启用。

3.5.8 已完成：

- Host 独立准入
- `multi_origin_hosts`
- foreign Registry 候选调度
- 匿名 Realm / Token 基础流程
- 候选失败回官方
- 共享 Blob 缓存语义
- 真实公开仓库验收

`3.5.9` 公共第三方 Source 覆盖是 **6/8**，不是 0/8：

| Host | 3.5.9 公共 Source 情况 |
|---|---|
| `gcr.io` | 多提供方 |
| `registry.k8s.io` | 多提供方 |
| `quay.io` | 多提供方 |
| `mcr.microsoft.com` | 多提供方 |
| `registry.gitlab.com` | 仅 DaoCloud |
| `public.ecr.aws` | 仅 DaoCloud |
| `<location>-docker.pkg.dev` | 无可用公共源 |
| `cgr.dev` | 无可用公共源 |

必须区分三层：

1. **Host 能力实现**：程序是否允许请求、解析路径、认证、进入调度。
2. **Source 覆盖**：有没有可用的第三方镜像站。
3. **实际运行验收**：探测、候选命中、拉取、Digest 校验、失败回退是否全部成立。

没有公共 Source 不等于程序不支持该 Host；有 Source 配置也不等于已完成加速验收。

> **2026-11-16 生产实测修正：** 上表是 3.5.9 的历史验收记录，不是当前生产库存。实测生产库 91 条 Source 中，`docker.io` 34 条（`registry_cap=-1`）、`ghcr.io` 2 条（`registry_cap=1 native`），8 个 foreign Host 的 Source 数均为 **0**。因此 3.5.12 的真实回源验收必须先完成 12-S（Source 就绪），不能把历史覆盖表当成现成测试对象。

### 2.3 当前模型不变量

- 一个 Source 只有一个主 `repo_host`
- 一个 Host 可以有多条 Source
- 一条 Source 不能同时服务多个 Host
- `repo_host` 是当前的主归属，不是永久唯一能力模型
- UI 标签只能来自持久化能力与探测结果，不能从 URL 猜测

## 3. 一源多 Host 问题

### 3.1 问题定义

公共镜像站经常用同一个 base URL 同时代理多个 Registry Host。春川 `https://gp.suwa.eu.org` 是首个实测样本：

- Docker Hub
- GHCR
- GCR
- `registry.k8s.io`

实测：

```text
GET https://gp.suwa.eu.org/v2/registry.k8s.io/pause/manifests/3.9
→ 200
→ sha256:7031c1b283388d2c2e09b57badb803c05ebed362dc88d84b480cc47f72a21097
```

这不是 K8S 专属问题。只要一条 Source 同时支持多个 Host，当前模型都会只保留一个主 Host，其他能力无法显示、无法调度、无法独立探测。

### 3.2 为什么首页没有 K8S

生产库中的春川 Source 当前是：

```text
repo_host             = docker.io
registry_cap          = -1
registry_path_mode    = ''
registry_probe_at     = 0
```

`registry_host_recovery` 和 `source_health_scope` 中也没有春川的 K8S 记录。

因此这不是 UI 漏读，而是：

- 春川只被登记为 Docker 主源
- 没有 K8S 的 Host 归属
- 没有 K8S 的探测结果
- 当前 Schema 不支持一条 Source 多 Host

**不能只补一个 K8S 标签。** 那会显示“支持 K8S”，但调度器不会把 K8S 请求分给它，属于假标签。

### 3.3 两个不同概念

- **同一 Host 多 Source**：一个 Host 配多个镜像源 → 3.5.12 做正式验收（原 3.5.13 并入）
- **一源多 Host**：一个镜像源同时服务多个 Host → 3.5.14a/3.5.14b 做能力模型

本文说的“其他多源也一样”指后者。任何一条 Source 只要实际支持多个 Host，都必须走同一套 `source_host_caps` 模型，不能按 Host 或按站点单独开分支。

## 4. 版本总表

| 版本 | 主题 | 依赖 | 默认行为 |
|---|---|---|---|
| 3.5.11 | Source Realm + Source/Host 认证身份 | 3.5.10 | 旧 Source 行为不变 |
| 3.5.12 | 合并验收 + 定点修复（原 3.5.12 + 3.5.13） | 3.5.11 | 新 Host 默认关闭；修复随开关生效 |
| 3.5.13 | 编号退役，已并入 3.5.12，不单独发布 | — | — |
| 3.5.14a | `source_host_caps` 回填 + 旧字段投影 + 新写路径 | 3.5.12 | 读路径不变 |
| 3.5.14b | 只切换 `source_host_caps` 为权威读路径 | 3.5.14a | 旧单 Host Source 不变 |
| 3.5.15 | CredStore + 标准私有 Registry 认证 | 3.5.14b | 匿名优先 |
| 3.5.15a | GitLab PAT / Deploy Token 专项 | 3.5.15 | 单独验收 |
| 3.5.16 | 公开 Manifest 竞速 | 3.5.11 + 3.5.14b | 默认关闭 |
| 3.5.17 | Blob 竞速可行性评估 | 3.5.16 | 仅评估 |

> 3.5.15a 不阻塞 3.5.16 的公开 Manifest 竞速；私有 Manifest 竞速依赖 3.5.15（以及 3.5.15a，如果使用 GitLab 专用凭据）。
>
> 本链中明确改 Schema 的是 **3.5.11**（`source_realm_policy`）和 **3.5.14a**（`source_host_caps`）。3.5.14b 只改读路径，不改 Schema。
>
> 3.5.11a / 3.5.11b / 3.5.11c 是 3.5.11 的内部里程碑：11a 只做身份与 resolver 基础，11b 才完成策略切换，11c 做编辑与清理。3.5.11 以 11b 验收通过为完成标志。
>
> **2026-11-16 调整：** 11c 原计划未实施，未随 3.5.11 发布；现并入 3.5.12 作为独立工作批次 **12c**，独立提交、独立回滚。历史记录不追记为“3.5.11 已完成 11c”。

## 5. 各版本安排

### 3.5.11 Source Realm + Source/Host 认证身份

**目标：** 把 Realm 判定从“Host 全局白名单”改为“按 Source + Host 计算的策略”，解决公共镜像站使用自己的认证端点时被误杀的问题，并让探活与 Gateway 共用同一套认证身份。

#### 3.5.11 分阶段：不压成一个最小 patch

3.5.11 同时包含身份传递、策略权威、Token Cache 失效和 Schema 迁移，不能压成一个“最小改动”。拆成三个内部里程碑：

| 阶段 | 内容 | Schema | 完成标志 |
|---|---|---|---|
| 3.5.11a | 认证身份、策略快照、统一 resolver、Token Cache key | 否 | 行为不变，所有 registry 请求都带身份 |
| 3.5.11b | `source_realm_policy` + 迁移 + Probe/Gateway 切换 | 是 | 3.5.11 功能完成 |
| 3.5.11c | 策略编辑 UI/API、审计、旧代码清理 | 否 | 原计划未实施，并入 3.5.12 的 12c |

**3.5.11 只有在 3.5.11b 验收通过后才算完成。** 3.5.11a 是内部基础，不单独对外宣称“3.5.11 已完成”。

范围：

- 新增 `source_realm_policy`，按 `(source_id, host)` 存储一个主 Realm 和最多 3 个额外 Realm
- 旧 Source 初始只有 `host = repo_host` 一条策略
- Realm 精确匹配 scheme + host，不接受通配符、userinfo、`http://`、IP 字面量
- 允许策略显式写跨 Host 的 Realm，例如 `lscr.io -> ghcr.io`
- 第三方 Source 不自动继承官方 Host 的 Realm
- 探活与 Gateway 使用同一套策略
- Token 缓存按完整身份隔离
- Source 禁用、归档、删除、改 Host、凭据轮换、Realm 策略变更时使旧 Token 失效
- 失败关闭，不因策略解析失败而放行

明确不做：

- 不做多 Host UI
- 不做多 Host 候选调度
- 不做多 Host 能力管理
- 不实现私有仓库 CredStore
- 不实现 OAuth / OIDC / 云厂商 IAM

#### 3.5.11-A 数据模型

建议一行一个 `(source_id, host)`，不要每行 Realm 各存一个 `policy_gen`：

```sql
CREATE TABLE source_realm_policy (
  source_id     INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  host          TEXT NOT NULL,
  primary_realm TEXT NOT NULL,
  extra_realms  TEXT NOT NULL DEFAULT '[]',
  policy_gen    INTEGER NOT NULL DEFAULT 1,
  updated_at    INTEGER NOT NULL,
  PRIMARY KEY(source_id, host)
);
```

`extra_realms` 存最多 3 个对象：

```json
[{"realm":"https://example.com","service":"example","enabled":1}]
```

理由：

- 一个 Host 只有一个 `policy_gen`，Token Cache 不会拿错代次
- 主 Realm 由 `primary_realm` 列保证唯一，不需要 partial unique index
- 额外 Realm 上限在同一个事务内检查
- 修改策略只更新一行，失败不会留下半套代次

如果以后坚持规范化多行，必须另加 `source_realm_policy_meta` 存 `policy_gen`；不能把 generation 分散到每个 Realm 行。

策略规则：

- `primary_realm` 只存 origin（scheme + host），3.5.11 不承诺路径锁定
- 只允许 `https://`
- 不允许通配符、userinfo、IP 字面量、query、fragment
- `extra_realms` 最多 3 个；`enabled=0` 也占位，避免先禁用再新增绕过上限
- `service` 在 3.5.11 只作为展示/审计字段，不参与 Realm 匹配；Token 请求仍使用 Challenge 的 `service`。3.5.15 私有凭据再决定是否锁定
- `lscr.io` 的 `ghcr.io` 是显式策略条目，不是通配符
- Docker Hub 的 `registry-1.docker.io` / `index.docker.io` 统一规范成 `docker.io`
- 没有策略的 `source_id > 0` 必须失败关闭
- `source_id = 0` 表示官方直连，走独立的 `OfficialPolicy`，不是“查不到就回退”

`policy_gen` 规则：

- 每个 `(source_id, host)` 一个 generation
- 修改主 Realm、额外 Realm、启用状态、service、删除策略：`policy_gen++`
- 同一个事务内完成策略行和 generation 更新
- Token Cache 用 generation 版本化 key，不依赖物理删除

#### 3.5.11-B 认证链路

每次 attempt 在候选选中后写入不可变策略快照：

```go
type PolicySnapshot struct {
    SourceID   int64
    LogicalHost string
    PolicyGen  int64
    CredGen    int64
    Official   bool
    Realms     []RealmRule
}
```

要求：

- `adapter.Plan` 只增加 `LogicalHost`；不在 Plan 创建阶段强塞 `SourceID`
- 候选选中后，把 `PolicySnapshot` 写入该次 attempt 的 `request.Context()`
- `bearerTransport` 从 `req.Context()` 取完整身份，不能只根据 `req.URL.Host` 猜 Source
- 不能把 Source 名称当身份；名称可改、可重复
- 并发 Token 刷新使用同一完整 key
- 不能把用户凭据发给第三方 Source 的 Realm
- `SourceID = 0` 必须显式表示官方直连；身份缺失不能解释成 0

所有 registry 上游请求必须走统一构造函数，不能只改 `doUpstream`：

- `internal/gateway/stream.go`：`doUpstream`
- `internal/gateway/registry.go`：`headOnce`
- `internal/gateway/parallel.go`：`rangeReq`
- `internal/gateway/gateway.go`：`openUpstream`
- `internal/gateway/stream.go`：resume 请求
- `internal/gateway/watch.go`：manifest 换源
- 其他直接调用 `g.cli.Do` 的 registry 路径

只补 `doUpstream` 不够；`rangeReq` 目前仍写着“认证按主机处理”，`watch.go` 漏掉会形成 fail-closed 盲区。

请求分类必须明确：

- Registry 请求：必须带身份，身份缺失 fail closed
- Git / Private / Raw 请求：不走 Registry 身份，不要误加
- 官方直连：`SourceID=0` + `Official=true`，不是身份缺失

对 `plan.Kind == "registry"` 的请求，如果 context 没有身份，直接失败关闭，不能退回按 Host 认证。

Probe 侧：

- `CredentialSnapshot` 字段保持不变
- `probe.RegistryHostProbe.Realms` 的 map 入口替换为 resolver / 身份入口
- Probe 和 Gateway 对同一 Source/Host 必须得到同一策略结果
- 不能因为“探活是独立进程”就保留第二套 Realm 逻辑

Probe 和 Gateway 必须共用一个接口：

```go
type RealmPolicyResolver interface {
    Resolve(ctx context.Context, sourceID int64, host string) (PolicySnapshot, error)
}
```

- 3.5.11a 提供 `LegacyHostPolicy`，行为与当前完全一致
- 3.5.11b 提供 `StorePolicy`
- `v2auth.Authenticator` 和 Gateway 都只依赖这个接口
- `ParseChallenge` 和 Realm 匹配只保留一份
- `v2auth.DefaultRealms` 只保留为 `OfficialPolicy` 的种子，不再作为 Source 的 fallback
- `runner.Options.Realms` / `probe.RegistryHostProbe.Realms` 从生产路径删除，只允许测试显式注入

Token Cache 使用结构体 key，不要拼字符串：

```go
type tokenKey struct {
    SourceID  int64
    Host      string
    Realm     string
    Service   string
    Scope     string
    CredGen   int64
    PolicyGen int64
}
```

同一个 key 的并发刷新使用 `singleflight`，避免多个请求同时打 Token 端点。

singleflight 细节：

- leader 使用 `context.WithoutCancel(req.Context())` 保留 Source 身份，但不受首个请求取消影响
- 另加独立超时，不能无限挂起
- waiter 各自 `select` 自己的 context
- leader 失败时把错误交给等待者，并清理 flight
- 不新增 `x/sync` 依赖，复用现有 flight 模式即可

Token Cache 容量：

- 当前 `tokenCacheMax=256` 是按 `realm|service|scope` 估的
- 加入 Source + Host + 两个 generation 后，256 可能偏小
- 改成配置项，默认至少 1024
- 淘汰仍按过期时间 + LRU；generation 是隔离维度，不是淘汰顺序
- 3.5.11b 策略变更时按 `source_id` 定向清理旧 generation
- 不允许把旧 generation 保留成 fallback

fail closed 错误语义：

- 身份缺失 / 策略缺失：返回 `503 Service Unavailable`，带明确错误码（如 `X-ML-Error: auth_policy_missing`）
- 不能伪装成 401；否则无法区分“策略缺失”和“镜像不存在/无权限”
- 上游真实 401：保持 401
- 候选源 401：跳过候选并记录，不把候选 401 直接透传给客户端
- 官方 401：按现有语义透传
- 增加 `identity_missing` / `policy_missing` / `token_refresh_error` 指标，仅用于门禁和排障，不做运行时 dual-read 开关

#### 3.5.11-C Schema 迁移

`source_realm_policy` 是新增表，属于 Schema 变更：

- 必须走离线 `db-migrate`，不能运行时建表
- 输入是 3.5.10 的 3.5.7 Schema，输出是 3.5.11
- 必须提升 `schema.Version` 和 `CanonicalSchemaHash`
- 采用临时库 + 原子发布：`EnsureFresh -> 临时库 -> copyAll -> publishNoClobber`
- 失败清理临时库和 sidecar，不保留半迁移目标库
- 旧二进制不能直接打开新库
- 回滚方式是恢复备份库 + 旧 `secret.key`
- 不能把“旧二进制直接跑”当作回滚方案
- 迁移完成后只保留一个 Schema 版本

**迁移不能宣称单事务。** 准确表述是“离线迁移 + 原子发布，失败不留半迁移状态”。

当前代码事实：

- 代码里没有 `realm_extra`；旧 Realm 实际是 `v2auth.DefaultRealms` 加上测试/runner 注入的 `Realms` map
- 迁移不能按不存在的 `realm_extra` 写逻辑
- 旧 Source 按 `repo_host` 种子化
- 已知 Host + 自定义 BaseURL（例如 `ghcr.dockerproxy.com` -> `ghcr.io`）：BaseURL origin 为主，内置官方 Realm 放 extra；`ghcr.io` 本身必须有内置条目
- `lscr.io` 必须保留 `ghcr.io` 这条显式跨 Host
- Artifact Registry 按自身 Host 种子化
- 自定义 Host、默认表里没有的 Source：没有策略，进入迁移报告
- 没有策略的 Source 失败关闭，不回退旧白名单

迁移报告至少输出：

```text
source_id | host | 原因 | 建议动作
```

#### 3.5.11-D 零残留、单轨、单事务审核

**1. 零残留**

完成后运行时只能保留：

- `source_realm_policy`
- `OfficialPolicy`

必须删除或停止使用：

- `v2auth.Authenticator.realms`
- `v2auth.DefaultRealmsFor` 作为 Source 的 fallback
- `gateway.realmAllowedForHost`
- `gateway.parseBearer`
- `runner.Options.Realms`
- `probe.RegistryHostProbe.Realms`
- 任何 Host → Realm 的静态表
- 任何“新表查不到就回退旧白名单”的默认值

“零残留”的定义：

- 旧 Token **不可达**
- 旧 namespace **已清理**
- 旧代码路径**已删除**
- 不是“数据库和缓存同一事务”
- 也不是“缓存旧条目当场物理消失”

迁移工具可以保留只读的旧种子；运行时不能引用。

**2. 单轨**

- Probe 和 Gateway 只读 `source_realm_policy` 或 `OfficialPolicy`
- 没有 dual-read 开关
- 没有 feature flag
- 没有按 Host 写死的 Realm 特判
- 没有策略时 fail closed
- `OfficialPolicy` 是显式分支，不是 fallback
- 3.5.11a 的 `LegacyHostPolicy` 是当时唯一权威；3.5.11b 切换后删除
- 迁移的影子对比只能在离线工具里跑

只要保留一个 fallback，3.5.11 就是双轨。

**3. 单事务**

数据库业务写入可以做到单事务：

- 创建 Source：`sources` + `source_realm_policy` + `policy_gen` + `source_events`
- 修改策略：`source_realm_policy` + `policy_gen++` + `source_events`
- `SetRepoHost`：删除旧 Host 策略 + 建立新 Host 策略 + `policy_gen++` + `source_events`
- 禁用 / 归档：状态变化 + `policy_gen++` + `source_events`
- 删除 Source：删除策略 + 原有清理 + `source_events`
- 额外 Realm 上限 3 在事务内检查

当前 `AddSource`、`SetRepoHost`、`DeleteSource` 已有事务，见：

- `internal/store/store.go:488`
- `internal/store/host.go:77`
- `internal/store/store.go:737`

需要把策略写入接进同一个事务，不能先建 Source 再单独建策略。

Store 当前是单连接：

```go
db.SetMaxOpenConns(1)
```

见 `internal/store/store.go:197`。运行时策略写入必须显式 `BEGIN IMMEDIATE`；当前单连接不是省略事务边界的理由。多表写入统一走事务 helper（如现有 `withSourceLive`，需要时新增 `withTx`），禁止“先裸 UPDATE，再补另一张表”。

这里的单事务只覆盖 3.5.11 的运行时策略写入。`SetCapsLive` / `CommitHostProbe` 属于 3.5.14a 的写入路径，见 3.5.14a-C；不要把 14a 的事务要求混进 3.5.11 的实现范围。

不能纳入数据库事务的部分：

- **Token Cache**：进程内存，不是数据库事务的一部分；用 generation 版本化 key 做逻辑失效
- **Schema 迁移**：一次性离线操作，采用临时库 + 原子发布，不是运行时事务
- **数据库 + 缓存 + 迁移整体**：不承诺单事务

**4. 生命周期覆盖**

- 创建 Source：建策略
- `SetRepoHost`：删旧 Host 策略、建新 Host 策略、`policy_gen++`
- 禁用 Source：`policy_gen++`
- 归档 Source：`policy_gen++`
- 删除 Source：显式删除策略，不能只依赖外键
- 轮换凭据：`credential_gen++`

旧 Host 策略不清理、禁用/归档不 bump generation，都会让旧 Token 继续可达。

#### 3.5.11-E 验收

> 11b 的可执行验收清单、故障注入和旧二进制核对见 [`3.5.11b-acceptance.md`](3.5.11b-acceptance.md)。

3.5.11a：

- 所有 registry 请求都带 `PolicySnapshot`
- HEAD、分片、续传、换源、并行路径不丢身份
- 缺少身份时 fail closed
- 旧 Realm 判定结果与 3.5.10 一致
- Token Cache 结构体 key 能区分同一 Realm 的不同 Source
- `go test -race` 覆盖并发 Token 刷新

3.5.11b：

- 每个 `(source_id, host)` 有且仅有一个 `primary_realm`
- 额外 Realm 最多 3 个，事务内校验
- 迁移报告为空，或每条缺失都有处理记录
- `m.daocloud.io` 作为 `public.ecr.aws` 的 Source 可通过探活
- `lscr.io -> ghcr.io` 跨 Host 策略仍可用
- 自定义 Source 不会被官方 Host 白名单误杀
- 官方 Realm 不会被第三方 Source 冒用
- 探活通过和 Gateway 实际回源不出现两套 Realm 语义
- 同一 Source 的不同 Host 不共享错误 Token
- Source 删除、禁用、归档、改 Host、轮换凭据后旧 Token 不能复用
- 没有策略的 Source 被明确阻断，不是静默 401
- 迁移后旧二进制被拒绝启动
- 回滚演练使用备份库 + 旧 `secret.key`

3.5.11c（原计划未实施，已并入 3.5.12 的 12c；以下清单作为 12c 验收基线）：

- UI/API 可编辑主 Realm 和额外 Realm
- 主 Realm 唯一
- 超过 3 个额外 Realm 被拒绝
- 修改策略写 `source_events`
- 修改后旧 Token 不可达
- 旧 Realm 代码和配置读取路径已删除

通用门禁：

- 运行时只引用 `source_realm_policy` 和 `OfficialPolicy`
- `rg` 不到旧 Realm 运行时符号
- `sqlite_master` 只有新策略表，没有旧 Realm 表
- 无 dual-read、无 feature flag
- 迁移临时文件清理
- 失败注入覆盖 `AddSource` / `SetRepoHost` / `DeleteSource` / 禁用启用 / 策略编辑 / 归档

#### 3.5.11-F 兼容与扩展

- 3.5.12（含原 3.5.13）：直接复用 3.5.11b 的 resolver，不再改 Realm 模型
- 3.5.14：每个新增 Host 能力必须同时有 `source_realm_policy`，否则该 Host 不进候选
- 3.5.15：继续用 `credential_gen` 扩展，不改策略表主键
- 3.5.16：竞速按 attempt 带策略快照，不能共享 Source 级 Token
- 未来一源多 Host：策略表已经是 `(source_id, host)`，不需要重做
- 未来换缓存后端：Token key 已完整，可直接迁移

回滚：

- 3.5.11a 是 code-only，可在 3.5.11b 前单独回滚
- 3.5.11b 改 Schema，回滚靠备份库 + 旧 `secret.key`
- 3.5.11c 是 code-only，可单独回滚
- 3.5.11b 不保留旧读路径、不保留 dual-read、不保留 feature flag

不能承诺：

- 数据库和 Token Cache 原子提交
- 缓存旧条目立即物理消失
- Schema 迁移单事务
- 旧二进制直接接管新 Schema
- 3.5.11 完成一源多 Host

#### 3.5.11-G 3.5.11a 实施基线

当前工作副本（以实际环境为准）：

- 代码基线是 `b4b0b22`
- 11a 实施分支是 `feat/3.5.11a`；本次复核的最终代码提交是 `73d42bf`（hash 以实际 `git log` 为准，后续文档提交会再前进）
- 代码与本路线图文档分开提交；不要用文档 HEAD 判断代码基线
- 分支从当前 `HEAD` 开，不要从 `b4b0b22` 开，否则会丢本地文档
- `0f4a9bb → b4b0b22` 实际约 167 个文件，不是 111 个；不要用旧统计做基线
- `go` 不在 PATH，但工具链在 `/opt/data/cache/tools/go/bin/go`（go1.27.3）
- 基线测试已通过：`internal/v2auth`、`internal/gateway`、`internal/store`、`internal/runner`

建议：

```bash
# 先处理未提交的 v1.6 文档，再开分支
git switch -c feat/3.5.11a HEAD
```

D1–D8 审核结论：

| 编号 | 事实 | 11a 动作 |
|---|---|---|
| D1 | Token Cache key 只有 `realm / service / scope`，没有 Source 身份 | 改成结构体 tokenKey，加入 Source/Host/CredGen/PolicyGen |
| D2 | Token 刷新没有并发合并，冷启动/过期会并发打 Token 端点 | 加 singleflight |
| D3 | 当前用 `req.Context()`，合并后 leader 取消会拖垮等待者 | leader 用 `WithoutCancel` + 独立超时，waiter 用自己的 ctx |
| D4 | `scopeFromPath` 在 `host_prefix` 下把 Host 段算进 repository | 用 `PolicySnapshot.LogicalHost` 精确剥前缀，补测试 |
| D5 | `v2auth` 和 Gateway 各有一套 Challenge 解析 | 删 Gateway 侧 `parseBearer` / `splitParams`，统一用 `v2auth.ParseChallenge` |
| D6 | `realmAllowedForHost` 查不到会回退内置白名单 | `LegacyHostPolicy` 完整复刻四段语义；11b 再删 fallback |
| D7 | Private 走独立 client，不走 `g.cli` | 11a 明确排除，不要给 Private 加 Registry 身份 |
| D8 | `watch.go:347` 的 manifest 换源走 `g.cli`，清单漏了 | 补进统一闸口 |

D6 的四段顺序必须完整复刻：

1. userinfo 一律拒绝
2. 同 Host 且同 scheme 放行
3. 否则查 `DefaultRealmsFor(host)`（Artifact Registry 动态 `https://<host>`）
4. 保留通配分支；现网静态表没有通配命中，但 11a 不能改变行为

11a 落地清单：

- `PolicySnapshot`，在候选选中点写入 attempt ctx，作为唯一身份入口
- `RealmPolicyResolver` 接口 + `LegacyHostPolicy`
- `v2auth.Authenticator.Do` 改为 `(sourceID, host)`；RealmAllowed 收敛到 resolver
- 统一构造函数覆盖 `doUpstream` / `headOnce` / `rangeReq` / `openUpstream` / resume / `watch.go`
- `tokenKey` 结构体 + 自研 singleflight + `WithoutCancel`
- 删除 Gateway 侧 `parseBearer` / `splitParams`
- 增加 `identity_missing` 指标，作为 11b 门禁，不做运行时开关
- Probe 的 `CredentialSnapshot` 不动；`Realms` map 改为 resolver
- fail closed 使用 503 + 明确错误码，不伪装 401
- Token Cache 容量改为配置项，默认至少 1024；淘汰按过期 + LRU，不按 generation 排序

11a 明确不做：

- 不建 `source_realm_policy`
- 不做 `db-migrate`
- 不做策略 UI/API
- 不改 `private.go`
- 不改 Probe 的 `CredentialSnapshot` 字段
- 不做 `SetCapsLive` / `CommitHostProbe`（属于 3.5.14a）
- 不保留第二套 Realm 运行时入口

11a 验收补充：

- `host_prefix` 的 scope 不再带 Host 段
- `watch.go` 换源路径带身份
- Probe/Gateway 对同一 Source/Host 结果一致
- 身份缺失返回 503，不返回伪造 401
- singleflight leader 取消不影响等待者
- Token Cache 扩容后不因多 Host 源挤空其他源

#### 3.5.11-H 11a 复核：可疑点与验收状态

复核时代码提交：`73d42bf`（`feat/3.5.11a`）。以下记录不能再被“测试通过”掩盖，尤其是未达验收项。

已补齐并可复核：

- `withRegistryPolicy` 的 nil plan、resolver 失败、Registry 缺 `LogicalHost` 均有单测；缺身份返回 `ErrPolicyMissing` / 503。
- `bearerTransport` 的无 Realm、坏 Realm、token 刷新失败分支有单测。
- `TestCandidateRegistryTokenUsesSourceIdentity`：真实 Gateway 路由 + scheduler 候选源，断言 SourceID、token 刷新和候选 Authorization。
- `TestFallbackToGhcrCarriesCandidateIdentity`：主上游 404 后 fallback 到 ghcr 候选，断言候选 SourceID/LogicalHost/Token。
- `TestProbePolicyReceivesSourceID`：Probe 的真实策略调用收到候选 SourceID。
- Probe 默认策略与 Gateway 统一为 `AllowSameHost=true`；新增 `TestHostProbeDefaultPolicyAllowsSameHost`。
- `withRegistryPolicy` 现在按实际 target URL 的 scheme 写入快照。
- Token Cache 增加 `auth_cache_hits_total`、`auth_cache_misses_total`、`auth_cache_evictions_total`、`auth_cache_entries`、`auth_cache_capacity`。

仍未闭环/仍需决策：

1. **`CredGen` / `PolicyGen` 在 11a 恒为 0**
   - Token Cache 已按 Source/Host/Realm/Service/Scope 隔离，但尚未按用户凭据代次隔离。
   - 3.5.15 前必须填真实 generation，否则轮换凭据只能靠进程重启或全量清缓存。

2. **`Plan.Registry` 由适配器自行置位**
   - 当前 Registry 适配器已置 `true`；未来新增 Registry 路径若忘记置位，路由级标记只会让它 fail closed 成 503，不会在 plan 阶段报错。

3. **Token Cache 容量不支持热更新**
   - `registry.token_cache_max` 只在启动时读取；改配置需重启。容量上限目前仍是 1024，实际多 Host 源的压力需用新增指标观察。

4. **`scopeFromPath` 的同名 Host/Repo 歧义**
   - 路径 `/v2/<host>/...` 会按 `LogicalHost` 剥前缀；如果仓库第一段刚好等于 Host，语义无法自动区分。当前 host_prefix 约定下可接受，但不能当成通用解析器。

5. **候选源覆盖仍是本地假源**
   - 候选/fallback 的认证用例使用 httptest 假源，验证的是代码路径，不等于真实公共 Source 的可用性验收。

11a 目标核对：

- 所有 Registry 请求有身份或明确 fail closed：**基本达到**；合法 plan 都写快照，缺 plan 走路由标记 + 503。
- HEAD / 分片 / 续传 / 换源 / 并行不丢身份：**代码路径已覆盖**，现有路径测试加新增 fallback 用例验证；head/换源尚无专门的认证断言，仍需真实候选网络样本。
- 缺身份 fail closed：**达到**（503 + `auth_policy_missing`）。
- Realm 判定：Gateway/Probe 已统一 `AllowSameHost=true`；Gateway 保持 3.5.10 行为，Probe 对“同 Host 但不在静态白名单”的 Realm 由拒绝改为放行，属于有意收口，11b 的 `StorePolicy` 需再复审。
- Token key 区分 Source：**达到**，有单测。
- Token Cache 容量配置：**达到**（默认 1024，YAML 可配）。
- `go test -race` 并发刷新：**达到**。
- 不建 `source_realm_policy`、不碰 Private、不改 `CredentialSnapshot`、不做 11b/14a：**达到**。

结论：选择 A 后，Probe/Gateway 的统一语义已落地，11a 的主体实现和可验证项完成；剩余的 `CredGen/PolicyGen=0`、`Plan.Registry` 置位、Token Cache 热更新和真实公共 Source 覆盖均属 11b/后续验收范围。

### 3.5.12 合并验收 + 定点修复（原 3.5.12 + 3.5.13）

> **实施状态（2026-11-16）：** 12c、12-F1、12-F2、12-T 已实现并通过自动化与 Docker 模拟验收，记录见 `docs/3.5.12-acceptance.md`。12-S/12a/12b 未执行（`NOT_EXECUTED`）：没有创建 Host Source、没有配置 `ML_HOSTPROBE_REPOS`、没有收窄生产白名单、没有实际探测与镜像尝试，不能标记为 `BLOCKED_EXTERNAL_SOURCE`。生产切换未授权、未执行。

**定位：** 本版不是纯验收版本，也**不改 Schema**。它合并原 3.5.12（GCR / Artifact Registry / K8S）与原 3.5.13（Quay / MCR / GitLab / ECR / cgr + 同 Host 多 Source），并包含 3.5.11c 欠账、两项已确认代码修复、Source 就绪门禁和候选→回退链路测试。原 3.5.13 编号退役，不单独发布，3.5.14 及后续版本不改号。

#### 3.5.12-A 生产基线（2026-11-16 实测，冻结）

| 项目 | 实测结果 |
|---|---|
| 生产镜像 | `mirror-lens:3.5.11` |
| 容器状态 | Up，`/healthz` OK |
| `schema_version` | 3.5.11 |
| `schema_hash` | `e39e11971963…` |
| `source_realm_policy` | 36 行（`docker.io` 34 + `ghcr.io` 2） |
| SQLite integrity | ok |
| 生产 Source 分布 | `docker.io` 34（`registry_cap=-1`）、`ghcr.io` 2（`registry_cap=1 native`）、其余 8 个 Host 均为 0 |
| 发布前备份 | DB + `secret.key`，目录存在 |
| 多源白名单 | 生产 `config.yaml` 未显式配置 `multi_origin_hosts`，使用默认 8 个 Host 全开 |

基线还必须记录：生产镜像 ID 或不可变 digest、`config.yaml` 的 SHA256、备份 DB 与 `secret.key` 各自的 SHA256。后续数据库变更审查以这份实测状态为起点，不以 Git 标签推断部署状态。

#### 3.5.12-B 工作项与执行顺序

| 顺序 | 工作项 | 内容 | 完成条件 |
|---|---|---|---|
| 1 | 12c | Source 级 Realm 策略 API/UI、Host 只读汇总、审计、`LegacyHostPolicy` 清理 | 不改 Schema；独立提交、独立回滚；行为覆盖无净损失 |
| 2 | 12-F1 | 探测执行入口统一 Host 匹配（仅 `jobs()` 入口） | 关闭并重启后，该 Host 的 recovery/健康行不再更新 |
| 3 | 12-F2 | 调度缓存 0 窗口验证（已接线，补测） | 修改返回后的下一次调度即读新顺序 |
| 4 | 12-S | 先收窄白名单，再逐 Host 完成 Source 就绪 | 每 Host ≥1 源，探测记录与 digest 齐备 |
| 5 | 12-T | 候选→排序→回源→失败切换链路测试 | digest 一致，不只验证第二个源收到请求 |
| 6 | 12a | GCR / `registry.k8s.io` / 具体 Artifact Registry Host 真实回源 | 真实 digest 证据 |
| 7 | 12b | Quay / MCR / GitLab / ECR / cgr + 同 Host 多 Source | 真实回源与受控回退分开记录 |
| 8 | 发布门禁 | 四态分类与阻塞清单 | 每条记录可复核 |
| 9 | 发布 | 3.5.12 | 阻塞项不伪装成通过 |

**门禁粒度：** 12-S 按 Host 生效，不是全局门禁。`12-S(host)` 未通过，不得启动该 Host 的 12a/12b 真实回源验收；无公共 Source 的 Host 记录“尝试镜像清单 + 失败证据”后标记“外部 Source 阻塞”，不阻塞其他 Host 与整体发布。

#### 3.5.12-C 12c：3.5.11c 欠账（Source 级策略管理）

原 3.5.11c 计划未实施（3.5.11 以 11b 验收为完成标志），现调整并入 3.5.12。**不引入 Host 级策略模型，不改 Schema。**

管理对象分三层，数据来源不同，不能混成一张可编辑的“Host 策略”表：

| 管理对象 | 数据来源 | UI 行为 |
|---|---|---|
| Host 启用状态 | `registry.multi_origin_hosts` | 只读展示 |
| Realm 策略 | `source_realm_policy` | 按 Source 编辑 |
| Source 数量 | `sources` | 只读汇总 |
| 策略审计 | `source_events` | 查询展示 |

实现内容：

- 新增 Source 级 Realm 策略 API：按 Host 列出 Source 及其策略、读取单条、更新（复用 `SetRealmPolicy`）、查询审计
- 新增 Source 级策略编辑 UI；新增 Host 只读汇总页（Host、Source 数、Realm 摘要、多源开关状态）
- `multi_origin_hosts` 仍由 `config.yaml` 管理，UI 不提供修改
- 不存在“策略启用/停用”字段；停用语义为**删除策略行**，运行时必须 fail closed（`ErrPolicyMissing`），不能退回不受约束的 Realm
- 删除 `LegacyHostPolicy` 死代码；8 个测试文件的相关断言迁移为 `StorePolicy` 行为断言，不得直接删测试
- 旧 Realm 配置读取路径已确认不存在，无需迁移配置

鉴权边界：当前是**单 admin 账号模型**（`internal/auth`），没有角色/权限分级。验收只能要求“未登录或无 CSRF 的请求被拒绝”，不能写“已登录但无权限”的用例；若需要分级权限，属独立版本的新功能。

验收：Schema 版本与 Hash 不变；策略增删改查正确；未登录/无 CSRF 拒绝；删除策略后 fail closed；审计完整；旧测试覆盖无净损失；独立提交、独立测试、独立回滚。

#### 3.5.12-D 12-F1：Host 关闭隔离

已确认缺口：`multi_origin_hosts` 只在 Gateway 选路读取；`ListDueHostProbes`（`internal/store/host.go:495`）不检查该配置，关闭 Host 后探测仍会运行并写入 `registry_host_recovery` / 健康记录。

范围修正：**只有 `jobs()` / `ListDueHostProbes` 是探测执行入口**。`reconcileRepos` 只在代表仓库映射变化时清理 `registry_cap/path_mode`，不执行探测、不写健康状态，保持原职责，不加入白名单过滤。

修复与验收：

- 抽取共享 Host 匹配函数，供 Gateway 与 Runner 共用；规则必须同时支持精确匹配与 `*.pkg.dev` 的 Artifact Registry 规则（`registry.go:383-391`），不能字符串相等，否则 `asia-east1-docker.pkg.dev` 会被误拦
- 过滤点在 Runner 取得任务之后、执行探测之前
- 关闭 Host → 重启容器（`registry.multi_origin_hosts` 为 `Restart: true`）→ 等待至少两个扫描窗口 → 断言该 Host 无探测执行、`registry_host_recovery` 与健康记录无新增或更新
- 重新启用 → 重启 → 断言探测恢复
- 关闭 foreign 多源不得误伤官方 Registry 的基础回源与健康检查
- 不以“日志无探测字样”作为唯一证据；以数据库行未变化为主，必要时补探测跳过计数指标
- 不要求 `reconcileRepos` 在关闭 Host 后停止清理能力状态；改变该语义需另立需求

#### 3.5.12-E 12-F2：调度缓存 0 窗口失效

已核对现状（2026-11-16 修正）：`main.go:396` 已接线 `eng.OnChange(sched.Invalidate)`；源与分组的启停、优先级、分组、删除、归档入口均调用 `s.eng.Changed()`（`internal/api/api.go:698/729/750/783/791/873/886`），删除另有 `Forget`。因此本项**不是代码修复**，而是 0 窗口验证与补测；若补测发现遗漏入口，再按缺陷修复。

验收目标为 0 窗口：

- 覆盖启用、停用、优先级、分组、删除、归档
- `repo_host` 修改当前没有 API（`SetRepoHost` 仅为 Store 方法），不纳入本版验收，留到 3.5.14 统一设计
- 通知链必须在持久化成功后、API 返回前完成 `InvalidateLists()`，不能异步延迟
- 修改操作返回后开始的调度请求，必须读到新顺序；并发场景用同一断言
- 修改前已取得旧列表的在途请求，不要求中途切换
- 不得依赖 `ListTTL=30s` 自然过期

#### 3.5.12-F 12-S：Source 就绪（12a/12b 前置门禁）

生产实测：8 个目标 Host 当前 Source 数均为 0。`docker.io` 的 `registry_cap=-1` 不得解释为 foreign Host 能力；3.5.14 之前一个 Source 只能绑定一个 `repo_host`，不能用春川（Source ID 638）替代独立源行。

**生产灰度顺序：先收窄白名单，再建源。** 生产 `config.yaml` 未显式配置 `multi_origin_hosts`，默认 8 个 Host 全开；先建源再验收不构成灰度。

1. 冻结基线（镜像、配置、DB、密钥、Schema、Policy、Source）
2. 在 `config.yaml` 显式写 `multi_origin_hosts: ["gcr.io"]`，按现有结构合并，不覆盖其他 Registry 配置
3. 重启容器，确认只有 `gcr.io` 进入多源候选范围
4. 建 GCR Source（`repo_host=gcr.io`），配置 `ML_HOSTPROBE_REPOS`，重建容器使环境变量生效
5. 等待自动探测 → `registry_cap=1` → 候选非空 → 真实回源并记录 digest
6. 通过后每次只追加一个 Host 并重启；不得第一次就恢复 8 个全开

| Host | 当前生产 Source | 12-S 最低要求 |
|---|---|---|
| `gcr.io` | 0 | ≥1 |
| `registry.k8s.io` | 0 | ≥1 |
| 具体 `<location>-docker.pkg.dev` | 0 | ≥1 |
| `quay.io` | 0 | ≥1 |
| `mcr.microsoft.com` | 0 | ≥1 |
| `registry.gitlab.com` | 0 | ≥1 |
| `public.ecr.aws` | 0 | ≥1 |
| `cgr.dev` | 0 | ≥1 |

- `<location>-docker.pkg.dev` 必须写具体三段式 Host（如 `asia-east1-docker.pkg.dev`）；`*.pkg.dev` 只用于 `multi_origin_hosts` 匹配，不能作为 `repo_host`
- 每条源必须 `repo_host` 正确，探测后 `registry_cap=1` 且 `registry_path_mode` 合法
- 探测代表仓库必须配置 `ML_HOSTPROBE_REPOS`（`host=repo`，逗号分隔）；未配置映射的 Host 会被跳过，`registry_cap` 永远升不到 1。生产容器当前未设置该变量，12-S 必须补齐并重建
- 由 `ListDueHostProbes` 自动调度探测，等待真实探测记录，不以手工触发替代
- 需要验证多 Source 回退的 Host 至少准备两个独立源；受控假上游不计入公共 Source 数量
- 记录每条源的探测结果与真实镜像 digest；无公共源的 Host 记录尝试清单与失败证据
- `ML_HOSTPROBE_REPOS` 的仓库必须实测存在、允许匿名拉取，并固定 tag 或 digest；试点阶段只需试点 Host 的映射，其余随 Host 逐个追加，不要为凑齐 8 个映射阻塞试点

| 12-S(host) 状态 | 进入真实回源验收 | 发布处理 |
|---|---|---|
| 通过，≥1 个真实源 | 是 | 可验收单源回源 |
| 通过，≥2 个真实源 | 是 | 可验收真实多源回退 |
| 通过，仅 1 个真实源 | 是 | 多源回退只能模拟 |
| 无真实源，有尝试清单与失败证据 | 否 | 标记“外部 Source 阻塞” |
| 探测仓库未映射或探测未执行 | 否 | 12-S 未完成，不得标外部阻塞 |

#### 3.5.12-G 12-T：候选到回退链路测试

沿用 `internal/gateway/` 已有的 `httptest.NewServer` 设施，不引入新基础设施。每个 Host 使用固定 repo/digest fixture，验证：候选集合 → scheduler 排序 → Gateway 实际请求 → 首选失败 → 下一 Source 回退 → digest 一致。失败切换后必须验证内容身份一致，不能只验证第二个源收到了请求。

场景至少覆盖：正常首选、首选 5xx 回退、首选超时回退、多源均失败、两源 digest 不一致、优先级变化、Host 关闭、Source 停用。

失败缓存断言（不能只看 HTTP 返回码）：

- 固定请求路径、方法、仓库与 tag/digest，用 `plan.CacheKey` 取得实际缓存 Key，测试不得自行实现一套 Key 算法
- 两个 Source 均失败后，断言响应失败，且 `CacheGet(plan.CacheKey)` 查不到有效条目（`internal/store/accel.go:328`）
- 确认没有错误响应被写入正向缓存；再次请求不得命中上一次失败内容
- 不能只检查缓存目录为空；索引、临时文件与正式对象不是同一层

保留现有流式传输、断点续传、hedge 和响应头处理的回归测试，不得因多源修复重新引入内容混合或取消上下文导致的中断。

#### 3.5.12-H 12a / 12b Host 验收矩阵

| Host | 生产 Source 现状 | 验收要求 | 阻塞分类 |
|---|---|---|---|
| `gcr.io` | 0 | 真实回源 + 多源回退（源数由 12-S 决定） | Source 待接入 |
| `registry.k8s.io` | 0 | 真实回源 + Realm | Source 待接入 |
| `<location>-docker.pkg.dev` | 0 | Host 准入 + 官方故障边 | Source 可用性 |
| `quay.io` | 0 | 真实回源 + 多源回退 | Source 待接入 |
| `mcr.microsoft.com` | 0 | 真实回源 + 多源回退 | Source 待接入 |
| `registry.gitlab.com` | 0 | Realm + 认证回退 | Source 覆盖有限 |
| `public.ecr.aws` | 0 | Realm + 认证回退 | Source 覆盖有限 |
| `cgr.dev` | 0 | 官方故障边 | Source 可用性 |
| `lscr.io` | 范围外 | 不验收 | 范围外 |

真实多源回退范围由 12-S 的实测源数决定：只有 ≥2 个真实公共源的 Host 才做真实回退，其余用受控上游验证机制并标注“模拟”。一个 Host 的结果不得外推到其他 Host，候选查询成功也不等于多 Source 实际回退成功。

#### 3.5.12-I 发布门禁

| 状态 | 含义 | 发布说明 |
|---|---|---|
| 真实回源通过 | 功能与公共加速均已验证 | 可声明支持 |
| 代码路径通过、无可用公共 Source | 软件路径已验证，公共加速不可用 | 标记“Source 可用性阻塞” |
| 仅模拟/故障注入通过 | 验证了控制逻辑，无真实回源证据 | 不得声明真实加速通过 |
| 代码路径不通过 | 存在实现缺陷 | 阻止对应 Host 验收 |

每条验收记录至少包含：`source_id`、`host`、测试仓库、digest、完整可复现命令、执行时间、结果。

发布必须项：

- 12c、12-F1、12-F2、12-T 完成
- 每个纳入验收的 Host 有探测仓库映射，且探测实际执行
- 真实回源 Host 至少 1 个真实公共 Source；声称真实多源回退的 Host 至少 2 个
- 无公共源的 Host 有尝试清单与失败证据
- 模拟测试与真实回源测试分开记录
- Docker Hub / GHCR 回归通过
- Schema 版本与 Hash 不变（本版不改 Schema），Policy 行数变化与新增 Host 对应
- 数据库完整性检查与备份校验通过

#### 3.5.12-J 不做

- 不重写 3.5.8 已完成的候选调度
- 不引入 `source_host_caps`，不提前做一源多 Host
- 不让一条 Source 服务多个 Host
- 不启用第一批之外的 Host
- 不把没有公共 Source 的 Host 标成“代码未实现”
- 不把模拟测试结果等同于真实公共源验证

### 3.5.13 编号退役

原 3.5.13（第一批剩余 Host + 同 Host 多 Source 正式验收）已整体并入 3.5.12，不单独发布。编号保留为空，不复用；3.5.14 及后续版本不改号。原 3.5.13 的 Host 矩阵、验收要求和阻塞分类已迁入 3.5.12-H。

### 3.5.14a `source_host_caps` 回填 + 旧字段投影 + 新写路径

**目标：** 建立统一能力表、完成确定性回填、把运行时写路径切到“权威表 + 投影”双写，但**不切换读路径**。线上候选集、标签和调度行为保持不变。

#### 3.5.14a-A 数据模型

```text
source_host_caps
  source_id
  host
  enabled
  probe_status
  registry_cap
  registry_path_mode
  registry_prefix
  last_supported_at
  registry_probe_at
  registry_probe_error
  PRIMARY KEY(source_id, host)
  FOREIGN KEY(source_id) REFERENCES sources(id) ON DELETE CASCADE
```

索引至少包含：

```sql
CREATE INDEX idx_source_host_caps_host
ON source_host_caps(host, probe_status, registry_cap);
```

字段语义：

- `enabled`：用户是否允许该 Source 服务此 Host
- `probe_status`：最近一次探测结论，四态：
  - `Supported`
  - `Unsupported`
  - `Inconclusive`
  - `Mismatch`
- `registry_cap`：能力值，必须与 `probe_status` 保持一致：
  - `1` ↔ `Supported`
  - `0` ↔ `Unsupported`
  - `-1` ↔ `Inconclusive`
  - `2` ↔ `Mismatch`
- `registry_path_mode`：已验证的路径模式（`native` / `host_prefix`）
- `registry_prefix`：GHCR 等前缀路径标志，兼容旧 `ghcr_prefix`；布尔
- `last_supported_at`：最近一次确认可用
- `registry_probe_at` / `registry_probe_error`：最近探测事实

`Mismatch` 不是普通 `Unsupported`：

- `Mismatch` 表示 Manifest 内容与官方不一致，必须硬排除
- 不能按 6 小时普通不支持重试
- 恢复策略与现有 `RecoveryHardExcluded` 一致

#### 3.5.14a-B 回填规则

对每个现有 Source：

1. `repo_host` 非空：
   - 写入 `host=repo_host`
   - `registry_cap=registry_cap`
   - `registry_path_mode=registry_path_mode`
   - `probe_status` 按 `registry_cap` 映射：`1→Supported`、`0→Unsupported`、`-1→Inconclusive`、`2→Mismatch`
2. `docker_cap`：
   - 写入或合并 `host=docker.io`
   - `registry_cap=docker_cap`
   - `registry_path_mode=native`
   - `probe_status` 映射：`1→Supported`、`0→Unsupported`、`-1→Inconclusive`
3. `ghcr_cap`：
   - 写入或合并 `host=ghcr.io`
   - `registry_cap=ghcr_cap`
   - `registry_path_mode=native`
   - `registry_prefix=ghcr_prefix`
   - `probe_status` 映射：`1→Supported`、`0→Unsupported`、`-1→Inconclusive`
4. 同一 Source 的同一 Host 只能有一行；合并时冲突必须 fail closed，不能静默覆盖。
5. 旧字段不删除；由同一事务生成投影。

Docker / GHCR 的 `probe_status` 映射必须显式实现，不能只写 `docker_cap` / `ghcr_cap`。否则 14b 的候选准入会与旧行为不一致。

冲突处理：

- `host=docker.io`：`docker_cap` 优先；`registry_cap` 只在 `docker_cap=-1` 时参考
- `host=ghcr.io`：`ghcr_cap` 优先；`registry_cap` 只在 `ghcr_cap=-1` 时参考
- 其他 Host：`registry_cap` 优先
- 两个已知值冲突且无法按上述优先级消解：fail closed，输出 `source_id` / `host` / 两个值，人工处理，不自动猜
- 不能静默选一个值继续迁移

#### 3.5.14a-C 写入边界

- 迁移工具只做一次性回填。
- 运行时 Store 只允许写 `source_host_caps`，并在同一事务内生成旧字段投影。
- API 不允许直接写 `repo_host` / `registry_cap` / `registry_path_mode` / `docker_cap` / `ghcr_cap` / `ghcr_prefix`。
- 旧字段对 API / 调用方只读；仅 Store 在同一事务内生成投影时允许写。
- 旧字段不能成为第二个权威入口。
- 增加一致性测试：随机生成多 Host Source，写入后断言新表与旧投影一致。

3.5.14a 必须包含新写路径，不能只建表。这一节是 3.5.14a 的运行时单事务门禁，不属于 3.5.11 的实现范围：

总则：任何一次运行时写入，只要影响多张表，就必须在同一个 `BEGIN IMMEDIATE` 事务内完成；不允许一张表成功、另一张表延后。只有 Token Cache 和离线迁移是例外。

1. `SetCapsLive`（`internal/store/store.go:614`）
   - 当前是无事务单条 UPDATE，直接写 `docker_cap` / `ghcr_cap` / `ghcr_prefix`
   - 改为 `BEGIN IMMEDIATE`
   - 写 `source_host_caps` 的 `docker.io` / `ghcr.io` 行
   - 同一事务生成旧字段投影
   - 保留 `status<>'archived'` 条件和 `RowsAffected` 语义
   - 失败整体回滚

2. `CommitHostProbe`（`internal/store/commit.go:68`）
   - 已经是 `BEGIN IMMEDIATE` 事务
   - 继续只处理 Foreign Host，继续拒绝 `ghcr.io`
   - 扩展为同时写 `source_host_caps` 和旧 `registry_cap` / `registry_path_mode`
   - 失败整体回滚

3. `SetCaps` / `SetHostCaps`
   - 不恢复为生产入口
   - `SetHostCaps` 保持 deprecated
   - 生产路径只允许 `SetCapsLive`（Docker/GHCR）和 `CommitHostProbe`（Foreign）

4. 事务内必须同时完成：
   - 权威表 `source_host_caps`
   - 旧字段投影
   - 必要的 `source_health_scope` / `registry_host_recovery`
   - 任一步失败整体回滚

5. 不允许：
   - 先写权威表、后写投影
   - 先写投影、后写权威表
   - Docker/GHCR 走一条无事务 UPDATE，Foreign 走事务
   - 在 14a 引入旧读路径的 dual-read 开关

#### 3.5.14a-D 验收

- 迁移前后 `SourceCandidates("docker")` 候选集 diff 为空
- 迁移前后 `SourceCandidates("ghcr")` 候选集 diff 为空
- 迁移前后 `SourceCandidates("registry:<host>")` 候选集 diff 为空
- 首页 HUB/GHCR/RAW/foreign 标签不变
- 双用途源（`repo_host=docker.io` + `ghcr_cap=1`）能力不变
- 旧单 Host Source 行为不变
- `registry_cap=2` 回填为 `probe_status=Mismatch`
- `docker_cap=-1/0/1` 与 `ghcr_cap=-1/0/1` 的 `probe_status` 映射正确
- Docker / GHCR 与 `registry_cap` 冲突按优先级消解；无法消解时 fail closed
- `SetCapsLive` 失败时权威表和投影都不落库
- `CommitHostProbe` 失败时权威表和投影都不落库
- 迁移失败不发布目标文件；灾难恢复使用备份库和旧 `secret.key`

#### 3.5.14a-E 能力审计

3.5.14a 的能力审计不切换读路径；回填和双写见前文：

- 枚举所有 enabled Source
- 对每个 Source 按 `host_prefix` 和 `native` 尝试候选 Host
- 只有实际 Manifest 可读且 Digest 可验证，才记 `probe_status=Supported`
- 认证失败、网络错误、仓库状态不确定记 `Inconclusive`
- 固定测试仓库确认路径模式不支持才记 `Unsupported`
- Manifest 内容与官方不一致记 `Mismatch`
- 记录 `source_id`、`host`、`path_mode`、`probe_at`、`probe_error`、`last_supported_at`
- 不从 `base_url` 推断，不从站点名称推断，不手工补标签
- 产出“多 Host Source 清单”
- `Inconclusive` 使用独立冷却和重试策略，不能长时间占用探测配额

#### 3.5.14a-F 离线迁移

3.5.14a 实施时的生产基线是 `3.5.11`（2026-11-16 实测）；3.5.11 新增 `source_realm_policy` 后 Schema 已提升。迁移链从 `3.5.7` 起保留给旧库，因此不是简单的一条 `3.5.7 → 3.5.14a`：

```text
3.5.7  → 3.5.11  → 3.5.12  → 3.5.14a
         Realm 表   验收（不改 Schema）  Host 能力表
```

当前 `db-migrate` 不能直接迁移这条链：

- `inputSchemaFingerprints` 只接受 `3.4.6 / 3.5.4 / 3.5.5`
- `upgrade356` 只处理 `3.5.6 → 3.5.7`
- 没有 3.5.7、3.5.11、3.5.12 的输入路径

因此 3.5.11 和 3.5.14a 都必须各自提供离线升级路径：

- 3.5.11：`3.5.7 → 3.5.11`，新增 `source_realm_policy`，按 `repo_host` 种子化并生成缺失策略报告
- 3.5.14a：`3.5.12 → 3.5.14`，新增 `source_host_caps` 并回填
- 这一步同时校验：没有策略的 Source 必须进入报告并在运行时 fail closed，不能回退旧白名单
- `copyAll` 不会自动生成 `source_realm_policy`；如果只复制旧表，升级后所有自定义 Host 都会认证失败
- 每一步都更新输入 Schema 指纹
- 更新 `cmd/db-migrate/main_test.go` 中“只允许三个输入指纹”的断言
- 迁移输入只读，输出临时库
- 每一步提升 `schema.Version` 并重新冻结 `CanonicalSchemaHash`

迁移顺序：

```text
1. 打开输入库只读，校验 schema_meta / fingerprint
2. 在临时输出库执行 Canonical DDL
3. copyAll 复制旧表
4. backfillHostCaps 生成 source_host_caps
5. 校验 source_host_caps 行数、冲突、双用途源
6. 计算 schema.Hash，finalizeTargetMeta
7. schema.Verify
8. 关闭临时库，清理 sidecar
9. publishNoClobber 原子发布
```

`backfillHostCaps` 不能省。`copyAll` 只复制旧库已有表，`source_host_caps` 是新增表，复制完一定是空表。漏掉这一步，14b 切过去后候选集会为空。

迁移失败边界：

- 任一步失败，删除临时库和 `-wal` / `-shm`
- 不发布目标文件
- 不保留半迁移数据
- 备份库和旧 `secret.key` 只作为灾难恢复，不作为常规回滚路径

准确表述是：

> 离线迁移 + 原子发布，失败不留半迁移状态。

不要写成“单事务迁移”。当前 `db-migrate` 是多阶段提交：

- `EnsureFresh` 一次提交
- `copyAll` 另一阶段
- `finalizeTargetMeta` 再更新

如果要求字面单事务，需要重构 `db-migrate`，这是额外工作，不能假设已经成立。

3.5.14b 不改 Schema、不改 `schema_meta`，保证 3.5.14a 镜像可以接管回滚。

### 3.5.14b 只切换 `source_host_caps` 为权威读路径

**目标：** 让 `source_host_caps` 成为所有逻辑 Registry Host 的能力权威，并切换全部读路径；Docker Hub / GHCR 不再例外。

#### 3.5.14b-A 权威关系

| 数据 | 3.5.14b 之后的职责 |
|---|---|
| `source_host_caps` | 所有 Registry Host 的能力权威 |
| `sources.repo_host` | 主 Host / 旧版本兼容投影 |
| `sources.registry_cap` | 主 Host 的旧版能力投影 |
| `sources.registry_path_mode` | 主 Host 的旧版路径投影 |
| `sources.docker_cap` | Docker 协议能力的兼容投影 |
| `sources.ghcr_cap` | GHCR 协议能力的兼容投影 |
| `sources.ghcr_prefix` | GHCR 前缀能力的兼容投影 |
| `registry_host_recovery` | 按 Source + Host 维护恢复状态 |
| `source_health_scope` | 按 Source + Host 维护健康证据 |
| `registry_host_gen` | Host 级凭据代次 |

约束：

- 新查询全部以 `source_host_caps` 为准
- 旧字段不能再独立决定候选资格
- 兼容字段只允许由权威数据产生
- 写入必须在同一事务内完成
- 不能出现新表与旧字段各自维护、互相覆盖
- Docker Hub / GHCR 不享受例外路径，必须有迁移和回归测试
- GHCR 的 `ghcr_prefix` 必须映射到 `source_host_caps.registry_prefix`，路径改写以新字段为准

#### 3.5.14b-B 候选准入

候选准入必须同时满足：

```text
source.enabled
AND source_host_caps.enabled
AND source_host_caps.probe_status = 'Supported'
AND source_host_caps.registry_cap = 1
AND source_host_caps.registry_path_mode IS NOT EMPTY
AND 健康状态允许
AND 恢复状态有效
```

探测四态：

| 结果 | 含义 |
|---|---|
| Supported | Manifest 成功，Digest 通过 |
| Unsupported | 固定测试仓库确认该路径模式不支持 |
| Inconclusive | 认证、网络、仓库状态不确定 |
| Mismatch | Manifest 内容与官方不一致，硬排除 |

规则：

- `Inconclusive` 不得覆盖之前的 `Supported`
- `Mismatch` 必须硬排除，不能按普通 Unsupported 重试
- `last_supported_at` 保留最近一次有效能力结论
- 候选准入必须同时满足 `probe_status='Supported'` 和 `registry_cap=1`
- Docker/GHCR 行也必须满足同一映射，不允许只写 `docker_cap` / `ghcr_cap`

#### 3.5.14b-C 必须修改的读路径

- `SourceCandidates("registry:<host>")`
- `registryCandidatesRaw()`
- `ScopedHealthOf()`
- `registry_host_gen` 关联
- Docker 候选查询
- GHCR 候选查询
- `SourceStat.RegistryPath`
- Gateway 按 Host 获取候选和路径模式
- Scheduler 按 Host 隔离候选、健康和熔断
- Probe 按 Source + Host 探测
- UI 读取所有已验证 Host 能力
- Manifest 缓存身份不跨 Host 混用
- 新旧能力变更后候选缓存正确失效

具体代码落点：

- `internal/store/sources.go:147`：`ScopedHealthOf` 不能再 `JOIN sources src ON src.repo_host = g.host`，改为按 `z.scope` 直接关联 `registry_host_gen`
- `internal/store/sources.go:244-254`：Docker / GHCR 候选查询不能再依赖 `repo_host` 和 `s.ghcr_prefix`，改为读 `source_host_caps` 的 `docker.io` / `ghcr.io` 行
- `internal/store/host.go:462-464`：可以继续把 `docker.io` / `ghcr.io` 排除在 Foreign Host Probe 之外，但它们的能力行必须由 `SetCapsLive` 写入 `source_host_caps`
- `internal/scheduler/scheduler.go:956`：GHCR 路径改写读 `source_host_caps.registry_prefix`，不再读 `sources.ghcr_prefix`
- Docker/GHCR 的 health family 不是 `registry`，不能套用 `registry_host_gen` 的代次规则

特别是 `ScopedHealthOf` 不能再用：

```sql
JOIN sources src ON src.repo_host = g.host
```

必须按健康记录的 `scope` 查对应 Host 的代次。

#### 3.5.14b-D 并发与缓存边界

| 维度 | 隔离方式 |
|---|---|
| Registry 能力 | Source + Host |
| 探测结果 | Source + Host |
| 失败及熔断 | Source + Host |
| 冷却 | Source + Host |
| 路径模式 | Source + Host |
| Realm 策略 | Source + Host |
| 实际回源并发 | Source 级共享 |
| 物理 Source 生命周期 | Source 级共享 |
| Manifest 缓存 | 逻辑 Host 隔离 |
| Blob 缓存 | 内容摘要共享 |

同一物理 Source 的 Docker / GHCR / GCR / K8S 请求可能落到同一服务端，不能按 Host 各开一份并发上限。

#### 3.5.14b-E 迁移与切换验收

- 备份数据库和 `secret.key`
- 14b 不改表、不改 `schema_meta`
- 旧 Source 的 `source_host_caps` 回填结果保持
- 双用途源同时保留 Docker 和 GHCR 能力
- `repo_host` 主 Host 投影正确
- `source_health_scope.scope` 保持原 Host 归属
- `registry_host_recovery.scope` 与新能力记录一致
- `registry_host_gen` 继续使用 Host 级代次
- 旧代次成功不当作新代次成功
- 迁移前后候选集 diff 为空
- 迁移后 HUB/GHCR 标签不变
- 旧单 Host Source 行为不变
- 回滚演练通过

迁移测试必须包含：

- 普通 foreign Source
- 双用途 Source（`repo_host=docker.io` + `ghcr_cap=1`）
- 空 `repo_host` Source
- 显式 GHCR Source
- 有 `ghcr_prefix=0/1` 的 Source
- `registry_cap=2` / `probe_status=Mismatch` 的 Source
- `docker_cap=-1/0/1` 与 `ghcr_cap=-1/0/1` 的映射
- 已过期探测成功记录
- 旧代次健康记录
- 有 recovery 行和无 recovery 行
- 3.5.7 输入库的 `db-migrate` 迁移
- `backfillHostCaps` 前后行数和冲突校验
- 迁移前后候选集 diff 为空

#### 3.5.14b-F 春川样本验收

- 春川可保存 `docker.io`、`ghcr.io`、`registry.k8s.io`、`gcr.io`
- 首页只显示实际探测通过的标签
- K8S 请求实际命中春川，而不是只显示标签
- 关闭春川 K8S 能力不影响 Docker / GHCR
- Docker / GHCR / K8S / GCR 的健康、冷却、失败互不污染

#### 3.5.14b-G 只读能力矩阵（P1）

- UI 增加 Source × Host 的只读能力矩阵
- 展示 `enabled`、`probe_status`、`registry_path_mode`、`last_supported_at`、`probe_error`
- 只读，不提供手工改标签或手工改能力
- 用于运维核对春川这类多 Host Source

#### 3.5.14b-H 单轨与回滚

3.5.14b 的发布纪律：

- 只包含新读路径
- 不包含旧读路径
- 不包含 dual-read 开关
- 不包含 feature flag
- 回滚方式是重新部署 3.5.14a 镜像
- 3.5.14a 与 3.5.14b 使用同一 Schema 版本
- 3.5.14b 不改表、不改 `schema_meta`
- 观察期只观察指标，不保留第二套代码
- 观察期结束后没有“删除旧读路径”这一步，因为旧路径不在 14b 二进制里

如果 14b 需要回滚：

1. 停止 14b
2. 部署 14a 镜像
3. 14a 使用旧读路径和旧字段投影
4. `source_host_caps` 保留，不删除
5. 修复后重新部署 14b

不允许用配置开关在同一二进制里切回旧读路径。那会留下长期双轨和清理债。

明确不做：

- 不把不同 Host 的缓存合并
- 不做跨 Host fallback 或竞速
- 不用手工标签替代真实探测
- 不自动猜测镜像属于哪个 Host
- 不为单一 Source 写代码特判
- 不在 3.5.14b 内做无前缀跨 Registry 自动匹配


### 3.5.15 CredStore + 标准私有 Registry 认证

**目标：** 让需要用户凭据的业务回源使用统一 CredStore，补齐标准私有 Registry 认证。

范围：

- Basic Auth
- Docker 用户名 / 密码
- 标准 Bearer Token 交换
- 匿名优先
- 凭据 Snapshot 和 generation
- 凭据轮换后旧 generation 不能覆盖新状态
- 日志脱敏

后续独立阶段：

- GCP ADC / OAuth
- AWS ECR IAM
- OIDC / 工作负载身份

安全边界：

- 用户凭据只发给明确授权的官方 Registry / Realm
- 不因为第三方 Source 声称需要认证，就把用户凭据发给它
- 匿名 401 才进入凭据认证；403、404、429、5xx 不默认用凭据重试

验收：

- 匿名公开仓库继续成功
- 需要认证的私有仓库可按凭据访问
- 业务回源真正使用 CredStore
- Probe 成功不能代替业务回源验收
- 跨 Host Realm、Token 和凭据泄漏测试通过

### 3.5.15a GitLab PAT / Deploy Token 专项

**目标：** 单独验证 GitLab 专用凭据，不混入 3.5.15 的标准 Bearer 认证。

范围：

- GitLab PAT / Deploy Token
- GitLab `/jwt/auth` 交换
- `read_registry` 权限边界
- 凭据只发往 GitLab 官方 Registry / Realm
- 不在本版本实现 GCP / AWS / OIDC

验收：

- GitLab 公开匿名流程不受影响
- PAT / Deploy Token 能完成私有镜像拉取
- 权限不足时返回准确状态，不降级成匿名成功
- 第三方 Source 不能获得 GitLab 用户凭据

### 3.5.16 公开 Manifest 竞速

**目标：** 同一 Host 已有多个稳定候选后，提供显式、可控的 Manifest 首包加速策略。

依赖：

- 公开竞速：3.5.11 + 3.5.14b
- 私有竞速：额外依赖 3.5.15；使用 GitLab 专用凭据时再依赖 3.5.15a

范围：

- `star_race` 是 Host 级查询策略，默认关闭
- 只允许同一 Host 下显式 `registry_race=true` 且健康的 Source
- 最多两个候选参与
- 其他候选仍按顺序回退
- winner 必须完成 Manifest 正文校验
- loser body / 连接 / goroutine 正确清理
- 只作用于 Manifest，不作用于 Blob
- 不跨 Host 竞速

验收：

- 默认关闭时严格 sequential
- 开关只影响指定 Host
- 只有显式合格候选参与，最多两个
- winner 响应完整可读
- loser 资源正确清理
- 有固定环境、基线、重复次数和 P50/P95 阈值的对照测速

### 3.5.17 Blob 竞速可行性评估

只做评估，不作为版本承诺：

- Blob 仍使用标准代理与内容寻址缓存
- 不并行拼接多个上游 Blob
- 只有 Body 所有权、取消边界、首字节后接管、Range/断点、`.part` 生命周期都有可验证方案后，才单独立项
- 未通过评估前不进入实现

## 6. 新旧路线映射

| 旧路线 | 新版本 | 说明 |
|---|---|---|
| 3.5.4 A：Host 凭据、Realm、Gateway 凭据、独立开关 | 3.5.11 + 3.5.15 | 公开 Realm 先做，私有 CredStore 后做 |
| 3.5.4 B：多 Registry 正式启用 | 3.5.12（含原 3.5.13） | 合并验收：补验证集缺口 + 完成第一批 |
| 3.5.4 C：Manifest star_race | 3.5.16 | 默认关闭，独立开关 |
| 3.5.4 D：Blob 竞速 | 3.5.17 | 只评估，单独立项 |
| 3.5.4 架构债：一源多 Host | 3.5.14a + 3.5.14b | 先回填投影，再切换权威读路径 |
| 3.5.10 Source Realm Draft | 3.5.11 | 3.5.10 已被标签修复占用 |

## 7. 版本门禁

1. 3.5.11 未通过前，不进入 3.5.12 的 Host 实现。
2. 3.5.12 按 Host 门禁声明：只有真实回源通过的 Host 才能声明支持，阻塞 Host 必须标注“Source 可用性阻塞”。
3. 3.5.12 完成前，不宣称第一批 Registry 全部支持。
4. 3.5.14a 可以回填、投影和双写，但不得切换候选读路径。
5. 3.5.14a 的 `backfillHostCaps` 未通过行数、冲突和双用途源校验前，不进入 14b。
6. 3.5.14b 完成前，不为任何 Source 手工添加多 Host 标签。
7. 3.5.14b 完成前，不把 Docker Hub / GHCR 的新旧能力模型混用。
8. 3.5.15 完成前，不宣称支持私有 NVCR / 私有 ECR。
9. 3.5.15a 完成前，不宣称支持私有 GitLab。
10. 3.5.16 默认关闭，不能用全局 Hedge 隐式开启。
11. 3.5.17 不是交付承诺，未评估通过不排期。

## 8. 回归门禁

### G0 流式内容一致性回归

这不是“已知未关闭 P0”，而是后续版本的回归门禁。

3.5.11、3.5.14b、3.5.16 合并前必须重新执行：

- Source 切换版本一致性
- 206 Content-Range 起点、总长度、Body 长度
- 200 前缀丢弃与 Content-Length
- 强 ETag / 弱 ETag / 无验证器分支
- Digest 校验器一致性
- `.part` 元数据绑定当前 Source
- resume 来源与实际回源来源一致
- 取消、关闭 Body、文件句柄和临时文件生命周期

当前 main 已有：

- `internal/gateway/watch.go:250` 的 `switchAcceptable`
- `internal/gateway/watch_switch_test.go`
- `internal/gateway/stream.go:672` 的 `plan.Upstream` 更新
- `internal/gateway/stream_switch_test.go`

如果后续发现问题，必须提供 commit、代码位置和可复现测试，不能笼统写“P0 未关闭”。

## 9. 仍然冻结的非目标

- 无前缀跨 Registry 自动搜索或猜测仓库归属
- 自动发现第三方镜像站
- GCR legacy 区域子域的自动支持或自动映射
- 不同 Registry Host 之间互相 fallback 或竞速
- Blob 竞速
- 为 Registry 功能重写通用代理协议、Scheduler 主架构或已有缓存系统
- 在当前一源一 Host 模型下提前建设通用站点映射表
- 在本轮新增 Azure ACR、NVIDIA NGC、AWS Private ECR 或其他 Host

## 10. 已定与待冻结

### 已定

1. 3.5.14 拆成 3.5.14a（回填 + 投影 + 新写路径）和 3.5.14b（只切权威读路径）。
2. 14a 的 `SetCapsLive` 和 `CommitHostProbe` 必须在同一事务内写 `source_host_caps` + 旧字段投影。
3. 14b 只包含新读路径，不包含旧读路径、dual-read 开关或 feature flag；回滚靠重新部署 14a 镜像。
4. 14a 与 14b 使用同一 Schema 版本；14b 不改表、不改 `schema_meta`。
5. `probe_status` 冻结为四态：`Supported / Unsupported / Inconclusive / Mismatch`，并与 `registry_cap` 的 `1/0/-1/2` 一一对应。
6. `Mismatch` 是硬排除，不按普通 Unsupported 的 6 小时周期重试。
7. `registry_prefix` 冻结为布尔，映射旧 `ghcr_prefix`，不做枚举。
8. `source_realm_policy` 一行一个 `(source_id, host)`：`primary_realm` + `extra_realms` JSON（最多 3 个）+ 每个 Host 一个 `policy_gen`；不按 Realm 行分散 generation。
9. GitLab PAT / Deploy Token 独立为 3.5.15a。
10. 无公共 Source 的 Host 允许用受控 Source 故障边验收代码路径，但对外必须标记“Source 可用性阻塞”，不能宣称已有公共加速能力。
11. 认证身份通过每次 attempt 的 request context 传递，不依赖静态 `adapter.Plan.SourceID`。
12. 3.5.14a 的离线迁移必须新增 `3.5.7 → 3.5.11 → 3.5.12 → 3.5.14a` 路径和 `backfillHostCaps` 步骤，不能只复用旧的三基线迁移。
13. 迁移采用临时库 + 原子发布；失败删除临时库和 sidecar，不保留半迁移状态；不宣称“单事务迁移”。
14. 3.5.11 分 11a/11b/11c；**11c 原计划未实施，已并入 3.5.12 的 12c**；完成后 Source Realm 只保留 `source_realm_policy`，官方直连走独立 `OfficialPolicy`（不是 fallback）；运行时数据库写入单事务；Token Cache 版本化失效；Schema 迁移离线 + 原子发布；无旧 Realm fallback；Schema 回滚靠备份。

### 仍待冻结

1. `db-migrate` 的升级命令拆分：3.5.7→3.5.11 和 3.5.12→3.5.14 是新增独立命令，还是扩展 `migrate` 的 baseline 列表。
2. `Mismatch` 是否允许手动重探，还是只能通过显式重置能力后重探。
3. `source_host_caps` 的 `probe_status` 用 TEXT 还是整数编码；无论哪种，必须与 `registry_cap` 保持一一映射。

在这些问题冻结前，3.5.14a 不进入实现。
