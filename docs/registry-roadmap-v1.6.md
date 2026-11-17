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
> 3.5.14 的冻结细则见 `docs/3.5.14-freeze.md`；两者冲突时以冻结文档为准。

## 1. 本版修正摘要

v1.6 相对 v1.5 的核心修正：

1. `3.5.10` 已被多能力标签修复占用；原 Source Realm 计划顺延为 **3.5.11**。
2. `3.5.12` / `3.5.13` 不再写成“从零实现第一批 Registry”，改为**既有实现缺口闭环与验收补齐**。（2026-11-16 后续修订：两者已合并为 3.5.12，3.5.13 编号退役，见 1.1）
3. `3.5.11` 的 Realm / Token 身份从第一天就采用 **Source + Host**，但不提前实现多 Host UI 和候选调度。
4. `3.5.14` 拆成 **3.5.14a（回填 + 投影 + 新写路径）**、**3.5.14b（只切换权威读路径）** 和 **3.5.14c（旧结构清理）**；14a/14b/14c 是内部里程碑，最终只发布 `v3.5.14`。
5. `source_host_caps` 成为**所有逻辑 Registry Host 的能力权威**，Docker Hub / GHCR 不再作为例外。
6. 认证身份改为**每次 attempt 的 request context**，不再要求静态 `adapter.Plan` 携带 `SourceID`。
7. `ghcr_prefix` 纳入 `source_host_caps` 的兼容投影模型，冻结为布尔，不做枚举。
8. 状态模型改为 `last_verdict`（Supported / Unsupported / Inconclusive / Mismatch）+ `effective_status`（Unknown / Supported / Unsupported / Mismatch），`Mismatch` 与现有 `RegistryCapMismatch=2` 对齐。
9. `3.5.14a` 同时包含新写路径；`3.5.14b` 只切读路径，不含旧读路径和 dual-read 开关；`3.5.14c` 才删除旧 Registry 能力字段和 `repo_host`。
10. 离线迁移增加 `3.5.7 → 3.5.11 → 3.5.14a` 路径和 `backfillHostCaps` 步骤；3.5.12 的程序版本不产生新的 Schema 输入指纹，生产输入仍是 `3.5.11 / e39e1197…`。
11. `3.5.15` 重新定义为 **CredStore 业务接入 + 官方私有 Registry 认证 + 凭据安全边界闭环**，不重建现有 CredStore；GitLab PAT 独立 3.5.15a。
12. `3.5.16` 的公开 Manifest 竞速依赖 3.5.11 + 3.5.14c；私有竞速才依赖 3.5.15。竞速不能建立在尚未收敛的 B1 模型上。
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
4. 离线迁移链改为 `3.5.7 → 3.5.11 → 3.5.14a`（2026-11-17 修订：3.5.12 是程序版本，不产生新的 Schema 输入；生产 Schema 仍是 3.5.11）。
5. 12-S 是**按 Host** 的门禁：`12-S(host)` 未通过，不启动该 Host 的真实回源验收；无公共 Source 的 Host 标记“外部 Source 阻塞”，不阻塞其他 Host 与整体发布。
6. 生产基线以 2026-11-16 实测为准（见 5 的 3.5.12-A 基线表），不以 Git 标签推断部署状态。
7. **12c 收敛为 Source 级 Realm 策略管理 + Host 只读汇总**，不新增 Host 级策略模型，不改 Schema；不存在“策略启停”字段，停用语义为删除策略行并 fail closed。
8. **12-S 必须先收窄生产 `multi_origin_hosts` 再逐 Host 加源**。生产当前未显式配置该字段，默认 8 个 Host 全开；先建源再验收不构成灰度。
9. **12-F1 只过滤真正的探测执行入口**（`jobs()` / `ListDueHostProbes`）。`reconcileRepos` 不执行探测、不写健康状态，不纳入白名单过滤。
10. **12-F2 不含 `repo_host` 修改**：当前没有修改 `repo_host` 的 API，`SetRepoHost` 仅为 Store 方法；该能力留到 3.5.14 一源多 Host 阶段统一设计。

### 1.2 2026-11-17 修订

1. `3.5.15` 不再定义为“从零建设 CredStore”。3.5.14 已有 Host 级加密存储、代次、Snapshot、Reconcile 和 Probe 认证基础；3.5.15 的重点是把凭据安全接入 Gateway 业务回源，并补齐管理入口。
2. `3.5.15` 拆成 **15-A（凭据管理与一致性）**、**15-B（Gateway 私有认证与缓存隔离）**、**15-C（身份、Probe 与重定向安全）**、**15-D（范围冻结与验收）** 四个内部工作块；1.3 已按二次评审整体重写。
3. 凭据授权冻结为 **AND** 门禁：必须同时满足 `SourceID == 0` 和 `PolicySnapshot.Official == true`，否则禁止使用用户凭据。`Official` 只能由可信内部调用路径设置，不能由请求参数或候选源配置声明。（1.3 已扩展为 `IdentityKind=Official` + Host/gen 校验；本条保留为历史修订记录。）
4. 候选源即使拥有合法的 Realm Policy，也不能获得官方用户名密码或官方私有 Token；重定向后必须重新校验目标 Host 和 Realm。
5. Hub/GHCR 暂不纳入首批；GitLab PAT / Deploy Token 仍独立为 3.5.15a。生产目录当前没有 `registry_creds.enc` 只作为部署基线记录，不能作为代码安全验收通过的证据。

### 1.3 2026-11-17 二次修订（3.5.15 整体重写）

1. 3.5.15 上一版不能直接冻结。P0-1～P0-7 全部接受：官方 `CredGen` 恒为 0、私有内容进入共享缓存、`SourceID` 零值身份、重定向认证头、Basic 范围矛盾、文件/DB 非原子、版本号冲突。
2. 3.5.15 以“唯一权威设计”重写，保留 15-A/15-B/15-C/15-D 结构，新增威胁模型、决策记录、最终复核裁定、凭据一致性状态机、私有缓存隔离、身份与重定向安全、Probe/Gateway 职责边界、回滚降级和完整验收矩阵。
3. 五项决策已定（正文见 3.5.15 决策记录 D-15-1～D-15-5）：Registry 直接 Basic 纳入；私有 Host 使用明确 allowlist；私有认证开启时强制 Gateway Access Token；私有/认证响应绕过共享缓存；官方 Realm 本版只支持同 Host 和已有内置 Realm。
4. `3.5.16` 保持“公开 Manifest 竞速”，`3.5.17` 保持“Blob 竞速可行性评估”；认证运维能力不占用这两个版本号，后续单独排期。
5. 本版不进入代码实施；先完成最终设计复核，再按 15-A → 15-B → 15-C → 15-D 顺序开工。
6. 2026-11-17 最终复核追加裁定：真实账号保持 `BLOCKED_EXTERNAL_SOURCE`；fixture 固定隔离容器 + 443 + allowlist 域名 + 测试 CA；epoch 由 Gateway 缓存管理模块清理；公共内容失去共享缓存接受；不增加 `CredDigest`；`PrivateAuthEnabled ⇒ GatewayAccessTokenRequired`；`OfficialOnly` 全生命周期；epoch 持久化与回滚闭合。

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
- **一源多 Host**：一个镜像源同时服务多个 Host → 3.5.14a/3.5.14b/3.5.14c 做能力模型与旧结构清理

本文说的“其他多源也一样”指后者。任何一条 Source 只要实际支持多个 Host，都必须走同一套 `source_host_caps` 模型，不能按 Host 或按站点单独开分支。

## 4. 版本总表

| 版本 | 主题 | 依赖 | 默认行为 |
|---|---|---|---|
| 3.5.11 | Source Realm + Source/Host 认证身份 | 3.5.10 | 旧 Source 行为不变 |
| 3.5.12 | 合并验收 + 定点修复（原 3.5.12 + 3.5.13） | 3.5.11 | 新 Host 默认关闭；修复随开关生效 |
| 3.5.13 | 编号退役，已并入 3.5.12，不单独发布 | — | — |
| 3.5.14a | `source_host_caps` 回填 + 旧字段投影 + 新写路径 | 3.5.12 | 读路径不变；Schema S14a |
| 3.5.14b | 只切换 `source_host_caps` 为权威读路径 | 3.5.14a | 旧单 Host Source 不变；Schema S14a |
| 3.5.14c | 删除旧 Registry 能力字段、旧投影和 `repo_host` 业务依赖 | 3.5.14b | Schema S14c；Source × Host 管理 |
| 3.5.15 | CredStore 业务接入 + 官方私有 Registry 认证 + 凭据安全边界闭环 | 3.5.14c | 私有认证默认关闭；Access Token 必需；缓存绕过；Hub/GHCR 暂缓 |
| 3.5.15a | GitLab PAT / Deploy Token 专项 | 3.5.15 | 单独验收 |
| 3.5.16 | 公开 Manifest 竞速 | 3.5.11 + 3.5.14c | 默认关闭 |
| 3.5.17 | Blob 竞速可行性评估 | 3.5.16 | 仅评估 |

> 3.5.15a 不阻塞 3.5.16 的公开 Manifest 竞速；私有 Manifest 竞速依赖 3.5.15（以及 3.5.15a，如果使用 GitLab 专用凭据）。
>
> 本链中明确改 Schema 的是 **3.5.11**（`source_realm_policy`）、**3.5.14a**（`source_host_caps`）和 **3.5.14c**（删除旧 Registry 字段）。3.5.14b 只改读路径，不改 Schema。
>
> `3.5.14a/b/c` 是 3.5.14 的内部里程碑，不是发布标签；最终发布标签为 `v3.5.14`。
>
> Schema 标签：14a/14b 为 `3.5.14a`（`H14a`），14c/最终发布为 `3.5.14`（`H14c`）；14b 不改 Schema，14c 不能复用 14a 的版本号。
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

> **实施状态（2026-11-17 修订）：** 12c、12-F1、12-F2、12-T 已实现并通过自动化与 Docker 模拟验收。12-S 已逐 Host 执行：gcr.io、registry.k8s.io、quay.io、mcr.microsoft.com 为 `REAL_PASS`；registry.gitlab.com、public.ecr.aws 的 host-prefix 候选已实测 200（`m.daocloud.io/v2/<host>/...`），但尚未经产品级 HostProbe/调度，记为 `CANDIDATE_OBSERVED` + `NOT_EXECUTED`；cgr.dev、asia-east1-docker.pkg.dev 的单一 403 不足以判定整个 Host 无公共源，记为 `NOT_EXECUTED`，保留 per-host 与 host-prefix 两种尝试证据。原 `BLOCKED_EXTERNAL_SOURCE` 不再作为这四个 Host 的统一状态。生产已完成第一阶段程序切换（3.5.11 → 3.5.12，未改 Schema、未迁移数据库）；第二阶段已完成四个 Host 的真实回源验收。记录见 `docs/3.5.12-acceptance.md` 第 6、7 节和 `docs/3.5.14-freeze.md` 第 10 节。

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

> **2026-11-17 冻结修订：** 本节的数据模型、投影边界、状态语义和迁移边界以 `docs/3.5.14-freeze.md` 为准；以下为摘要。

#### 3.5.14a-A 数据模型

```text
source_host_caps
  source_id
  host
  enabled
  last_verdict
  effective_status
  registry_path_mode
  registry_prefix
  PRIMARY KEY(source_id, host)
  FOREIGN KEY(source_id) REFERENCES sources(id) ON DELETE CASCADE
```

索引至少包含：

```sql
CREATE INDEX idx_source_host_caps_host
ON source_host_caps(host, effective_status, enabled);
```

字段语义：

- `enabled`：用户是否允许该 Source 服务此 Host
- `last_verdict`：最近一次探测观察，四态：`Supported` / `Unsupported` / `Inconclusive` / `Mismatch`
- `effective_status`：当前有效能力结论，四态：`Unknown` / `Supported` / `Unsupported` / `Mismatch`
- `registry_path_mode`：已验证的路径模式（`native` / `host_prefix`）
- `registry_prefix`：GHCR 等前缀路径标志，兼容旧 `ghcr_prefix`；布尔

新表不存 `registry_cap`、`last_supported_at`、`registry_probe_at`、`registry_probe_error`。`registry_cap` 只是旧字段投影，由 `effective_status` 派生；Foreign Host 的探测事实和成功时间以 `registry_host_recovery` 为唯一来源。

`Inconclusive` 不得覆盖已有 `Supported` / `Unsupported` 结论；`Mismatch` 表示 Manifest 内容与官方不一致，必须硬排除，不能按 6 小时普通不支持重试，只能由管理员显式重置后重新探测。

#### 3.5.14a-B 回填规则

对每个现有 Source：

1. `repo_host` 非空且不是 `docker.io` / `ghcr.io`：
   - 写入 `host=repo_host`
   - `last_verdict` / `effective_status` 由旧 `registry_cap` 映射：`1→Supported/Supported`、`0→Unsupported/Unsupported`、`-1→Inconclusive/Unknown`、`2→Mismatch/Mismatch`
   - `registry_path_mode=registry_path_mode`
2. `docker_cap`：
   - 写入或合并 `host=docker.io`
   - `last_verdict` / `effective_status` 由 `docker_cap` 映射：`1→Supported/Supported`、`0→Unsupported/Unsupported`、`-1→Inconclusive/Unknown`
   - `registry_path_mode=native`
3. `ghcr_cap`：
   - 写入或合并 `host=ghcr.io`
   - `last_verdict` / `effective_status` 由 `ghcr_cap` 映射：`1→Supported/Supported`、`0→Unsupported/Unsupported`、`-1→Inconclusive/Unknown`
   - `registry_path_mode=native`
   - `registry_prefix=ghcr_prefix`
4. `repo_host=''`：不创建 Foreign caps 行；不从 `base_url` 推断 Host；不把空字符串映射为 `docker.io`；`github_cap` / `git_cap` 不生成 Registry caps 行。
5. 同一 Source 的同一 Host 只能有一行；合并时冲突必须 fail closed，不能静默覆盖。
6. 旧字段不删除；由同一事务生成投影。
7. 回填 `enabled=1`；只有双用途 `ghcr.io` 的 `policy_gaps` 可以把该行置 0。14a 投影不读取 `enabled`，保证旧读路径不变；14b 的 `SetSourceHostEnabled` 才把停用同步为 0。

投影规则：

| 新表 Host | 旧字段投影 |
|---|---|
| `docker.io` | `docker_cap` |
| `ghcr.io` | `ghcr_cap`、`ghcr_prefix` |
| 原 `repo_host` 且是 Foreign Host | `registry_cap`、`registry_path_mode`、`registry_probe_at`、`registry_probe_error` |
| 其他新增 Foreign Host | 不投影 |

投影值取 `effective_status`，不取 `last_verdict`。新增 Foreign Host 探测成功不能修改 `repo_host`，也不能修改旧 Foreign 投影。`github_cap` 继续只写旧字段，不建 `github.com` caps 行。

Docker / GHCR 的 `effective_status` 映射必须显式实现，不能只写 `docker_cap` / `ghcr_cap`。否则 14b 的候选准入会与旧行为不一致。

冲突处理：

- `host=docker.io`：`docker_cap` 优先；`registry_cap` 只在 `docker_cap=-1` 时参考
- `host=ghcr.io`：`ghcr_cap` 优先；`registry_cap` 只在 `ghcr_cap=-1` 时参考
- 其他 Host：`registry_cap` 优先
- 两个已知值冲突且无法按上述优先级消解：fail closed，输出 `source_id` / `host` / 两个值，人工处理，不自动猜
- 不能静默选一个值继续迁移

六个双用途 Source（54、69、71、86、88、638）必须同时补 `(source_id, ghcr.io)` 的 `source_realm_policy`；不能安全生成时进入 `policy_gaps`，并把该 caps 行 `enabled=0`，同时在 14b 验收中记录预期差异。

生产只读盘点的预期行数为 194（4 Foreign + 95 Docker + 95 GHCR，无重叠）；实际行数不一致必须解释。明细见 `docs/3.5.14-freeze.md` 4.2。

#### 3.5.14a-C 写入边界

- 迁移工具只做一次性回填。
- 运行时 Store 只允许写 `source_host_caps`，并在同一事务内生成旧字段投影。
- API 不允许直接写 `repo_host` / `registry_cap` / `registry_path_mode` / `docker_cap` / `ghcr_cap` / `ghcr_prefix`。
- 旧字段对 API / 调用方只读；仅 Store 在同一事务内生成投影时允许写。
- 旧字段不能成为第二个权威入口。
- `SetCapsLive` 只拥有 `docker.io` / `ghcr.io` caps 行和 `docker_cap`、`ghcr_cap`、`ghcr_prefix` 投影；`github_cap` 继续只写旧字段。
- `CommitHostProbe` 只拥有 Foreign Host caps 行和原 `repo_host` 的 `registry_cap`、`registry_path_mode`、`registry_probe_at`、`registry_probe_error` 投影；不得修改 Docker/GHCR 投影。
- 增加一致性测试：随机生成多 Host Source，写入后断言新表与旧投影一致；新增 Foreign Host 探测不得改变 `repo_host` / 旧 Foreign 投影。

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

- 旧 `SourceCandidates("docker")` 查询在 14a 前后 diff 为空
- 旧 `SourceCandidates("ghcr")` 查询在 14a 前后 diff 为空
- 旧 `SourceCandidates("registry:<host>")` 查询在 14a 前后 diff 为空
- 新 caps 查询只作 shadow；新增 Host 的预期差异逐条记录，不要求 14a 的 old/new 全量相等
- 首页 HUB/GHCR/RAW/foreign 标签不变
- 双用途源（`repo_host=docker.io` + `ghcr_cap=1`）能力不变；六个双用途 Source 的 `ghcr.io` policy 缺口有明确结论
- 旧单 Host Source 行为不变
- `registry_cap=2` 回填为 `effective_status=Mismatch`
- `docker_cap=-1/0/1` 与 `ghcr_cap=-1/0/1` 的 `effective_status` 映射正确
- Docker / GHCR 与 `registry_cap` 冲突按优先级消解；无法消解时 fail closed
- `SetCapsLive` 失败时权威表和投影都不落库
- `CommitHostProbe` 失败时权威表和投影都不落库
- 新增 Foreign Host 探测成功不改变 `repo_host` / `registry_cap` / `registry_path_mode`
- 性能退化不超过 `docs/3.5.14-freeze.md` 11.4 的阈值
- 迁移失败不发布目标文件；灾难恢复使用备份库和旧 `secret.key`

#### 3.5.14a-E 能力审计

3.5.14a 的能力审计不切换读路径；回填和双写见前文：

- 枚举所有 enabled Source
- 对每个 Source 按 `host_prefix` 和 `native` 尝试候选 Host
- 只有实际 Manifest 可读且 Digest 可验证，才记 `last_verdict=Supported` / `effective_status=Supported`
- 认证失败、网络错误、仓库状态不确定记 `last_verdict=Inconclusive`；已有 Supported/Unsupported 时保持 `effective_status` 不变
- 固定测试仓库确认路径模式不支持才记 `last_verdict=Unsupported`
- Manifest 内容与官方不一致记 `last_verdict=Mismatch` / `effective_status=Mismatch`
- 记录 `source_id`、`host`、`path_mode`、`probe_at`、`probe_error`；Foreign Host 的成功时间以 `registry_host_recovery.last_success_at` 为准，不再写 `last_supported_at`
- 不从 `base_url` 推断，不从站点名称推断，不手工补标签
- 产出“多 Host Source 清单”
- `Inconclusive` 使用独立冷却和重试策略，不能长时间占用探测配额

#### 3.5.14a-F 离线迁移

3.5.14a 实施时的生产程序是 `3.5.12`，Schema 基线是 `3.5.11`（2026-11-16 实测）；3.5.11 新增 `source_realm_policy` 后 Schema 已提升。迁移链从 `3.5.7` 起保留给旧库，因此不是简单的一条 `3.5.7 → 3.5.14a`：

```text
3.5.7  → 3.5.11  → 3.5.14a
         Realm 表    Host 能力表
```

当前 `db-migrate` 已支持 3.5.7 输入，并已有 `backfillPolicies` / `policy_gaps`：

- `inputSchemaFingerprints` 已包含 `3.4.6 / 3.5.4 / 3.5.5 / 3.5.7`
- `3.5.7 → 3.5.11` 可以复用现有复制、Realm 回填和缺失策略报告
- 缺的是 `3.5.11 → 3.5.14a` 的输入指纹、`source_host_caps` DDL 和 `backfillHostCaps`

因此 3.5.14a 的迁移要分两段：

- 3.5.11：`3.5.7 → 3.5.11`，新增 `source_realm_policy`，按 `repo_host` 种子化并生成缺失策略报告
- 3.5.14a：`3.5.11 → 3.5.14a`，校验 `schema_version=3.5.11` / hash `e39e1197…`，新增 `source_host_caps` 并回填

这一步同时校验：没有策略的 Source 必须进入报告并在运行时 fail closed，不能回退旧白名单。

`copyAll` 不会自动生成 `source_realm_policy`；如果只复制旧表，升级后所有自定义 Host 都会认证失败。

每一步都更新输入 Schema 指纹；`cmd/db-migrate/main_test.go` 中“只允许三个输入指纹”的断言必须同步更新。

迁移输入只读，输出临时库；每一步提升 `schema.Version` 并重新冻结 `CanonicalSchemaHash`。

事务边界必须分开验收：

- 事务内：DDL、复制、回填、metadata、结构 Hash 验证、外键检查、完整性检查
- `schema.Verify` 当前只接受 `*sql.DB`，必须新增 `VerifyTx(*sql.Tx)` 或等价接口
- 事务提交后：关闭临时库、清理 sidecar
- 文件发布：独立执行 `publishNoClobber`

不能把文件系统原子发布写进 SQLite 事务承诺。

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
- 14a 开放流量前回滚 RPO=0；开放后恢复旧库会丢失迁移后的写入，不承诺 RPO=0；若要求 0，必须实现并实测 14a→3.5.11 反向迁移

准确表述是：

> 14a 的迁移验收要求“staging 单事务 + 文件原子发布”；两者分开验收。

当前 `db-migrate` 是多阶段提交：

- `EnsureFresh` 一次提交
- `copyAll` 另一阶段
- `finalizeTargetMeta` 再更新

14a 必须重构为目标临时库上的单事务：DDL、复制、Realm 回填、`backfillHostCaps`、metadata、结构 Hash 验证、外键/完整性检查全部在同一个 `BEGIN IMMEDIATE` 内；`schema.VerifyTx` 在事务内验证；提交后再独立 `publishNoClobber`。

如果本轮不重构，只能按“离线迁移 + 原子发布”验收，不能宣称“单事务迁移”；这两种口径不能混写。

`copyAll` 不能把 `source_host_caps` 当作旧表复制；它是新增表，复制后保持空表，必须由 `backfillHostCaps` 填充。

3.5.14b 不改 Schema、不改 `schema_meta`，保证 3.5.14a 镜像可以接管回滚。

### 3.5.14b 只切换 `source_host_caps` 为权威读路径

> **2026-11-17 冻结修订：** 本节候选准入、读路径和回滚边界以 `docs/3.5.14-freeze.md` 为准。

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
AND source_host_caps.effective_status = 'Supported'
AND source_host_caps.registry_path_mode IS NOT EMPTY
AND Host 全局准入允许
AND health / recovery 允许
AND policy_exists(source_id, host)
AND（Foreign Host）成功结论未过期
```

`policy_exists` 对所有 Host 生效；Docker/GHCR 的 policy 在 14a 回填时补齐。`multi_origin_hosts` 只约束 Foreign Host，Docker/GHCR 不因未列入该白名单被排除。

Docker/GHCR 不要求 `registry_host_recovery` 行；Foreign Host 的 TTL 只认 `registry_host_recovery.last_success_at`。

探测四态：

| 结果 | 含义 |
|---|---|
| Supported | Manifest 成功，Digest 通过 |
| Unsupported | 固定测试仓库确认该路径模式不支持 |
| Inconclusive | 认证、网络、仓库状态不确定 |
| Mismatch | Manifest 内容与官方不一致，硬排除 |

规则：

- `Inconclusive` 不得覆盖之前的 `Supported` / `Unsupported`
- `Mismatch` 必须硬排除，不能按普通 Unsupported 重试；只能管理员显式重置
- `effective_status` 决定候选；`last_verdict` 只记录最近探测事实
- Docker/GHCR 行也必须使用同一状态模型，不允许只写 `docker_cap` / `ghcr_cap`
- 新增 Host 是 14b 的预期新增候选，必须显式列出，不能要求与 14a 全量 diff 为空

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
- `internal/api/policy.go:65`：策略 Host 归属校验不能再比较 `src.RepoHost`，改为检查 `(source_id, host)` 是否存在 `source_host_caps` 行
- `ListRealmPolicies` / `ListPolicyHosts`：不再通过 `sources.repo_host` 取 Host，改为按 caps 的 Host 集合
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
- Source × Host 管理 API/UI 通过：新增、启停、删除（14b 禁止删原 `repo_host`）、重置 verdict
- 春川多 Host 验收通过；关闭 K8S 不影响 Docker/GHCR
- 性能退化不超过 `docs/3.5.14-freeze.md` 11.4 的阈值

迁移测试必须包含：

- 普通 foreign Source
- 双用途 Source（`repo_host=docker.io` + `ghcr_cap=1`）
- 空 `repo_host` Source
- 显式 GHCR Source
- 有 `ghcr_prefix=0/1` 的 Source
- `effective_status=Mismatch` 的 Source
- `docker_cap=-1/0/1` 与 `ghcr_cap=-1/0/1` 的映射
- 已过期探测成功记录
- 旧代次健康记录
- 有 recovery 行和无 recovery 行
- 14a 已通过 3.5.7 输入库的 `db-migrate` 迁移；14b 复用其结果，不重复迁移
- `backfillHostCaps` 前后行数和冲突校验
- 逐 Host old/new 候选对比；旧 Host 无非预期差异，新增 Host 显式列出

#### 3.5.14b-F 春川样本验收

- 春川可保存 `docker.io`、`ghcr.io`、`registry.k8s.io`、`gcr.io`
- 首页只显示实际探测通过的标签
- K8S 请求实际命中春川，而不是只显示标签
- 关闭春川 K8S 能力不影响 Docker / GHCR
- Docker / GHCR / K8S / GCR 的健康、冷却、失败互不污染

#### 3.5.14b-G Source × Host 管理与只读矩阵

- UI 增加 Source × Host 能力矩阵，展示 `enabled`、`last_verdict`、`effective_status`、`registry_path_mode`、`blocked_reason`
- `enabled` 可编辑；`effective`、探测结论、阻止原因只读
- API 必须覆盖 `GET/POST /api/sources/{id}/hosts`、`PUT .../enabled`、`DELETE .../{host}`、`POST .../{host}/reset`
- `AddSourceHost` 必须同时写 caps、policy、health/recovery 和审计
- `SetSourceHostEnabled` 必须同步旧投影（`enabled ? cap : 0`）、审计和调度缓存失效
- 14b 期间禁止删除 `host == repo_host`；原主 Host 的移除留到 14c 或走 `SetRepoHost` 迁移
- 用于运维核对春川这类多 Host Source；14b-F 的 K8S 关闭必须走这个 API，不能靠改配置文件

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


### 3.5.14c 旧 Registry 结构清理

> **2026-11-17 冻结修订：** 本节以 `docs/3.5.14-freeze.md` 第 8 节为准。

**目标：** 删除旧 Registry 能力字段、旧投影和 `repo_host` 业务依赖，完成单轨运行；不删除 `github_cap` / `git_cap` / `range_cap`。

#### 3.5.14c-A 删除范围

- `sources.repo_host`
- `sources.registry_cap`
- `sources.registry_path_mode`
- `sources.registry_probe_at`
- `sources.registry_probe_error`
- `sources.docker_cap`
- `sources.ghcr_cap`
- `sources.ghcr_prefix`
- 旧投影写入和旧读兼容代码

保留 `github_cap`、`git_cap`、`range_cap`：它们代表 GitHub RAW / Release、Git smart HTTP、Range 分片，不是 Registry Host 能力。

#### 3.5.14c-B Host 关系操作

14c 不保留“主 Host”字段。Source 的 Host 集合完全由 `source_host_caps` 表达；UI/API 改为 Source × Host。

`AddSourceHost` / `SetSourceHostEnabled` / `RemoveSourceHost` / `ResetSourceHostVerdict` 已在 14b 建立（见 3.5.14b-G），14c 只做：

- 删除 `repo_host` 后，`RemoveSourceHost` 不再有“不能删原主 Host”的限制，任何 Host 都可移除。
- `SetRepoHost` 及依赖 `repo_host` 的旧 API 一并删除。
- 旧字段的 JSON/API 投影删除；UI 完全改为 Source × Host。

事务语义沿用 14b：caps + policy + health/recovery + 审计必须同事务；`registry_host_gen` 是 Host 级全局代次，不随单 Source 的 Host 删除而删除。

#### 3.5.14c-C SQLite 表重建

14c 删列不能只改 Canonical DDL，必须按 SQLite 安全重建流程：

1. 保存 `sources` 相关索引、触发器、视图定义。
2. 按最终 Canonical 建新表。
3. 复制保留列。
4. 检查 `AUTOINCREMENT` / `sqlite_sequence`。
5. 删旧表，重命名新表。
6. 恢复索引、触发器、视图。
7. 确认 `source_host_caps` 的外键仍指向重建后的 `sources`，必要时重建该外键。
8. `PRAGMA foreign_key_check`。
9. `PRAGMA integrity_check`。
10. 校验最终 Canonical Hash。

`PRAGMA foreign_keys` 必须在事务开始前按迁移流程设置；不能指望在事务中修改立即生效。

#### 3.5.14c-D 回滚与 RPO

| 时间区间 | 回滚方式 | RPO |
|---|---|---|
| 停服、迁移、隔离验收期间 | 恢复 14c 前 DB + 14b 镜像 | 0 |
| 开放生产流量之后 | 不能直接恢复旧快照并宣称无损 | 需反向迁移或接受明确数据丢失窗口 |

RPO=0 只承诺到开放生产流量之前。开放后若要求 RPO=0，必须实现并实测 14c→14b 反向迁移；不能用“有备份”代替证明。

#### 3.5.14c-E 验收

- `repo_host` 和旧 Registry 字段删除；`github_cap` / `git_cap` / `range_cap` 保留。
- Source × Host API/UI 可用；per-host 启停生效且不影响其他 Host。
- 表重建后 `foreign_key_check` / `integrity_check` / Canonical Hash 通过。
- 停服期间回滚 RPO=0 演练通过；开放流量后的数据丢失窗口有书面确认。
- 反向迁移若未实现，文档明确写“不承诺开放后 RPO=0”。

### 3.5.15 CredStore 业务接入、官方私有 Registry 认证与凭据安全边界闭环

**目标：** 不重建 CredStore。复用 3.5.14 已有的 Host 级加密存储、`registry_host_gen` 代次、`Snapshot`、`Reconcile` 和 Probe 认证基础，把凭据安全接入 Gateway 的真实 Manifest/Blob 回源，并补齐凭据管理入口和官方/候选源隔离。

**依赖：** 3.5.14c（v3.5.14）；3.5.11 的 `source_realm_policy`、`PolicySnapshot` 和 Token Cache 身份键。**代码基线：** `6e07752`（2026-11-17）。

#### 3.5.15.0 范围与非目标

**范围：** 15-A 凭据管理与一致性、15-B Gateway 私有认证与缓存隔离、15-C 身份/Probe/重定向安全、15-D 范围冻结与验收。

**非目标：**

- 不重建 CredStore，不新增平行凭据文件、凭据表或 generation 机制。
- 不重写 Gateway 的代理、调度和流式传输主架构；只增加认证执行、缓存准入和请求身份。
- Docker Hub 私有仓库、GHCR 用户名/PAT 不纳入首批；`credentialHost` 继续拒绝 `docker.io` / `ghcr.io`。
- GitLab PAT / Deploy Token 留在 3.5.15a；GCP ADC / OAuth、AWS ECR IAM、OIDC / 工作负载身份不纳入。
- 不做 per-client、per-repository 授权；本版只建立 Host 级凭据能力，权限边界见 15-D。
- 不新增 Registry 类型，不改 `3.5.16`（公开 Manifest 竞速）和 `3.5.17`（Blob 竞速可行性评估）的版本目标。

**Schema 边界：** 凭据管理、审计、状态机使用现有 `registry_creds.enc`、`registry_host_gen`、`audit` 和 `meta`，不改变 Schema 迁移基线。后续若要增加 official realm 配置、per-client 授权或 per-tenant 缓存身份，属于新的 Schema/配置变更，必须单独定版，不能塞进本版。

#### 3.5.15.1 威胁模型与信任边界

**资产：** Registry 用户名/密码、Token、Authorization、私有 Manifest/Blob、共享缓存内容、`secret.key`。

**不可信面：**

- 第三方候选 Source 及其返回的 `WWW-Authenticate`、重定向和内容。
- 能访问 `/v2/` 的 LAN 客户端（默认匿名，开启 Access Token 后共享令牌）。
- 任意跨 Host、跨 Scheme、跨来源的重定向目标。
- 损坏、截断或被替换的凭据文件、数据库记录和缓存条目。
- 管理员误操作（配错 Host、重复写入、并发修改）。

**信任边界：**

- 官方直连 = `IdentityKind=Official` 且 `SourceID=0`，只能由可信内部入口构造。
- 候选 Source = `IdentityKind=Candidate`，永远不能读取或使用官方凭据、官方私有 Token。
- 共享缓存 = 公共资源；私有/认证响应不得进入，也不得从缓存命中后绕过认证。
- 面板管理员 = 可信操作者；但凭据 API 仍要 CSRF、HTTPS、no-store 和审计。
- Gateway Access Token = 共享访问令牌，不是 per-user 身份；本版不提供 per-client 授权。

**必须成立的不变量：**

1. 官方凭据只用于官方 attempt；候选 Source 即使有合法 Realm Policy 也拿不到用户名密码或官方私有 Token。
2. 需要凭据的请求在缓存查询前就绕过共享缓存；认证失败不得回退到匿名缓存结果。
3. `Authorization` 不跨 Host、不跨 Scheme、不跟随未授权重定向；Token 端点 Basic 不离开 Token 端点来源。
4. 身份零值 = `Unknown`，一律拒绝凭据；`Official` 不能由请求参数、Header、候选源配置或 Challenge 声明。
5. 凭据、Token、Authorization、可逆密文不进入普通日志、审计明文、API 响应和错误体。
6. 单 Host 凭据损坏只隔离该 Host；文件级损坏才全局 fail closed；匿名回源不受影响。
7. 文件与 SQLite 不承诺原子提交；只承诺可检测的 Pending 状态和可恢复的状态机。
8. 没有真实私有 Host 证据时，只能声明“机制通过受控 fixture”，不能声明“私有 Registry 已支持”。

#### 3.5.15-A 凭据管理与一致性

##### 3.5.15-A.1 统一写入接口

- 新增 `PutCredential(host, Credential)`，`Credential` 至少包含 `Username`、`Password`、`TokenMethod`、`TokenAuth`；`Put(rawHost, username, password)` 只保留为兼容包装，内部必须走同一路径，禁止两个写入实现。
- 认证模式在 API 层用枚举表达，避免调用方拼非法字段组合：
  - `basic`：Registry 直接 Basic；`TokenMethod`/`TokenAuth` 不参与。
  - `bearer_token_basic`：Bearer Challenge，Token 端点使用 Basic。
  - `bearer_token_none`：Bearer Challenge，Token 端点不带 Basic。
- 严格校验：`TokenMethod` 只允许空值 / `GET`，`POST` 明确拒绝；`TokenAuth` 只允许空值 / `none` / `basic`；`basic` 必须有非空用户名密码；未知值拒绝写入。`bearer_token_none` 允许空凭据条目，但必须显式标记，不能靠空字符串猜。
- 幂等更新：解密现有凭据，与目标 `Credential` 逐字段比较；完全相同则不写文件、不递增 generation、不失效 Token。仅 `TokenMethod` / `TokenAuth` 变化也算变更。
- 更新语义：`PUT` 是“目标状态整体替换”；`password` 省略表示保留现有密码，显式空串拒绝；删除用 `DELETE`，不能用空密码表达删除。
- Host 规范化：统一走 `credentialHost` / `registryhost.Check`；禁止端口、IP、localhost、私网字面量和未授权 Host。UI 只展示 `credentialCapableHosts()` 返回的 Host，不展示“理论上可配但实际不支持”的 Host。

##### 3.5.15-A.2 凭据一致性状态机

文件和 SQLite 不是一个事务系统。本版不声称原子提交，只承诺以下状态可检测、可恢复；状态按 Host 独立维护。

| 状态 | 含义 | 允许认证 | API 展示 | 恢复方式 |
|---|---|---|---|---|
| `Absent` | 该 Host 没有凭据，数据库摘要为空 | 否（匿名流程） | 未配置 | `PutCredential` |
| `Ready` | 文件记录与数据库 `gen` / `cred_digest` 一致 | 是 | 已配置 + gen | 正常读取 |
| `Pending` | 文件已更新，数据库事务未提交；或删除只完成一半 | 否（fail closed） | 待对账 | 后台 `ReconcileHost` 重试；成功后转 `Ready` / `Absent` |
| `Quarantined` | 该 Host 记录损坏、AEAD 失败或摘要无法对齐 | 否（仅该 Host） | 损坏 | 管理员修复/删除；不影响其他 Host |
| `Error` | 写文件失败、磁盘满、fsync 失败、锁失败 | 是，仅限旧 `Ready` 快照；新写入未生效 | 错误 | 修复磁盘/权限后重试；旧文件仍有效时继续用旧快照 |

**写入顺序（唯一允许的顺序）：**

1. 校验 Host 和 `Credential`；读取现有记录并判断是否幂等。
2. 在 `Store.mu` + 文件 `flock` 内，写临时文件、`fsync`、`rename`、父目录 `fsync`。
3. 在同一临界区内执行数据库事务：`registry_host_gen` 递增、写入 `cred_digest`、写审计事件。
4. 事务成功 → `Ready`；事务失败 → `Pending`，文件保留，后台按 Host 重试；不得回滚文件、不得静默使用旧摘要。
5. 进程在步骤 2 后、步骤 3 前崩溃 → 启动时 `ReconcileHost` 以文件为事实来源，补写摘要并递增 gen；补写前该 Host 为 `Pending`。
6. 进程在步骤 3 后崩溃 → 文件已 durable、DB 已提交，重启后 `Ready`。
7. 删除：文件移除记录；数据库写 `emptyDigest` 并递增 gen；审计写 `credential_delete`；失败同样进入 `Pending`，不得把“文件里没有”当成删除成功。
8. 并发管理写：`Store.mu` + `flock` 串行化；API 带 `expected_gen` 做乐观并发，冲突返回 409；禁止后写覆盖先写。
9. 磁盘满 / `fsync` 失败：临时文件清理，原文件保持不变，数据库不变；保留旧 `Ready` 快照，返回 `Error`。
10. 文件级 JSON/版本损坏 → 所有 Host fail closed；单条记录 AEAD/摘要损坏 → 只隔离该 Host，其他 Host 继续 `Ready`。

**Generation 规则：**

- `registry_host_gen` 行永不删除；删除凭据写 `emptyDigest` 并 `gen+1`，避免重新添加时 ABA。
- 每次有效变更 `gen+1`；幂等更新不递增。
- 凭据变更的失效范围只限该 Host：清该 Host 的 Token、负缓存和认证健康；不清除公共候选健康、不重置 `source_host_caps`、不把候选源标 `Unsupported`。
- Token Cache 的 `CredGen` 必须来自同一次一致性读取；不得先读 gen 再读凭据拼装。
- 数据库恢复/回滚可能让 gen 回退；恢复流程必须清空 Token Cache 并重新校验，不能假设 gen 全局单调。
- 认证执行前重新读取当前 gen，与快照 `Generation` 比较；不一致时放弃本次认证并重新解析一次，仍不一致则 fail closed。该检查是边界，不是原子锁；不能声称覆盖检查与发请求之间的所有竞态。

##### 3.5.15-A.3 管理 API 与 UI

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/credentials` | 列出 credential-capable Host、状态、用户名、模式、gen；不返回密码/Token |
| `GET` | `/api/credentials/{host}` | 单 Host 状态；`Pending` / `Quarantined` 带原因码 |
| `PUT` | `/api/credentials/{host}` | 新增/更新；body `{mode, username, password?, expected_gen?}`；`password` 省略表示保留；更新必须带 `expected_gen` |
| `DELETE` | `/api/credentials/{host}` | 删除；成功 204；`Pending` / 冲突 409 |
| `POST` | `/api/credentials/{host}/verify` | 可选：官方 Host 真实认证验证；不走缓存、不返回 Token；结果只用于运维展示 |

- 写操作必须 `guard(csrf(...))`，只允许管理员会话；GET 也要求管理员登录。
- 凭据 API 必须 HTTPS（或可信反代 `X-Forwarded-Proto=https`）；纯 HTTP 默认拒绝写入，除非显式本地调试开关。
- 响应 `Cache-Control: no-store`；请求体上限 64 KiB；密码/Token 不出现在 URL、query、日志、错误体、审计 detail。
- `PUT` 校验顺序：Host 合法性 → 模式合法 → 字段组合 → `expected_gen` → 写入；任何一步失败不得产生半写文件。
- UI 只显示“已配置/待对账/损坏/错误”、用户名、模式、gen、最近更新时间；没有“查看密码”入口；删除需确认。
- 审计事件：`credential_put`、`credential_delete`、`credential_verify`；记录 host、mode、gen、结果、操作者/IP；不记录密码、Token、Authorization、密文。
- 审计必须可靠：凭据变更的审计与 `registry_host_gen` 更新在同一数据库事务内，或使用同步审计路径；不能只依赖队列满即丢的异步旁路。
- 备份单元：`mirror-lens.db` + `secret.key` + `registry_creds.enc` 必须一起备份/恢复；`secret.key` 缺失时 fail closed，不自动生成新密钥；密钥轮换不在本版。

##### 3.5.15-A.4 启动与运行时对账

- 启动时按 Host 执行 `ReconcileHost`，不得因为一个 Host 失败把整个 `hostCreds` 置 nil。
- 热路径不得每次 `Snapshot` 全量解密所有 Host；按 Host 读取并缓存 `(host, gen)`，全量校验只放启动、对账和管理操作。
- `Reconcile` 失败分类：文件级损坏 → 全局禁用凭据但保留匿名回源；记录级损坏 → 只隔离该 Host。
- `Pending` 状态由后台重试处理；重试只针对该 Host，使用 `flock` 与数据库事务；成功后写审计（`credential_reconcile`）。
- 管理 API 必须能显示 `Pending` / `Quarantined` / `Error`，并允许管理员触发一次 `ReconcileHost`；不能要求重启进程才能修复。
- 匿名回源、公共缓存、非 Registry 路径不受凭据状态影响。

#### 3.5.15-B Gateway 私有认证与缓存隔离

##### 3.5.15-B.1 前置条件

私有认证默认关闭。只有同时满足以下条件，才允许对某个 Host 使用凭据：

1. `registry.private_auth_enabled=true`。
2. Host 在 `registry.private_auth_hosts` 显式 allowlist 内。
3. `auth.require_token=true`；否则启动失败，不能静默降级。
4. 该 Host 的凭据状态为 `Ready`。
5. 请求身份通过 15-C 的 `IdentityKind=Official` 门禁。

- `registry.private_auth_hosts` 必须是 3.5.15-D.1 代码 allowlist 的子集；启动时校验，配置未授权 Host 直接拒绝启动。
- `auth.require_token=true` 但未配置有效 Access Token 时拒绝启动，不能以空令牌放行。
- 强制不变式：`registry.private_auth_enabled=true` ⇒ `auth.require_token=true` 且存在有效 Access Token。运行时 Access Token 被关闭或失效时，私有认证必须在请求路径上立即 fail closed，不能继续使用 Host 凭据。

##### 3.5.15-B.2 请求流程

```text
收到 Registry 请求
  |
  v
Host 是否在私有认证 allowlist？
  |-- 否：走原有匿名流程（缓存行为不变）
  |
  v 是
缓存查询前直接绕过共享缓存（读和写都不进入）
  |
  v
匿名回源（不带凭据）
  |
  +-- 200：直接返回；该 Host 不写共享缓存（BYPASS）
  |
  +-- 401：解析 Challenge
  |     |
  |     +-- 无 Challenge / 非法：401，结束
  |     |
  |     v
  |   身份门禁 + 凭据快照 + gen 复核
  |     |
  |     +-- 失败：401/503，fail closed，不读缓存
  |     |
  |     v
  |   直接 Basic 或 Bearer Token 流程
  |     |
  |     +-- 成功：重试一次，强制 OfficialOnly，不写缓存
  |     |
  |     +-- 失败：准确 401/403；负缓存；不回退匿名缓存
  |
  +-- 403/404/429/5xx：按 15-B.5 分类；不默认用凭据重试
```

- **缓存绕过必须在缓存查询之前**：不能只在认证成功后设置 `no-store`。私有认证 Host 的请求一律不读、不写共享缓存，包括 Manifest、Blob、Range、resume、HEAD 和并行分片。
- **认证后强制 OfficialOnly（全生命周期）**：一旦本次请求使用了凭据，Manifest、Blob、Range/resume、HEAD、重试、hedge、Source 切换和重定向都必须保持 OfficialOnly；不得重新进入候选源竞速，不得把认证后的请求交给第三方候选 Source。OfficialOnly 不是一次认证标志，而是整个请求生命周期的约束。
- Blob CDN 重定向按 15-C.3 的受控无认证 GET/HEAD 规则执行，仍属于官方请求链，不得转候选源。
- **认证失败不回退缓存**：401/403 之后不得读取该 key 的旧缓存并返回；否则等于用旧公共内容掩盖私有认证失败。
- **匿名 200 也不写缓存**：私有认证 Host 的匿名成功响应同样不进入共享缓存，避免“先匿名成功、后转私有”的窗口。未开启私有认证的 Host 不受影响，匿名成功响应维持原有缓存行为。这是对“缓存查询前绕过”的保守解释：Host 级私有认证开启后，该 Host 的所有请求都视为可能涉及私有认证，不做 per-repo 猜测。代价是该 Host 的公共内容也失去共享缓存；本版接受这个代价，per-repo 可见性/租户缓存留到后续版本。

##### 3.5.15-B.3 历史缓存隔离

仅从 3.5.15 开始禁止写入，不能证明升级前的私有内容已经消失。必须做命名空间隔离：

- 在 `meta` 增加 `registry_cache_epoch`（或等价配置），所有 Registry 缓存键加入 epoch 前缀：`docker:m:<epoch>:...`、`docker:b:<epoch>:...`。epoch 必须是持久化状态，不能只是进程内变量。
- 升级到 3.5.15 时递增一次；每次新增私有认证 Host 时递增一次（全局失效，接受一次性成本）；不得按请求递增。旧 epoch 的条目立即不可达。
- epoch 切换必须先于新版本对外提供服务；启动时先读取 epoch 并确认新键空间生效，再开放 Gateway 流量。启动失败或 epoch 切换失败时，不得以旧 epoch 继续服务私有认证请求。
- 滚动升级时，旧实例必须先停止接收私有认证流量，再切换 epoch；不能让旧实例与新实例同时服务私有认证。
- 物理清理由 Gateway 缓存管理模块（现有或新增的缓存管理路径）负责，不放进迁移逻辑；清理任务必须具备限速、进度、失败重试和审计记录。
- 旧 epoch 清理完成前，旧条目也不可被新请求命中；不能只依赖 TTL。清理失败不得回退或重新开放旧 epoch，清理任务继续重试。
- 回滚到 3.5.14c 时，旧二进制不认识 epoch，会按旧键读取；回滚步骤必须先确认旧格式 `docker:*` 缓存不可达并完成清理，清理失败时阻断回滚，不能假定旧缓存安全。
- 验收必须注入“旧 epoch 的私有 Manifest/Blob”，证明匿名请求无法命中，清理后物理条目消失。

##### 3.5.15-B.4 Token Cache

- 保留现有身份键：`SourceID`、`Host`、`Realm`、`Service`、`Scope`、`CredGen`、`PolicyGen`；不得删字段或退化成仅 Host/Realm。
- 本版不增加 `CredDigest`；恢复流程必须清空 Token Cache（见 D.3），不依赖进程重启。
- `Realm` 规范化为 `scheme://host[:port]`，去尾部斜杠；`Scope` 排序、去重、去空；多 scope 用固定分隔符编码后再入键。
- `CredGen` 必须来自官方 resolver 的 `registry_host_gen`；`PolicyGen` 来自策略解析；两者任一不一致都不得复用 Token。
- Token 端点使用 Basic 时补 `account=<username>`（按 Registry 协议要求；不支持该参数的端点忽略）。
- 401 失效：带 Token 的请求仍返回 401 时，删除该 key，重新获取 Token 并重试一次；第二次仍 401 直接返回，不循环。
- 同一 key 的并发刷新用 singleflight；旧 generation 的刷新只能写旧 key，不能覆盖新 generation；不得使用全局“当前 Token”槽位。
- Token 端点 5xx 归为上游认证服务故障（502/503），不计入 Registry Source 健康，不触发候选回退。

##### 3.5.15-B.5 错误分类与健康

| 上游结果 | 处理 | 是否用凭据重试 | 是否影响 Source 健康 |
|---|---|---|---|
| 401 + 合法 Challenge | 进入认证流程 | 是，一次 | 否 |
| 401 无 Challenge / 解析失败 | 返回 401 `auth_challenge_missing` | 否 | 否 |
| 认证后仍 401 | 返回 401 `auth_failed`；负缓存 | 否（同凭据不重试） | 否 |
| 403 匿名且带 Challenge | 允许一次认证尝试 | 是，一次 | 否 |
| 403 匿名且无 Challenge | 返回 403 | 否 | 否 |
| 403 认证后 | 返回 403 `auth_forbidden` | 否 | 否 |
| 404 | 返回 404；候选源可跳过 | 否 | 候选按现有规则，不因认证失败惩罚 |
| 429 | 现有冷却/限流 | 否 | 按现有策略 |
| 5xx | 现有上游故障策略 | 否 | 按现有策略 |
| Realm 不在策略内 | 返回 401/403 `auth_realm_denied`；记录安全事件（限流） | 否 | 否 |
| 凭据 gen 不一致 | 重解析一次；仍不一致返回 503 `auth_stale` | 一次 | 否 |
| Token 端点 401/403 | 返回 401/403 `token_auth_failed` | 否 | 否 |
| Token 端点 5xx | 返回 502 `token_upstream_error` | 否 | 否 |

- 认证失败不得计入 Source 失败、冷却、`Unsupported` 或候选降级；候选源 401/403 只跳过，不惩罚。
- 错误密码必须负缓存：按 Host + 模式 30s 起，指数退避到 5m；凭据变更立即清除；防止把 Registry 账号打到锁定。
- 所有认证错误返回稳定错误码，不返回上游响应体、Token、Authorization 或密码。

#### 3.5.15-C 身份、Probe 与重定向安全

##### 3.5.15-C.1 可信身份

- 引入 `IdentityKind`：`Unknown=0`、`Official`、`Candidate`。零值必须是 `Unknown`，一律拒绝凭据。
- 身份对象使用不可由普通调用方随意构造的形态：私有字段 + 受控构造函数（`NewOfficialIdentity` / `NewCandidateIdentity`），或封装在认证上下文里；仅增加一个导出枚举字段不够，内部代码仍可字面量构造 `Official`。
- `Official` 只能由官方直连入口构造：`SourceID=0`、Host 来自已解析 plan、`PolicySnapshot.Official=true`。不得由请求参数、HTTP Header、候选源配置、Challenge 或缓存内容决定。
- 门禁 API：`AuthorizeCredential(ctx) (CredentialSnapshot, error)`，在凭据读取处和最终认证执行处各校验一次：
  - `IdentityKind=Official`；
  - `SourceID=0`；
  - `PolicySnapshot.Official=true`；
  - `snap.Host == policy.LogicalHost`（规范化后）；
  - `snap.HasCredential=true`；
  - `snap.Generation == policy.CredGen`。
- 官方策略解析必须从 `registry_host_gen` 填充 `PolicySnapshot.CredGen`：`OfficialPolicy.Resolve` 按 `LogicalHost` 读取对应 generation，不得保留默认 0。静态 `OfficialPolicy` 只允许用于不涉及真实凭据的测试；生产路径必须走带 generation 的 resolver。
- generation 读取失败（DB 错误、行缺失、Host 不匹配）不得默认 0 继续认证，必须 fail closed 并返回 `auth_stale` / `auth_unavailable`。
- 解析后同时校验 `snap.Host == policy.LogicalHost`（规范化后）和凭据状态与认证模式一致：`basic` 必须有非空用户名密码；`bearer_token_basic` 必须有 TokenMethod/TokenAuth；`bearer_token_none` 必须显式标记。任何不一致都拒绝认证。
- 认证执行前重新读取当前 `registry_host_gen`；与快照 gen 不一致时重新解析一次，仍不一致 fail closed。不能只依赖快照读取时的检查。
- 候选源永远不能获得官方凭据或官方私有 Token；候选 Token 缓存键必须包含其 `SourceID`，不得与官方 key 合并。

##### 3.5.15-C.2 Probe 与官方私有验证

- 当前 `ListDueHostProbes` 只从 `sources` 生成任务，即只有候选 Source（`SourceID>0`），没有官方直连探测；`HostProbeRunner` 现在把官方 Host 凭据快照和候选 `SourceID` 一起传给 Probe。这两个事实意味着不能只加认证门禁就认为 Probe 安全。
- **候选 Source 探测：不读取、不传递官方凭据。** `CredentialSnapshot` 为空；401/403 记为 `auth_error` / `Inconclusive`，不得标 `Unsupported`，不得惩罚候选源。
- **官方私有 Registry 验证：独立入口、独立身份、独立结果。** 使用 `IdentityKind=Official`、`SourceID=0`、不走缓存；结果只用于凭据验证/运维展示，不写 `source_host_caps`、`registry_host_recovery` 或候选健康。
- 两种结果分开记录：候选可用性 ≠ 官方私有认证能力；匿名探测成功 ≠ 官方私有认证成功。
- `Probe` 与 `Gateway` 共用 Challenge 解析、Realm 判定、Token 参数、Basic 规则和错误分类，但**不共用官方凭据**；凭据只由官方 Gateway attempt 和官方 verify 使用。

##### 3.5.15-C.3 重定向

| 请求类型 | 规则 |
|---|---|
| Registry API | 最多 5 跳；每跳 SSRF 检查；只允许 HTTPS；Host 或 Scheme 变化时清除 `Authorization`；跨 Host 不重新认证；同 Host 也重新校验策略 |
| Token 端点 | 最多 3 跳；只允许同源 HTTPS；跨源重定向直接拒绝；任何变化都清除 Basic；不得把 Basic 带到其他来源 |
| Blob CDN | 只允许 GET/HEAD；只允许 HTTPS；不携带 `Authorization` / Cookie；保留签名 query；每跳 SSRF 检查；最多 5 跳；不记录 query |

- HTTPS → HTTP 一律拒绝，不允许“先降级再重定向”。
- 跨 Host 必须清除认证头；跨 Scheme 必须重新检查安全边界；不能依赖 Go 默认的同域/子域复制行为。
- Token 是不透明字符串，不能从 Token 内容判断归属；Token 归属只由缓存键和受控获取上下文保证。
- 未授权目标必须停止凭据转发，不能“先发再根据响应判断”。
- 重定向次数必须有限制；每次重定向都重新经过 SSRF 检查。

##### 3.5.15-C.4 Realm 授权

- 官方直连：只允许同 Host 和 `DefaultRealmsFor(host)` 的内置 Realm；本版不提供 official realm 管理入口，不引入新 Schema。
- 候选 Source：继续使用 `source_realm_policy`；候选即使有合法 Realm，也不能获得官方凭据。
- 外部 Token Realm（Harbor/Keycloak 等）不在 3.5.15 支持范围；需要单独版本和配置/ Schema 设计，不能靠候选策略绕过。

#### 3.5.15-D 支持范围、回滚与验收冻结

##### 3.5.15-D.1 私有 Host allowlist

**原则：** 首批使用明确 allowlist，不开放任意 Host；不为测试方便扩大生产凭据发送范围。

| Host | 私有仓库 | 认证方式 | 直接 Basic | 本版状态 |
|---|---|---|---|---|
| `quay.io` | 有 | Bearer + Token 端点 Basic | 按真实 fixture | `PENDING_REAL_FIXTURE` |
| `gcr.io` | 有 | Bearer + Token 端点 Basic | 按真实 fixture | `PENDING_REAL_FIXTURE` |
| `registry.gitlab.com` | 有 | PAT / Deploy Token | 不适用 | 3.5.15a |
| `registry.k8s.io` | 无 | 匿名 | 否 | `ANONYMOUS_ONLY` |
| `mcr.microsoft.com` | 无 | 匿名 | 否 | `ANONYMOUS_ONLY` |
| `lscr.io` | 无 | 匿名 | 否 | `ANONYMOUS_ONLY` |
| `public.ecr.aws` | 无 | 匿名 | 否 | `ANONYMOUS_ONLY` |
| `cgr.dev` | 无 | 匿名 | 否 | `ANONYMOUS_ONLY` |
| `*.pkg.dev` | 有 | GCP ADC / `_json_key` | 不适用 | `NOT_SUPPORTED`（本版） |
| `docker.io` / `ghcr.io` | 仅公共 | 匿名 | 否 | 私有 `NOT_SUPPORTED` |

- 未列出的内置 Host 默认 `ANONYMOUS_ONLY`；本版不为它们开放凭据入口。
- `credentialHost` 继续拒绝端口、IP、localhost、私网字面量和未授权 Host；生产 allowlist 不因测试放宽。
- `*.pkg.dev` 本版不支持凭据：`credentialHost` 应排除，避免 UI 让用户配置一个用不了的 Host；公共匿名拉取保持现有路径。
- 直接 Basic 是协议能力，不等于每个 Host 都使用；Host 状态按真实 Challenge 和真实 fixture 记录。
- 现有 `tokenAuthMode` 的 Basic 只覆盖 Token 端点认证，不能作为 Registry 直接 Basic 已支持的证据；两者必须分别有实现和真实 fixture。
- UI 只展示 `credentialCapableHosts()`；`ANONYMOUS_ONLY` / `NOT_SUPPORTED` 不显示“添加凭据”入口。
- 测试环境固定采用“隔离容器 + 443 + allowlist 域名 + 测试 CA”；测试 CA 只注入测试环境，不改变生产信任根，禁止关闭 TLS 校验；测试域名固定解析到隔离环境，不允许任意解析到公网目标；测试 Host 只存在于测试构建或测试配置，生产二进制和默认 allowlist 不包含测试域名；不得把 `localhost:5000` 或任意 IP 加入生产 allowlist。

##### 3.5.15-D.2 客户端访问控制

- 私有认证默认关闭；`registry.private_auth_enabled=false`。
- 开启私有认证必须同时 `auth.require_token=true`；装配时校验，不满足直接拒绝启动。
- 运行时 Access Token 被关闭或失效时，私有认证必须立即失效（fail closed）：新请求不得使用 Host 凭据；在途请求不得再发起新的凭据认证或候选回退。该检查在请求路径执行，不能只依赖启动校验。
- Access Token 是共享令牌，不是 per-user 身份；任何持有有效令牌的客户端都能使用 Host 级凭据访问该凭据可访问的所有私有仓库。
- UI 和文档必须写明 Host 级 blast radius；本版不宣称 per-client、per-repository 授权。
- 未开启 Access Token 时，私有认证请求不得生效；不能靠“只在 UI 隐藏”实现。

##### 3.5.15-D.3 回滚与降级

- 本版不改 Schema；回滚以二进制/配置为主。
- 关闭 `registry.private_auth_enabled`：停止使用凭据，恢复匿名流程；凭据保留但不再生效；Token Cache 清空或按 Host 失效；共享缓存行为恢复。
- 回滚到 3.5.14c 前必须处理两件事：
  1. 3.5.14c 的 `HostProbeRunner` 仍会把官方 Host 凭据传给候选 Source；若 `registry_creds.enc` 存在，会重新引入 P0 泄漏。回滚前必须备份并删除凭据文件，或确认 HostProbe 不使用凭据。
  2. 3.5.14c 不认识新的 cache epoch，会按旧键读取；回滚前必须确认旧格式 `docker:*` 缓存不可达并完成清理，清理失败时阻断回滚。
- 坏凭据导致 401 风暴：负缓存 + 关闭开关；不惩罚 Source，不影响匿名路径。
- 文件/DB 不一致：按 Host `Pending`/`Quarantined`；匿名路径不受影响；管理员可 `ReconcileHost`。
- 恢复硬性顺序：
  1. 恢复 DB、`secret.key`、`registry_creds.enc` 作为一个备份单元。
  2. 完成凭据文件与 DB 对账，校验 generation 和摘要。
  3. 清空进程内 Token Cache。
  4. 增加或切换 `registry_cache_epoch`。
  5. 确认旧缓存不可达。
  6. 最后开放 Gateway 流量。
- 不重启进程的在线数据库恢复，必须显式失效 Token Cache，不能依赖“重启会清空”这个假设。
- `secret.key` 缺失时 fail closed，不自动生成新密钥。

##### 3.5.15-D.4 发布门禁

- 15-A/15-B/15-C/15-D 全部通过前，不宣称“私有 Registry 已支持”。
- 私有认证默认关闭；未同时满足 allowlist、Access Token、身份门禁、缓存绕过、历史缓存隔离时，不得开启。
- 没有真实私有 Host 证据时，只能写“机制通过受控 fixture”；真实 fixture 失败时必须缩小 3.5.15 的支持声明（Host 或认证方式），不得用模拟测试替代。`PENDING_REAL_FIXTURE` 不能写成 `REAL_PASS`。
- 验收状态只允许 `REAL_PASS` / `SIMULATED_PASS` / `BLOCKED_EXTERNAL_SOURCE` / `NOT_EXECUTED`；公共仓库回源成功不能写成私有 `REAL_PASS`。
- `3.5.16` / `3.5.17` 保持原规划；认证运维能力另排版本，不占用这两个号。

#### 3.5.15 验收矩阵

**证据状态（不得互相替代）：**

- `REAL_PASS`：真实私有 Registry 账号完成实际认证和内容读取。
- `SIMULATED_PASS`：隔离测试环境通过（隔离容器 + 443 + allowlist 域名 + 测试 CA）。
- `BLOCKED_EXTERNAL_SOURCE`：账号、网络、权限或外部服务条件不足。
- `NOT_EXECUTED`：尚未执行，不能当作通过。

每项还必须标注证据类型：`代码测试` / `真实进程` / `真实私有 Host`。

##### 功能验收

| 编号 | 验收项 | 必须证明 | 失败判定 |
|---|---|---|---|
| F01 | 公共匿名回归 | 原有匿名 Manifest/Blob 回源不退化 | 任一匿名路径需要凭据或返回认证错误 |
| F02 | 私有直接 Basic Manifest | 真实私有 Basic fixture（官方 Host 或受控 fixture）完成 Manifest 认证 | 只验证 Token 端点 Basic |
| F03 | 私有 Bearer Manifest | Challenge、Realm、Token 流程完整 | 只验证匿名 Token |
| F04 | 私有 Blob | Blob 使用正确身份且 Digest 一致 | Manifest 成功但 Blob 401/403 或身份串用 |
| F05 | 错误密码 | 返回真实 401 `auth_failed` | 改写成源不可用、404 或匿名成功 |
| F06 | 403 | 不触发密码重试 | 用凭据重试并掩盖原始状态 |
| F07 | 404 | 不触发凭据重试，不污染源健康 | 用凭据重试或标 Unsupported |
| F08 | 429/5xx | 遵循既有冷却与故障策略 | 用凭据重试 |
| F09 | 凭据新增/修改/删除 | API/UI 成功，重启后仍有效 | 任一步留下半状态 |
| F10 | 凭据修改 | gen 更新，新请求使用新凭据 | 新请求仍用旧 gen |
| F11 | 凭据删除 | 后续请求不再使用旧凭据/旧 Token | 删除后仍命中旧身份 |
| F12 | 凭据文件损坏 | 文件级 fail closed；记录级只隔离该 Host | 单条损坏导致全部 Host 失效 |
| F13 | 摘要不一致 | 拒绝使用并产生可审计错误 | 静默使用旧快照 |
| F14 | Token 缓存隔离 | Source/Host/Realm/Service/Scope/CredGen/PolicyGen 不串用 | 任一维度串用 |
| F15 | HEAD | HEAD 走同一认证与缓存绕过规则 | HEAD 绕过认证或命中私有缓存 |
| F16 | Range/resume | 分片/断点使用同一身份，不写 `.part` | 分片绕过缓存或身份串用 |
| F17 | Token 过期/401 | 删除旧 key，刷新一次，不循环 | 一直 401 到 TTL 或无限重试 |
| F18 | 删除幂等 | 已删除再删返回 204/明确状态 | 误报记录丢失或半删除 |
| F19 | 幂等更新 | 相同内容不写文件、不 bump gen | 重复保存导致 token/探测抖动 |
| F20 | 重启/恢复 | 重启、Pending、Quarantined 状态可解释 | 重启后状态丢失或全部禁用 |

##### 安全验收

| 编号 | 验收项 | 必须证明 | 失败判定 |
|---|---|---|---|
| S01 | 官方请求使用凭据 | 同时满足 Official、SourceID=0、Host/gen 一致 | 任一条件缺失仍发凭据 |
| S02 | 候选源访问官方 Host | 不获得官方用户名密码 | 候选收到 Basic |
| S03 | 候选源访问官方 Realm | 不获得官方私有 Token | 候选复用官方 Token |
| S04 | 候选源伪造 Official | 无法通过最终认证门禁 | 字面量构造的 Official 可用 |
| S05 | 非白名单 Realm | 不发送凭据 | 向未授权 Realm 发 Basic |
| S06 | 同 Host 不同 Source | Token 不跨 Source 共享 | 候选命中官方 key |
| S07 | 跨 Host 重定向 | 不转发原始 Authorization | 认证头跨 Host |
| S08 | 重定向至未授权 Realm | 不发送凭据、不请求 Token | 先发再判断 |
| S09 | 日志检查 | 不出现密码、Token、Authorization、可逆密文 | 任一敏感值进日志 |
| S10 | API 响应检查 | 不返回凭据明文/可逆密文/Token | 错误体或列表泄露 |
| S11 | 凭据轮换 | 旧 generation 不覆盖新 generation | 旧刷新覆盖新状态 |
| S12 | 并发刷新 | 旧 Token 刷新不污染新代次缓存 | 新 key 被旧结果覆盖 |
| S13 | 私有缓存泄漏 | 私有请求连续两次均绕过共享缓存；匿名客户端无法命中 | 任一私有内容进入共享缓存 |
| S14 | 历史缓存 | 升级前旧 epoch 私有内容不可匿名命中，清理后物理消失 | 旧缓存仍可命中 |
| S15 | Unknown 身份 | 零值身份不能获取凭据 | Unknown 被当成 Official |
| S16 | 伪造 Official | 普通调用方无法构造可用官方上下文 | 导出字段可随意构造 |
| S17 | Token 端点重定向 | 不向其他来源发送 Basic | Basic 跨源 |
| S18 | HTTPS 降级 | HTTPS→HTTP 重定向被拒绝 | 降级后继续认证 |
| S19 | CDN 重定向 | 无认证 GET/HEAD 可按规则访问获准 CDN | 带 Authorization 访问 CDN |
| S20 | 记录级损坏 | 单 Host 损坏不影响其他 Host | 全局禁用 |
| S21 | 文件/DB 故障恢复 | 崩溃后可恢复 Pending，状态可解释 | 必须重启或永久 pending |
| S22 | 审计耐久性 | 凭据变更审计不因队列满静默丢失 | 审计缺失 |
| S23 | 客户端访问控制 | 私有认证开启时未授权客户端无法使用 Gateway | 无令牌仍可拉私有 |
| S24 | 认证失败退避 | 错误密码不会持续高频请求导致锁定 | 每请求都打 Token 端点 |

##### 并发、恢复与性能验收

| 编号 | 验收项 | 必须证明 |
|---|---|---|
| C01 | 两连接并发 | 两个独立 SQLite 连接，在 Snapshot 与 gen 校验之间注入修改，断言旧快照被拒绝 |
| C02 | 旧/新 generation | 旧 gen Token 刷新只写旧 key，不覆盖新 gen |
| C03 | 多进程写同一 Host | `flock` + DB 事务串行化，无丢失更新 |
| C04 | 文件后 DB 前崩溃 | 启动 `ReconcileHost` 补写摘要，Pending→Ready |
| C05 | DB 后崩溃 | 文件已 durable，重启 Ready，不重复 bump |
| C06 | 并发删除/更新 | 结果确定，无半删除/半更新 |
| C07 | singleflight | 同 key 并发只打一次 Token 端点；不同 key 不合并 |
| C08 | 缓存绕过并发 | 私有/公共请求并发时不串缓存、不互相污染 |
| R01 | 文件新于 DB | 启动对账以文件为准，补写摘要，gen+1 |
| R02 | DB 新于文件 | 该 Host Quarantined；其他 Host 不受影响 |
| R03 | 单条损坏 | 只隔离该 Host；文件级损坏才全局 fail closed |
| R04 | `secret.key` 缺失 | fail closed，不自动生成新密钥 |
| R05 | 磁盘满/fsync 失败 | 原文件不变，旧 Ready 继续可用，返回 Error |
| R06 | 备份恢复 | DB + key + 凭据文件作为单元恢复，状态可解释 |
| P01 | 匿名路径 A/B | 3.5.14c vs 3.5.15，同环境重复≥5 次，报告 P50/P95/P99、吞吐、错误率；预先确定 P95 阈值 |
| P02 | 认证路径 | 单独统计 Basic/Bearer、Token 命中/未命中，不与匿名混算 |
| P03 | 50 Host 成本 | Snapshot/对账在 50 Host 下的延迟与锁竞争 |
| P04 | Token Cache | 命中率、singleflight 合并率、401 刷新率 |
| P05 | 缓存命中 vs 绕过 | 公共 Host 缓存不退化；私有认证 Host 明确记录 BYPASS 成本 |

#### 3.5.15 决策记录与最终复核裁定

**状态：** 五项待决事项已形成明确决策；3.5.15 进入最终设计复核。真实私有账号验证未完成前，相关验收保持 `BLOCKED_EXTERNAL_SOURCE`，不能宣称“私有 Registry 已支持”。

##### 决策记录

| 编号 | 决策 | 裁定 |
|---|---|---|
| D-15-1 | Registry 直接 Basic | 纳入 3.5.15；与 Token 端点 Basic 分开实现、分开验收 |
| D-15-2 | 私有 Host 范围 | 明确 allowlist；不开放任意 Host、端口、IP、localhost、私网 |
| D-15-3 | 私有认证与 Access Token | 私有认证默认关闭；开启必须 `auth.require_token=true`，否则拒绝启动 |
| D-15-4 | 认证/私有响应缓存 | 私有认证 Host 的请求在缓存查询前绕过共享缓存；认证失败不回退缓存 |
| D-15-5 | 官方 Realm | 本版只支持同 Host 和已有内置 Realm；外部 Realm 留后续版本 |
| D-15-6 | 版本号 | `3.5.16` 公开 Manifest 竞速、`3.5.17` Blob 竞速评估保持不变 |
| D-15-7 | `*.pkg.dev` | 本版不支持凭据；`credentialHost` 排除，公共匿名拉取不变 |
| D-15-8 | 客户端授权 | 不做 per-client / per-repository 授权；明确 Host 级 blast radius |
| D-15-9 | 审计 | 凭据变更审计与 gen 更新同事务或同步写；不接受队列满静默丢失 |
| D-15-10 | 真实私有账号 | 外部实测前保持 `BLOCKED_EXTERNAL_SOURCE`；不得用公共仓库或模拟测试替代 |
| D-15-11 | Basic/Bearer fixture | 固定采用隔离容器 + 443 + allowlist 域名 + 测试 CA；不改变生产信任根，不关闭 TLS 校验 |
| D-15-12 | 历史缓存清理 | `registry_cache_epoch` 负责逻辑隔离；Gateway 缓存管理模块负责物理清理，限速、进度、重试、审计；清理失败不重开旧 epoch |
| D-15-13 | 私有 Host 公共内容缓存 | 接受失去共享缓存；3.5.15 不做 per-repo 公私识别 |
| D-15-14 | Token Key `CredDigest` | 不增加，维持七字段；恢复时强制清空 Token Cache |
| D-15-15 | 私有认证与 Access Token | `PrivateAuthEnabled=true` ⇒ `GatewayAccessTokenRequired=true`；运行时关闭 Access Token 使私有认证失效 |
| D-15-16 | OfficialOnly | 贯穿 Manifest、Blob、重试、hedge、Source 切换和重定向全生命周期 |
| D-15-17 | epoch 与回滚 | epoch 持久化；切换先于对外服务；回滚前确认旧缓存不可达并完成清理；清理失败阻断回滚 |

##### 最终复核裁定（2026-11-17）

1. **真实私有账号**：外部实测前保持 `BLOCKED_EXTERNAL_SOURCE`。验收状态只允许 `REAL_PASS`（真实私有 Registry 账号完成实际认证和内容读取）、`SIMULATED_PASS`（隔离测试环境通过）、`BLOCKED_EXTERNAL_SOURCE`（账号、网络、权限或外部服务条件不足）三种，未执行记 `NOT_EXECUTED`；这些状态不得互相替代。quay.io / gcr.io 公共仓库回源成功不能作为私有认证成功的证据。
2. **Basic/Bearer fixture**：固定采用“隔离容器 + 443 + allowlist 域名 + 测试 CA”。测试 CA 只注入测试环境，不改变生产信任根，禁止通过关闭 TLS 校验简化测试；测试域名固定解析到隔离环境，不允许任意解析到公网目标；不把 `localhost:5000` 或任意 IP 加入生产 allowlist。
3. **历史缓存清理**：`registry_cache_epoch` 只负责逻辑隔离；物理清理由 Gateway 缓存管理模块负责，不放进迁移逻辑。清理任务必须具备限速、进度、失败重试和审计记录；清理失败不能重新开放旧 epoch。回滚前必须确认旧格式缓存不可达并完成必要清理；清理失败时阻断回滚，不能假定旧缓存安全。epoch 切换必须先于新版本对外提供服务，不能出现新版本已接受请求但历史缓存隔离尚未生效的窗口。
4. **公共内容失去共享缓存**：接受该 Host 公共内容失去共享缓存。3.5.15 不引入仓库级私有/公共内容识别，因为需要额外证明 Manifest 仓库权限识别、Blob 跨仓库复用、Tag/Digest/多架构 Manifest 一致性和缓存键授权身份覆盖；这些超出本版范围。per-repo 可见性/租户缓存留后续版本。
5. **Token Key 是否增加 `CredDigest`**：不增加，维持 `SourceID`、`Host`、`Realm`、`Service`、`Scope`、`CredGen`、`PolicyGen` 七字段。恢复流程写成硬性顺序：恢复 DB、`secret.key`、`registry_creds.enc` → 完成凭据文件与 DB 对账 → 校验 generation 和摘要 → 清空进程内 Token Cache → 增加或切换 `registry_cache_epoch` → 确认旧缓存不可达 → 最后开放 Gateway 流量。存在不重启进程的在线数据库恢复场景时，必须显式失效 Token Cache，不能依赖进程重启。
6. **外部 Token Realm**：本版不承诺；Harbor/Keycloak 等外部 Token Realm 单独立项，需单独配置/Schema 设计，不能靠候选策略绕过。

##### 必须检查的三个设计细节

1. **私有认证开关与 Access Token**：`PrivateAuthEnabled=true` 必须推出 `GatewayAccessTokenRequired=true`。运行时 Access Token 被关闭或失效时，私有认证必须立即失效（fail closed），不能继续使用 Host 凭据；该检查在请求路径上执行，不能只依赖启动校验。
2. **OfficialOnly 全生命周期**：不能只在第一次认证成功时设置。Manifest、Blob、Range/resume、HEAD、重试、hedge、Source 切换和重定向都必须遵守同一身份约束；认证后的请求不得重新进入候选源竞速，也不得交给第三方候选 Source。OfficialOnly 是整个请求生命周期的约束，不是一次认证标志。
3. **epoch 与回滚语义**：`registry_cache_epoch` 必须是持久化状态（`meta`），不能只是进程内变量。新版本启动失败或 epoch 切换失败时，不得以旧 epoch 继续服务私有认证请求。回滚到 3.5.14c 前必须完成缓存清理和验证；清理失败时回滚流程必须阻断，不能假定旧缓存安全。

### 3.5.15a GitLab PAT / Deploy Token 专项

**目标：** 单独验证 GitLab 专用凭据，不混入 3.5.15 的标准 Foreign Registry 认证。

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

- 公开竞速：3.5.11 + 3.5.14c
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

#### 3.5.16-H 认证运维硬化（附加工作块）

> 本工作块是 3.5.15 验收遗留项的后续承载，不替代 3.5.16 的公开 Manifest 竞速目标；若排期冲突，Manifest 竞速优先，本工作块顺延。

**H.1 私有凭据 Host 范围扩展（P03 产品级）**

- 现状：`credentialCapable` 仍是代码内 `gcr.io` / `quay.io` 两项，P03 只能做存储层 fixture，不能算产品级 50 Host。
- 目标：把凭据 Host allowlist 改为显式配置驱动，仍禁止通配符、端口、IP、私网字面量和 `*.pkg.dev`。
- 启动校验：`registry.private_auth_hosts` 必须是配置 allowlist 的子集；非法项拒绝启动，不能静默忽略。
- 验收：50 个显式 Host 的 `ListStatus`、写入、删除、对账、UI 列表；P03 在 50 Host 下达到阈值；任何未列入 allowlist 的 Host 仍不可写入/发送凭据。
- 非目标：不为跑分临时放宽 allowlist；不改变 3.5.15 的默认两项范围。

**H.2 恢复与故障注入硬化**

- 目标：把 3.5.15 只靠单元测试覆盖的恢复路径，升级为真实进程/文件系统演练。
- 场景：写入过程中 `kill -9`；磁盘满；`fsync` 失败；文件与 SQLite 摘要不一致；备份恢复；Pending 后台重试；回滚到 3.5.14c 前的缓存清理。
- 要求：失败后不得出现假 Ready；不得匿名命中旧私有缓存；审计记录恢复结果；故障注入点只能存在于测试构建，不进入生产路径。
- 验收：每个场景有可重复脚本、真实退出码/日志、恢复前后状态对照；无法自动化的场景必须写明人工步骤和判定标准。

**3.5.15 延期项映射**

| 3.5.15 验收项 | 延期到 | 延期原因 |
|---|---|---|
| C03 多进程写同一 Host | 3.5.16-H.2 | 3.5.15 只有单进程并发测试；需真实两进程 + `flock` + DB 事务演练 |
| C05 COMMIT 后崩溃 | 3.5.16-H.2 | 需在 COMMIT 后精确 `kill -9`，当前只有幂等等价证明 |
| R05 磁盘满/fsync 失败 | 3.5.16-H.2 | 需故障注入点与文件系统级演练，不能靠单元测试代替 |
| S21 真实 Pending 窗口崩溃 | 3.5.16-H.2 | 后台 `retryPendingCreds` 已实现并有调度测试；真实进程在 Pending 窗口内崩溃仍需 H.2 演练 |
| P03 50 Host 产品级 | 3.5.16-H.1 | 产品 allowlist 仍为 `quay.io`/`gcr.io`；扩 Host 属 H.1，不能用存储层 fixture 冒充 |

F09 面板 UI 按用户指示忽略，不纳入 3.5.15 延期项；其 API 生命周期已通过。

**H.3 公网 CDN 302 验收**

- 3.5.15 已完成本地 TLS 302 fixture；本节补一个可控公网 302 端点，验证 CDN 跳转不带 `Authorization`/`Cookie`、保留签名 query、拒绝 HTTPS 降级和 POST。
- 若没有可控公网端点，保持 `SIMULATED_PASS`，不能写成 `REAL_PASS`。

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
| 3.5.4 架构债：一源多 Host | 3.5.14a + 3.5.14b + 3.5.14c | 先回填投影，再切换权威读路径，最后删除旧字段 |
| 3.5.10 Source Realm Draft | 3.5.11 | 3.5.10 已被标签修复占用 |

## 7. 版本门禁

1. 3.5.11 未通过前，不进入 3.5.12 的 Host 实现。
2. 3.5.12 按 Host 门禁声明：只有真实回源通过的 Host 才能声明支持，阻塞 Host 必须标注“Source 可用性阻塞”。
3. 3.5.12 完成前，不宣称第一批 Registry 全部支持。
4. 3.5.14a 可以回填、投影和双写，但不得切换候选读路径。
5. 3.5.14a 的 `backfillHostCaps` 未通过行数、冲突和双用途源校验前，不进入 14b。
6. 3.5.14b 完成前，不为任何 Source 手工添加多 Host 标签。
7. 3.5.14b 完成前，不把 Docker Hub / GHCR 的新旧能力模型混用。
8. 3.5.14c 完成前，不宣称 3.5.14 已完成零残留、单轨运行或旧结构清理。
9. 3.5.15-A/15-B/15-C/15-D 全部通过前，不宣称支持私有 NVCR / 私有 ECR，也不宣称“私有 Registry 已支持”。
10. 3.5.15 的凭据授权门禁未通过前，不开放凭据管理 API/UI；门禁必须同时满足 `IdentityKind=Official`、`SourceID=0`、`PolicySnapshot.Official=true`、Host/gen 一致、凭据状态 `Ready`。
11. 3.5.15 私有认证默认关闭；未同时满足 `registry.private_auth_enabled=true`、Host 在 `registry.private_auth_hosts`、`auth.require_token=true`、缓存绕过和历史缓存隔离时，不得开启。
12. 3.5.15 的 S13～S24、C01～C08、R01～R06 未通过前不得发布；`go test -race` 不能替代 SQLite 多连接并发与崩溃恢复验收。
13. 3.5.15a 完成前，不宣称支持私有 GitLab。
14. 3.5.16 默认关闭，不能用全局 Hedge 隐式开启。
15. 3.5.17 不是交付承诺，未评估通过不排期。
16. 3.5.15 未完成 epoch 持久化、历史缓存清理和回滚验证前，不开放私有认证流量。
17. 3.5.15 的 `REAL_PASS` 只能由真实私有 Registry 账号产生；`SIMULATED_PASS` / `BLOCKED_EXTERNAL_SOURCE` 不得写成 `REAL_PASS`。

## 8. 回归门禁

### G0 流式内容一致性回归

这不是“已知未关闭 P0”，而是后续版本的回归门禁。

3.5.11、3.5.14b、3.5.14c、3.5.16 合并前必须重新执行：

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

1. 3.5.14 拆成 3.5.14a（回填 + 投影 + 新写路径）、3.5.14b（只切权威读路径）和 3.5.14c（旧结构清理）；14a/14b/14c 是内部里程碑，最终发布 `v3.5.14`。
2. 14a 的 `SetCapsLive` 和 `CommitHostProbe` 必须在同一事务内写 `source_host_caps` + 旧字段投影。
3. 14b 只包含新读路径，不包含旧读路径、dual-read 开关或 feature flag；回滚靠重新部署 14a 镜像。
4. 14a 与 14b 使用同一 Schema 版本；14b 不改表、不改 `schema_meta`；14c 改 Schema 并单独验收。
5. 状态模型冻结为 `last_verdict`（Supported / Unsupported / Inconclusive / Mismatch）+ `effective_status`（Unknown / Supported / Unsupported / Mismatch）；候选准入只看 `effective_status`。
6. `Mismatch` 是硬排除，不按普通 Unsupported 的 6 小时周期重试；只能管理员显式重置后重探。
7. `registry_prefix` 冻结为布尔，映射旧 `ghcr_prefix`，不做枚举。
8. `source_realm_policy` 一行一个 `(source_id, host)`：`primary_realm` + `extra_realms` JSON（最多 3 个）+ 每个 Host 一个 `policy_gen`；不按 Realm 行分散 generation。
9. GitLab PAT / Deploy Token 独立为 3.5.15a。
10. 无公共 Source 的 Host 允许用受控 Source 故障边验收代码路径，但对外必须标记“Source 可用性阻塞”，不能宣称已有公共加速能力。
11. 认证身份通过每次 attempt 的 request context 传递，不依赖静态 `adapter.Plan.SourceID`。
12. 3.5.14a 的离线迁移走 `3.5.7 → 3.5.11 → 3.5.14a`；3.5.7→3.5.11 复用现有 `backfillPolicies`，新增 3.5.11→3.5.14a 的 `source_host_caps` 回填。
13. 迁移采用临时库 + 原子发布；失败删除临时库和 sidecar，不保留半迁移状态；不宣称“单事务迁移”。
14. 3.5.11 分 11a/11b/11c；**11c 原计划未实施，已并入 3.5.12 的 12c**；完成后 Source Realm 只保留 `source_realm_policy`，官方直连走独立 `OfficialPolicy`（不是 fallback）；运行时数据库写入单事务；Token Cache 版本化失效；Schema 迁移离线 + 原子发布；无旧 Realm fallback；Schema 回滚靠备份。
15. 14a 的旧 Foreign 投影只对应原 `repo_host`；`docker.io`/`ghcr.io` 使用各自协议槽位；新增 Foreign Host 不覆盖旧字段。
16. 14c 删除 `repo_host` 和旧 Registry 能力字段，不保留“主 Host”；`github_cap` / `git_cap` / `range_cap` 保留。
17. 14c 开放生产流量前 RPO=0；开放后不承诺 RPO=0，除非实现并实测 14c→14b 反向迁移。
18. 3.5.15 不重建 CredStore；范围冻结为 15-A 凭据管理与一致性、15-B Gateway 私有认证与缓存隔离、15-C 身份/Probe/重定向安全、15-D 范围冻结与验收。
19. 3.5.15 的用户凭据使用门禁为 AND，但不再以 `SourceID == 0` 单独承担身份边界：必须 `IdentityKind=Official`、`SourceID=0`、`PolicySnapshot.Official=true`、`snap.Host == policy.LogicalHost`、`snap.Generation == policy.CredGen`、`snap.HasCredential=true`；`IdentityKind` 零值 Unknown 一律拒绝，身份对象用受控构造函数。
20. 3.5.15 私有认证默认关闭；开启必须 `auth.require_token=true`；Access Token 是共享令牌，不做 per-client/per-repository 授权，Host 级 blast radius 必须写入 UI/文档。
21. 3.5.15 私有认证 Host 的请求在缓存查询前绕过共享缓存（读和写）；认证失败不回退匿名缓存；认证后 OfficialOnly 贯穿全生命周期。
22. 3.5.15 引入 `registry_cache_epoch` 命名空间隔离历史缓存；升级和每次新增私有认证 Host 时递增 epoch（全局失效，一次性成本），旧条目不可达并由 Gateway 缓存管理模块限速清理；回滚到 3.5.14c 前确认旧格式缓存不可达并完成清理，清理失败阻断回滚。
23. 3.5.15 私有 Host 使用明确 allowlist；不开放任意 Host、端口、IP、localhost、私网；`*.pkg.dev` 本版不支持凭据；Docker Hub/GHCR 私有不纳入。
24. 3.5.15 纳入 Registry 直接 Basic，并与 Token 端点 Basic、用户名密码换 Bearer 三种语义分开实现/验收。
25. 3.5.15 文件与 SQLite 不承诺原子提交；使用 `Ready/Pending/Quarantined/Error` 状态机、按 Host `ReconcileHost`、后台重试和单 Host 隔离；启动对账失败不得把整个 CredStore 置空。
26. 3.5.15 凭据变更审计与 `registry_host_gen` 更新同事务或同步写；备份单元为 DB + `secret.key` + `registry_creds.enc`。
27. 3.5.15 官方 Realm 本版只支持同 Host 和已有内置 Realm；外部 Realm 留后续版本。
28. 3.5.16 保持公开 Manifest 竞速，3.5.17 保持 Blob 竞速可行性评估；认证运维能力不占用这两个版本号。
29. 生产目录没有 `registry_creds.enc` 只作为部署基线记录，不作为代码安全验收通过的证据；必须先通过 15-C 的授权门禁和缓存隔离，再开放凭据管理 API/UI。
30. 真实私有账号未实测前，相关验收保持 `BLOCKED_EXTERNAL_SOURCE`；`REAL_PASS` / `SIMULATED_PASS` / `BLOCKED_EXTERNAL_SOURCE` 三态不得互相替代，公共仓库回源成功不能写成私有 `REAL_PASS`。
31. Basic/Bearer fixture 固定采用隔离容器 + 443 + allowlist 域名 + 测试 CA；测试 CA 只注入测试环境，不改变生产信任根，不关闭 TLS 校验，测试域名固定解析到隔离环境。
32. 历史缓存清理：`registry_cache_epoch` 负责逻辑隔离，Gateway 缓存管理模块负责物理清理（限速、进度、重试、审计）；清理失败不重开旧 epoch，回滚前必须确认旧缓存不可达并完成清理。
33. 私有认证 Host 的公共内容失去共享缓存，本版接受；不做 per-repo 公私识别。
34. Token Key 不增加 `CredDigest`，维持七字段；恢复时强制清空 Token Cache，不依赖进程重启。
35. `PrivateAuthEnabled=true` ⇒ `GatewayAccessTokenRequired=true`；运行时关闭 Access Token 即私有认证 fail closed。
36. `OfficialOnly` 贯穿 Manifest、Blob、重试、hedge、Source 切换和重定向全生命周期，不是一次认证标志。
37. `registry_cache_epoch` 必须持久化；epoch 切换先于对外服务；启动或切换失败不得以旧 epoch 继续私有认证。

### 仍需实测的证据项（不需要再拍板）

1. 6 个双用途 Source 的 GHCR policy 能否生成；不能生成的进入 `policy_gaps`，对应 caps 行 `enabled=0`。
2. GitLab / ECR 的隔离环境产品级 HostProbe、真实调度和 Digest 对照。
3. cgr / asia-east1 的 per-host 与 host-prefix 完整尝试证据。
4. 候选查询性能基线：P50 / P95 / P99、吞吐、错误率、SQL 次数。
5. 14c 反向迁移是否实现；不实现就接受开放流量后的数据丢失窗口。
6. 真实私有 Host（quay.io / gcr.io）的私有 Manifest/Blob、Token 端点 Basic、轮换和错误处理证据；没有账号就记 `BLOCKED_EXTERNAL_SOURCE` / `NOT_EXECUTED`。
7. 受控私有 Basic/Bearer fixture（隔离容器 + 443 + allowlist 域名 + 测试 CA，见 3.5.15-D.1）与 Registry 直接 Basic 的真实请求证据。
8. 历史缓存 epoch 隔离演练：旧 epoch 私有 Manifest/Blob 不可匿名命中，清理后物理消失。

在这些证据项完成前，3.5.14a 不进入实现。
