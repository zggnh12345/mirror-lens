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
2. `3.5.12` / `3.5.13` 不再写成“从零实现第一批 Registry”，改为**既有实现缺口闭环与验收补齐**。
3. `3.5.11` 的 Realm / Token 身份从第一天就采用 **Source + Host**，但不提前实现多 Host UI 和候选调度。
4. `3.5.14` 拆成 **3.5.14a（回填 + 投影 + 新写路径）** 和 **3.5.14b（只切换权威读路径）**。
5. `source_host_caps` 成为**所有逻辑 Registry Host 的能力权威**，Docker Hub / GHCR 不再作为例外。
6. 认证身份改为**每次 attempt 的 request context**，不再要求静态 `adapter.Plan` 携带 `SourceID`。
7. `ghcr_prefix` 纳入 `source_host_caps` 的兼容投影模型，冻结为布尔，不做枚举。
8. `probe_status` 从三态改为四态，补上 `Mismatch`，与现有 `RegistryCapMismatch=2` 对齐。
9. `3.5.14a` 同时包含新写路径；`3.5.14b` 只切读路径，不含旧读路径和 dual-read 开关。
10. 离线迁移增加 `3.5.7 → 3.5.11 → 3.5.13 → 3.5.14a` 路径和 `backfillHostCaps` 步骤，不能只复用旧的三基线迁移。
11. `3.5.15` 收窄为 CredStore + 标准私有 Registry 认证；GitLab PAT 独立 3.5.15a。
12. `3.5.16` 的公开 Manifest 竞速依赖 3.5.11 + 3.5.14b；私有竞速才依赖 3.5.15。
13. 撤回“流式 P0 未关闭”的错误判断；保留为 G0 回归门禁。
14. 增加 Host 验收矩阵、探测四态、迁移验收和双用途源回归。
15. 增加 3.5.11 完成定义：单一 Realm 权威、数据库单事务、Token Cache 版本化、无 fallback、Schema 回滚靠备份。

## 2. 当前基线

### 2.1 已完成版本

| 版本 | 内容 | 状态 |
|---|---|---|
| 3.5.4 | 隐式 Registry 身份收敛、Canonical 前置 | 已发布 |
| 3.5.5 | Canonical Schema、离线升级 | 已发布 |
| 3.5.8 | 第一批公开 Registry 多源调度 | 已发布 |
| 3.5.9 | 多源开关、只读标签、鉴权回退修正 | 已发布 |
| 3.5.10 | 多能力标签修复（HUB/GHCR/RAW） | 已发布并部署 |
| 3.5.11 | Source 自有 Realm 与认证隔离 | 规划中 |

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

- **同一 Host 多 Source**：一个 Host 配多个镜像源 → 3.5.13 做正式验收
- **一源多 Host**：一个镜像源同时服务多个 Host → 3.5.14a/3.5.14b 做能力模型

本文说的“其他多源也一样”指后者。任何一条 Source 只要实际支持多个 Host，都必须走同一套 `source_host_caps` 模型，不能按 Host 或按站点单独开分支。

## 4. 版本总表

| 版本 | 主题 | 依赖 | 默认行为 |
|---|---|---|---|
| 3.5.11 | Source Realm + Source/Host 认证身份 | 3.5.10 | 旧 Source 行为不变 |
| 3.5.12 | GCR / Artifact Registry / K8S 既有缺口闭环 | 3.5.11 | 新 Host 默认关闭 |
| 3.5.13 | 第一批剩余 Host + 同 Host 多 Source 正式验收 | 3.5.12 | 顺序回源 |
| 3.5.14a | `source_host_caps` 回填 + 旧字段投影 + 新写路径 | 3.5.13 | 读路径不变 |
| 3.5.14b | 只切换 `source_host_caps` 为权威读路径 | 3.5.14a | 旧单 Host Source 不变 |
| 3.5.15 | CredStore + 标准私有 Registry 认证 | 3.5.14b | 匿名优先 |
| 3.5.15a | GitLab PAT / Deploy Token 专项 | 3.5.15 | 单独验收 |
| 3.5.16 | 公开 Manifest 竞速 | 3.5.11 + 3.5.14b | 默认关闭 |
| 3.5.17 | Blob 竞速可行性评估 | 3.5.16 | 仅评估 |

> 3.5.15a 不阻塞 3.5.16 的公开 Manifest 竞速；私有 Manifest 竞速依赖 3.5.15（以及 3.5.15a，如果使用 GitLab 专用凭据）。
>
> 本链中明确改 Schema 的是 **3.5.11**（`source_realm_policy`）和 **3.5.14a**（`source_host_caps`）。3.5.14b 只改读路径，不改 Schema。

## 5. 各版本安排

### 3.5.11 Source Realm + Source/Host 认证身份

**目标：** 把 Realm 判定从“Host 全局白名单”改为“按 Source + Host 计算的策略”，解决公共镜像站使用自己认证端点时被误杀的问题。

范围：

- 新增 `source_realm_policy`，按 `(source_id, host)` 存储允许的 Realm
- 旧 Source 初始只有 `host = repo_host` 一行
- Realm 精确匹配 scheme + host，不接受通配符、userinfo、`http://`
- 第三方 Source 不自动继承官方 Host 的 Realm
- 探活与 Gateway 使用同一套策略
- Token 缓存按完整身份隔离
- Source 禁用、删除、凭据轮换、Realm 策略变更时使旧 Token 失效
- 失败关闭，不因策略解析失败而放行

建议表结构：

```text
source_realm_policy
  source_id
  host
  realm_url
  service
  is_primary
  enabled
  policy_gen
  updated_at
  PRIMARY KEY(source_id, host, realm_url)
```

策略规则：

- 每个 `(source_id, host)` 最多一个 `is_primary=1`
- 额外 Realm 只能显式增加，不做通配符或自动发现
- 一个 Source 的不同 Host 可以有不同主 Realm
- 初始迁移时，`host = repo_host`，`is_primary=1`

主 Realm 唯一性：

```sql
CREATE UNIQUE INDEX idx_source_realm_primary
ON source_realm_policy(source_id, host)
WHERE is_primary = 1;
```

额外 Realm 上限 3 个，由 Store 在写入事务内检查；超过直接拒绝。

Schema 纪律：

- `source_realm_policy` 是新增表，属于 Schema 变更
- 必须走离线 `db-migrate`，不能运行时建表
- 必须提升 `schema.Version` 和 `CanonicalSchemaHash`
- 3.5.11 的输入是 3.5.10 的 3.5.7 Schema
- 3.5.14a 的输入将是 3.5.13 的 Schema，不是直接 3.5.7

Token 缓存 key 至少包含：

```text
source_id
host
realm_origin
service
scope
credential_generation
realm_policy_generation
```

认证上下文要求：

- `adapter.Plan` 只增加 `LogicalHost`；不在 Plan 创建阶段强塞 `SourceID`
- 候选选中后，把 `SourceID + LogicalHost + CredentialGeneration + PolicyGeneration` 写入该次 attempt 的 request context
- `bearerTransport` 从 `req.Context()` 取完整身份，不能只根据 `req.URL.Host` 猜 Source
- 不能把 Source 名称当身份；名称可改、可重复
- 并发 Token 刷新使用同一完整 key
- 不能把用户凭据发给第三方 Source 的 Realm

明确不做：

- 不做多 Host UI
- 不做多 Host 候选调度
- 不做多 Host 能力管理
- 不实现私有仓库 CredStore
- 不实现 OAuth / OIDC / 云厂商 IAM

验收：

- `m.daocloud.io` 作为 `public.ecr.aws` 的 Source 时可通过探活
- 旧 Source 空配置行为与 3.5.9 一致
- 探活通过和 Gateway 实际回源不出现两套 Realm 语义
- 同一 Source 的不同 Host 不共享错误 Token
- Source 删除、禁用、轮换凭据后旧 Token 不能复用
- Realm 请求继续经过 SSRF / DNS / IP Guard

#### 3.5.11-A 完成定义与零残留门禁

3.5.11 可以做到单事务写入、单轨运行、代码和权威数据零残留，但不是自动成立，必须满足以下条件。

**1. 单一 Realm 权威**

完成后只能保留：

- `source_realm_policy`

必须删除或降为只读审计、不能再参与判定：

- 旧全局 Realm 白名单
- 旧 `realm_extra`
- 代码里的 Host → Realm 静态表
- 任何“新表查不到就回退旧白名单”的 fallback

只要保留一个 fallback，3.5.11 本身就是过渡态。

**2. 数据库单事务**

所有写路径必须在一个 `BEGIN IMMEDIATE` 事务内完成：

- 创建 Source：`sources` + 默认 `source_realm_policy` + `policy_gen` + `source_events`
- 修改 Realm：`source_realm_policy` + `policy_gen++` + `source_events`
- 禁用 / 删除 Source：状态或删除 + `policy_gen++`
- 任一步失败整体回滚

不允许：

- 先建 Source，再单独建 Realm 策略
- 先写策略，再补 `policy_gen`
- 策略写入成功、审计失败但只回滚一半
- 存在第二条单独写 Realm 的路径

**3. Token Cache 不能纳入数据库事务**

必须明确：

- `source_realm_policy` 和 `policy_gen` 可以事务提交
- 进程内 Token Cache 的删除不是数据库事务的一部分
- 不能宣称“数据库 + 缓存原子提交”

正确做法是版本化 key：

```text
source_id | host | realm_origin | service | scope
| credential_generation | realm_policy_generation
```

规则：

- 修改 Realm → `policy_gen++`
- 轮换凭据 → `credential_generation++`
- 禁用 / 删除 Source → 旧 key 不再可达
- 部署或启动时清空旧 namespace
- 旧 key 不允许作为 fallback
- 旧条目依靠 TTL 或显式 flush 清理

“零残留”在这里的含义是：

- 旧 Token **不可达**
- 旧 namespace **已清理**
- 不是“数据库和缓存同一事务”
- 也不是“缓存旧条目当场物理消失”

**4. Schema 迁移窗口**

3.5.11 新增 `source_realm_policy`，属于 Schema 变更：

- 必须离线 `db-migrate`
- 运行时不允许建表或补列
- 旧二进制不能直接打开新库
- 回滚方式是恢复备份库 + 旧 `secret.key`
- 不能把“旧二进制直接跑”当作回滚方案
- 迁移完成后只保留一个 Schema 版本

**5. 单轨运行**

完成后必须满足：

- Gateway 和 Probe 只读 `source_realm_policy`
- 没有旧 Realm 读取路径
- 没有 dual-read 开关
- 没有 feature flag
- 没有按 Host 写死的 Realm 特判
- 3.5.11 仍然只支持单 Host；不要把它和一源多 Host 混为一谈

**6. 验收清单**

- 每个 `(source_id, host)` 有且仅有一个 `is_primary=1`
- 所有 Source 的 Realm 策略已回填
- 旧 Realm 配置不再参与判定
- 创建、修改、禁用、删除都走单事务
- Token Cache key 包含完整身份和两个 generation
- 删除、禁用、轮换凭据后旧 Token 不能复用
- `m.daocloud.io` 作为 `public.ecr.aws` Source 可通过探活
- 迁移后旧二进制被拒绝启动
- 回滚演练使用备份库 + 旧 `secret.key`
- 代码中不存在旧 Realm 白名单和 fallback

**7. 不能承诺的部分**

- 不能承诺数据库和 Token Cache 原子提交
- 不能承诺缓存旧条目立即物理消失
- 不能承诺旧二进制直接接管新 Schema
- 不能承诺 3.5.11 完成一源多 Host

满足以上 1–6，3.5.11 完成后就是单轨、零残留；否则会留下旧 Realm 或旧 Token Cache 的尾巴。

### 3.5.12 GCR / Artifact Registry / K8S 既有缺口闭环

**目标：** 以 3.5.8 / 3.5.9 的现有实现为基线，补齐 GCR、Artifact Registry、K8S 的真实回源验收和剩余缺口；不是重新实施第一批 Registry。

范围：

- 审计 3.5.8 已有实现，确认 `multi_origin_hosts`、Host 准入、`officialOnly`、路径改写和回退仍然有效
- 三个 Host 的独立启停
- 三个 Host 的匿名 Bearer Challenge / Realm / Token 流程
- 每个 Host 的固定测试仓库、Tag 或 Digest
- 真实进程、真实 Source、完整 Manifest / Blob 拉取
- 候选失败回官方
- Docker Hub / GHCR 回归

不做：

- 不重写 3.5.8 已完成的候选调度
- 不让一条 Source 服务多个 Host
- 不解决春川 K8S 标签
- 不启用第一批之外的 Host
- 不把没有公共 Source 的 Host 标成“代码未实现”

验收：

- 三个 Host 可独立启停，关闭后不能查询候选或借 fallback 绕过
- 配置 Source 后请求实际命中候选，而不是只检查配置存在
- 三个 Host 都能完成真实公开仓库拉取
- Host A 的故障、探测和冷却不污染 Host B
- Docker Hub / GHCR 无回归
- 所有验收记录带 Source、Host、测试仓库、Digest、回退结果

### 3.5.13 第一批剩余 Host + 同 Host 多 Source 正式验收

**目标：** 完成第一批剩余 Host 的覆盖验收，并把“同 Host 多 Source”从已有基础能力提升为正式验收能力。

范围：

- `quay.io`
- `mcr.microsoft.com`
- `registry.gitlab.com`
- `public.ecr.aws`
- `cgr.dev`（受公共 Source 可用性约束）
- `<location>-docker.pkg.dev`（选择一个真实区域 Host 作为代表）

已有基础能力：

- `SourceCandidates("registry:<host>")` 已能返回多条 Source
- 每条 Source 已有独立健康记录
- 候选按顺序尝试
- 失败后继续回退
- 最后保留官方回源

3.5.13 不重新设计这套调度，只补：

- 两条以上真实 Source 同时加入同一 Host
- 第一条 404，第二条成功
- 第一条 401，第二条成功
- 第一条 429/503，检查冷却和 Retry-After
- 全部候选失败，官方回退成功
- 各 Source 的熔断、健康、并发和缓存归属不串用
- 修改 Source 优先级后，调度缓存及时更新

Host 验收矩阵：

| Host | 3.5.8/3.5.9 现状 | 3.5.13 要求 | 阻塞分类 |
|---|---|---|---|
| `gcr.io` | 多提供方覆盖 | 真实回源回归 | 无 |
| `<location>-docker.pkg.dev` | 无公共 Source | Host 准入 + 官方故障边 | Source 可用性 |
| `registry.k8s.io` | 多提供方覆盖 | 真实回源回归 | 无 |
| `quay.io` | 多提供方覆盖 | 正式验收 | 无 |
| `mcr.microsoft.com` | 多提供方覆盖 | 正式验收 | 无 |
| `registry.gitlab.com` | 仅 DaoCloud | 正式验收 + Realm | Source 覆盖有限 |
| `public.ecr.aws` | 仅 DaoCloud | 正式验收 + Realm | Realm / Source 覆盖有限 |
| `cgr.dev` | 无可用公共 Source | 只做官方故障边，不承诺公共源 | Source 可用性 |
| `lscr.io` | 原第一批明确排除 | 单独扩展验收 | 范围外 |

`cgr.dev` 和 `<location>-docker.pkg.dev` 不能被写成“代码未完成”；它们当前主要是 Source 可用性问题。

`cgr.dev` 的官方匿名入口返回 401，不等于可用的故障边。3.5.13 只能用受控 Source 的 404/5xx 验证回退路径，用官方 401 验证 Challenge 透传；对外仍标记“公共 Source 缺失”，不能把 401 当成功，也不能把“代码路径通过”写成“已有公共加速能力”。

验收：

- 第一批全部可验收 Host 的公开代表性仓库可实际拉取
- 一个 Host 配置两条以上 Source 时按顺序尝试并正确回退
- 一个 Source 故障不导致同 Host 其他 Source 被错误熔断
- 不同 Host 的 Source、健康、Manifest 缓存和认证状态不串用
- 只完成 3.5.12 时只能称“验证集通过”，不能称“第一批全部支持”
- 没有公共 Source 的 Host 必须明确标记“Source 可用性阻塞”，不能伪造通过

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

3.5.14a 必须包含新写路径，不能只建表：

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

当前生产 Schema 是 `3.5.7`，但 3.5.11 新增 `source_realm_policy` 后会再次提升 Schema。因此迁移链不是简单的一条 `3.5.7 → 3.5.14a`：

```text
3.5.7  → 3.5.11  → 3.5.13  → 3.5.14a
         Realm 表    Host 能力表
```

当前 `db-migrate` 不能直接迁移这条链：

- `inputSchemaFingerprints` 只接受 `3.4.6 / 3.5.4 / 3.5.5`
- `upgrade356` 只处理 `3.5.6 → 3.5.7`
- 没有 3.5.7、3.5.11、3.5.13 的输入路径

因此 3.5.11 和 3.5.14a 都必须各自提供离线升级路径：

- 3.5.11：`3.5.7 → 3.5.11`，新增 `source_realm_policy`
- 3.5.14a：`3.5.13 → 3.5.14`，新增 `source_host_caps` 并回填
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
| 3.5.4 B：多 Registry 正式启用 | 3.5.12 + 3.5.13 | 先补验证集缺口，再完成第一批验收 |
| 3.5.4 C：Manifest star_race | 3.5.16 | 默认关闭，独立开关 |
| 3.5.4 D：Blob 竞速 | 3.5.17 | 只评估，单独立项 |
| 3.5.4 架构债：一源多 Host | 3.5.14a + 3.5.14b | 先回填投影，再切换权威读路径 |
| 3.5.10 Source Realm Draft | 3.5.11 | 3.5.10 已被标签修复占用 |

## 7. 版本门禁

1. 3.5.11 未通过前，不进入 3.5.12 的 Host 实现。
2. 3.5.12 只证明三个 Host 可用，不宣称第一批完整支持。
3. 3.5.13 完成前，不宣称第一批 Registry 全部支持。
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
8. `source_realm_policy` 每个 `(source_id, host)` 最多 1 个主 Realm + 3 个额外 Realm；主 Realm 用 partial unique index 保证唯一。
9. GitLab PAT / Deploy Token 独立为 3.5.15a。
10. 无公共 Source 的 Host 允许用受控 Source 故障边验收代码路径，但对外必须标记“Source 可用性阻塞”，不能宣称已有公共加速能力。
11. 认证身份通过每次 attempt 的 request context 传递，不依赖静态 `adapter.Plan.SourceID`。
12. 3.5.14a 的离线迁移必须新增 `3.5.7 → 3.5.11 → 3.5.13 → 3.5.14a` 路径和 `backfillHostCaps` 步骤，不能只复用旧的三基线迁移。
13. 迁移采用临时库 + 原子发布；失败删除临时库和 sidecar，不保留半迁移状态；不宣称“单事务迁移”。
14. 3.5.11 完成后只保留 `source_realm_policy` 一个 Realm 权威；数据库写单事务；Token Cache 版本化 key；无旧 Realm fallback；Schema 回滚靠备份。

### 仍待冻结

1. `db-migrate` 的升级命令拆分：3.5.7→3.5.11 和 3.5.13→3.5.14 是新增独立命令，还是扩展 `migrate` 的 baseline 列表。
2. `Mismatch` 是否允许手动重探，还是只能通过显式重置能力后重探。
3. `source_host_caps` 的 `probe_status` 用 TEXT 还是整数编码；无论哪种，必须与 `registry_cap` 保持一一映射。
4. `source_realm_policy` 额外 Realm 的审计字段是否需要单独记录 `policy_gen` 变化。

在这些问题冻结前，3.5.14a 不进入实现。
