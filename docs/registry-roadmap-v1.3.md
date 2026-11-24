# Mirror Lens Registry 后续版本路线图 v1.3

**状态：** 规划草案，待冻结  
**基准日期：** 2026-11-15  
**合并来源：**
- `docs/3.5.4-convergence-roadmap.md`（旧 A–D 顺序与架构债）
- `mirror-lens-registry-roadmap-v1.2.1.md`（最新 Phase 0–4 规划）
- `docs/3.5.10-source-realm-design`（Source 自有 Realm Draft）
- 一源多 Host 通用问题（Source-Beta K8S 只是首个实测样本）

> 本文只做版本安排与验收边界，不代表下列版本已经排期或实现。
> 版本号是规划占位，不承诺发布日期；每个版本必须单独验收，未通过不得进入下一版本。

## 1. 先修正版本号冲突

`3.5.10` 已经被“多能力标签修复”占用并发布：

- `v3.5.10`
- 修复 HUB / GHCR / RAW 多标签
- 已部署生产

原计划放在 `3.5.10` 的 **Source 自有 Realm 与认证隔离**，顺延为：

**`3.5.11`**

后续版本以此为准，避免同一个版本号同时表示 UI 修复和 Registry 认证能力。

## 2. 已完成与当前基线

| 版本 | 内容 | 状态 |
|---|---|---|
| 3.5.4 | 隐式 Registry 身份收敛、Canonical 前置 | 已发布 |
| 3.5.5 | Canonical Schema、离线升级 | 已发布 |
| 3.5.8 | 第一批公开 Registry 多源调度 | 已发布 |
| 3.5.9 | 多源开关、只读标签、鉴权回退修正 | 已发布 |
| 3.5.10 | 多能力标签修复（HUB/GHCR/RAW） | 已发布并部署 |
| 3.5.11 | Source 自有 Realm 与认证隔离 | 设计 Draft，未实现 |

当前模型的不变量仍然是：

- 一个 Source 只有一个主 `repo_host`
- 一个 Host 可以有多条 Source
- 一条 Source 不能同时服务多个 Host
- `repo_host` 是 Registry 归属的唯一持久化事实
- UI 标签只能来自持久化能力与探测结果，不能从 URL 猜测

## 3. 本次新增问题：一源多 Host（Source-Beta只是样本）

### 3.1 问题定义

当前模型是：

- 一个 Source 只有一个主 `repo_host`
- 一个 Host 可以有多条 Source
- 一条 Source 不能同时服务多个 Host

但公共镜像站经常用**同一个 base URL 同时代理多个 Registry Host**。Source-Beta `https://registry-upstream-b.example.net` 就是一个已实测样本：

- Docker Hub
- GHCR
- GCR
- `registry.k8s.io`

实测：

```text
GET https://registry-upstream-b.example.net/v2/registry.k8s.io/pause/manifests/3.9
→ 200
→ sha256:7031c1b283388d2c2e09b57badb803c05ebed362dc88d84b480cc47f72a21097
```

**这不是 K8S 专属问题。** 只要一条 Source 同时支持多个 Host，当前模型都会只保留一个主 Host，其他能力无法显示、无法调度、无法独立探测。Source-Beta只是目前最先被实测确认的样本。

### 3.2 为什么首页没有 K8S

生产库中的Source-Beta Source 当前是：

```text
repo_host             = docker.io
registry_cap          = -1
registry_path_mode    = ''
registry_probe_at     = 0
```

`registry_host_recovery` 和 `source_health_scope` 中也没有Source-Beta的 K8S 记录。

因此这不是 UI 漏读，而是：

- Source-Beta只被登记为 Docker 主源
- 没有 K8S 的 Host 归属
- 没有 K8S 的探测结果
- 当前 Schema 不支持一条 Source 多 Host

**不能只补一个 K8S 标签。** 那会显示“支持 K8S”，但调度器不会把 K8S 请求分给它，属于假标签。

### 3.3 影响范围

这个问题不只影响Source-Beta的 K8S，也影响任何“一源多 Host”的情况：

- 一个公共镜像站同时代理 Docker Hub / GHCR / GCR / K8S / Quay 等
- 一个 Source 对不同 Host 使用不同 V2 前缀或路径模式
- 一个 Source 对不同 Host 使用不同 Realm / Token 端点
- 一个 Source 对不同 Host 有不同健康状态、冷却和失败原因

因此 3.5.14 必须是**通用能力模型**，不能为Source-Beta或 K8S 写特判。

注意区分两个概念：

- **同一 Host 多 Source**：一个 Host 配多个镜像源，3.5.13 解决
- **一源多 Host**：一个镜像源同时服务多个 Host，3.5.14 解决

本文说的“其他多源也一样”指后者：任何一条 Source 只要实际支持多个 Host，都必须走同一套 `source_host_caps` 模型，不能按 Host 或按站点单独开分支。

### 3.4 解决版本

这个问题进入 **3.5.14：一源多 Host 通用能力模型**。

3.5.12 只负责把 `registry.k8s.io` 作为独立 Host 验证通过；它不会让Source-Beta同时显示 K8S，也不会解决其他多 Host Source。


## 4. 后续版本总表

| 版本 | 主题 | 依赖 | 默认行为 |
|---|---|---|---|
| 3.5.11 | Source 自有 Realm 与认证隔离 | 3.5.10 | 旧 Source 行为不变 |
| 3.5.12 | 第一批验证集：GCR / Artifact Registry / K8S | 3.5.11 | 新 Host 默认关闭 |
| 3.5.13 | 第一批完整覆盖 + 同 Host 多 Source | 3.5.12 | 顺序回源 |
| 3.5.14 | 一源多 Host 通用能力模型（Source-Beta等） | 3.5.13 | 旧单 Host Source 不变 |
| 3.5.15 | Gateway-CredStore 私有凭据闭环 | 3.5.14 | 匿名优先 |
| 3.5.16 | Manifest 星标竞速 | 3.5.15 | 默认关闭 |
| 3.5.17 | Blob 竞速评估 | 3.5.16 | 仅评估，不承诺实现 |

## 5. 各版本具体安排

### 3.5.11 Source 自有 Realm 与认证隔离

**目标：** 把 Realm 判定从“Host 全局白名单”改为“按 Source 计算的策略”，解决公共镜像站使用自己认证端点时被误杀的问题。

范围：

- Source 可以显式声明自己的 Realm 端点
- 探活与 Gateway 使用同一套 Realm 策略
- Token 缓存按 Source 和凭据代次隔离
- 第三方 Source 不自动继承官方 Host 的 Realm
- 失败关闭，不因策略解析失败而放行
- 不实现 OAuth、OIDC、云厂商 IAM
- 不实现私有仓库 CredStore

验收：

- `m.daocloud.io` 作为 `public.ecr.aws` 的 Source 时可通过探活
- 旧 Source 空配置行为与 3.5.9 一致
- 探活通过和 Gateway 实际回源不出现两套 Realm 语义
- Source 禁用、删除、轮换凭据后旧 Token 不能复用
- Realm 请求继续经过 SSRF / DNS / IP Guard

### 3.5.12 第一批验证集：GCR / Artifact Registry / K8S

**目标：** 用三个代表性 Host 打通 Host 准入、真实候选回源、匿名 Token、单源顺序回源和缓存语义。

Host：

- `gcr.io`
- `<location>-docker.pkg.dev`
- `registry.k8s.io`

范围：

- Host 独立 enable / disable，准入早于候选查询
- 修复 foreign Registry 的 `officialOnly=true` 路径，让配置了 Source 的 Host 真正进入候选调度
- 每条 Source 显式设置正确的 `repo_host`
- 接通 `native` / `host_prefix`
- 三个 Host 的匿名 Bearer Challenge / Realm / Token 流程
- 每个 Host 配置代表性 Probe 仓库
- 保持 Manifest Host 隔离、Blob 按 Digest 共享

不做：

- 不让一条 Source 服务多个 Host
- 不解决任何“一源多 Host”标签；Source-Beta K8S 和其他同类 Source 都等 3.5.14
- 不启用第一批之外的 Host

验收：

- 三个 Host 可独立启停，关闭后不能查询候选或借 fallback 绕过
- 配置 Source 后请求实际命中候选，而不是只检查配置存在
- 三个 Host 都能完成真实公开仓库拉取
- Host A 的故障、探测和冷却不污染 Host B
- Docker Hub / GHCR 无回归

### 3.5.13 第一批完整覆盖 + 同 Host 多 Source

**目标：** 扩展到第一批剩余 Host，并完成同一 Host 多条 Source 的顺序调度。

新增 Host：

- `quay.io`
- `mcr.microsoft.com`
- `registry.gitlab.com`
- `lscr.io`
- `public.ecr.aws`
- `cgr.dev`

范围：

- 每个 Host 的匿名公开仓库 Realm / Token 流程
- 同一 `repo_host` 允许多条 Source
- 每条 Source 独立探测、健康、冷却、退避和并发限制
- 候选按序尝试，错误分类保留 401/403、404/410、429/503、网络错误、内容校验失败
- 扩展 `/metrics` 与日志维度
- 保持官方回退，不做跨 Host fallback

验收：

- 第一批全部 Host 的公开代表性仓库可实际拉取
- 一个 Host 配置两条以上 Source 时按顺序尝试并正确回退
- 一个 Source 故障不导致同 Host 其他 Source 被错误熔断
- 不同 Host 的 Source、健康、Manifest 缓存和认证状态不串用
- 只完成 Phase 1 时只能称“验证集通过”，不能称“第一批全部支持”

### 3.5.14 一源多 Host 通用能力模型

**目标：** 让任意一条 Source 可以声明并服务多个 Registry Host；Source-Beta只是首个验收样本，不是特例。

这是旧路线图中的显式架构债，也是“Source-Beta K8S 不显示”以及其他同类多 Host Source 问题的正式归属版本。

#### 3.5.14-A 能力盘点

实现前先做一次全量审计，不预设只有Source-Beta：

- 枚举所有 enabled Source
- 对每个 Source 按 `host_prefix` 和 `native` 尝试候选 Host
- 候选 Host 至少覆盖：`docker.io`、`ghcr.io`、`gcr.io`、`registry.k8s.io`、`quay.io`、`mcr.microsoft.com`、`registry.gitlab.com`、`lscr.io`、`public.ecr.aws`、`cgr.dev`
- 只有实际 Manifest 可读且 Digest 可验证，才记录为 `cap=1`
- 记录 `source_id`、`host`、`registry_cap`、`registry_path_mode`、`probe_at`、`probe_error`
- 不从 `base_url` 推断，不从站点名称推断，不手工补标签
- 产出“多 Host Source 清单”，作为 3.5.14 的输入

Source-Beta是清单中的第一个已确认样本；其他公共镜像站是否支持多 Host，以审计结果为准。

审计输出至少包含：

| source_id | base_url | host | cap | path_mode | probe_at | probe_error |
|---|---|---|---|---|---|---|
| 638 | `https://registry-upstream-b.example.net` | `docker.io` | 1 | `native` | 已有 | — |
| 638 | `https://registry-upstream-b.example.net` | `registry.k8s.io` | 待探测 | `host_prefix` | 待探测 | — |
| … | … | … | … | … | … | … |

表格只是格式示例，实际值必须由探测写入，不能手工填成能力事实。

#### 3.5.14-B 数据模型

```text
source_host_caps
  source_id
  host
  registry_cap
  registry_path_mode
  registry_probe_at
  registry_probe_error
  enabled
  PRIMARY KEY(source_id, host)
```

兼容策略：

- `sources.repo_host` 保留为主 Host，保证旧查询和旧数据不变
- 迁移时为每个现有 Source 回填一条 `source_host_caps` 记录
- 旧单 Host Source 行为完全不变
- 新 Source 可以一次配置多个 Host
- 每个 Host 独立探测、独立健康、独立冷却、独立路径模式
- Source 的 Realm / 凭据策略必须按 `(source, host)` 计算，不能只按 Source
- UI 标签只显示已探测通过的 Host，不根据 URL 猜测
- `SourceCandidates("registry:<host>")` 按 Host 读取能力记录
- 不改变 Manifest 缓存身份和 Blob 内容寻址语义
- 代码中不得出现 `gp.suwa`、`Source-Beta` 或 `K8S` 的 Source 特判

#### 3.5.14-C 通用验收

- 至少两条多 Host Source 通过验收：一条真实来源（Source-Beta）+ 一条独立 fixture 或第二条真实来源
- 每条 Source 的多个 Host 可独立启停、独立探测、独立冷却
- 每个 Host 的标签只在对应探测通过后显示
- 每个 Host 的请求实际命中该 Source，而不是只显示标签
- 关闭其中一个 Host 不影响同一 Source 的其他 Host
- 不同 Host 的健康、失败、冷却、Manifest 缓存互不污染
- 未探测或探测失败的 Host 不显示标签
- 旧单 Host Source 的候选集、缓存和健康状态无回归
- 迁移前后旧 Source 的候选集 diff 为空

#### 3.5.14-D Source-Beta样本验收

- Source-Beta可保存 `docker.io`、`ghcr.io`、`registry.k8s.io`、`gcr.io`
- 首页只显示实际探测通过的标签
- K8S 请求实际命中Source-Beta，而不是只显示标签
- 关闭Source-Beta K8S 能力不影响 Docker/GHCR
- Docker/GHCR/K8S/GCR 的健康、冷却、失败互不污染

明确不做：

- 不把不同 Host 的缓存合并
- 不做跨 Host fallback 或竞速
- 不用手工标签替代真实探测
- 不自动猜测镜像属于哪个 Host
- 不为单一 Source 写代码特判


### 3.5.15 Gateway-CredStore 私有凭据闭环

**目标：** 让需要用户凭据的业务回源使用统一 CredStore，补齐第一批 Host 的私有仓库和受限仓库能力。

范围：

- Gateway 实际请求使用 CredStore，而不是只在 Host Probe 阶段使用
- 每次请求使用一致的凭据 Snapshot，generation 贯穿写回
- 凭据轮换后旧 generation 不能覆盖新状态
- Realm 按 Host 独立白名单
- 匿名优先，匿名不足时使用用户凭据
- 日志脱敏，不记录密码、Token、Authorization

验收：

- 匿名公开仓库继续成功
- 需要认证的私有仓库可按凭据访问
- 业务回源真正使用 CredStore
- Probe 成功不能代替业务回源验收
- 跨 Host Realm、Token 和凭据泄漏测试通过

### 3.5.16 Manifest 星标竞速

**目标：** 同一 Host 已有多个稳定候选后，提供显式、可控的 Manifest 首包加速策略。

范围：

- `star_race` 是 Host 级查询策略，默认关闭
- 只允许同一 Host 下显式 `registry_race=true` 且健康的 Source
- 最多两个候选参与
- 其他候选仍按顺序回退
- winner 确认后取消并清理 loser
- 只作用于 Manifest，不作用于 Blob
- 不跨 Host 竞速

验收：

- 默认关闭时严格 sequential
- 开关只影响指定 Host
- 只有显式合格候选参与，最多两个
- winner 响应完整可读，loser body / 连接 / goroutine 正确清理
- 有固定环境、基线、重复次数和 P50/P95 阈值的对照测速

### 3.5.17 Blob 竞速评估

**目标：** 只做评估，不作为版本承诺。

- Blob 仍使用标准代理与内容寻址缓存
- 不并行拼接多个上游 Blob
- 只有在 Body 所有权、取消边界、首字节后接管、Range/断点、`.part` 生命周期都有可验证方案后，才单独立项
- 未通过评估前不进入实现

## 6. 新旧路线映射

| 旧路线 | 新版本 | 说明 |
|---|---|---|
| 3.5.4 A：Host 凭据、Realm、Gateway 凭据、独立开关 | 3.5.11 + 3.5.15 | 公开 Realm 先做，私有 CredStore 后做 |
| 3.5.4 B：多 Registry 正式启用 | 3.5.12 + 3.5.13 | 先验证集，再第一批完整覆盖 |
| 3.5.4 C：Manifest star_race | 3.5.16 | 默认关闭，独立开关 |
| 3.5.4 D：Blob 竞速 | 3.5.17 | 只评估，单独立项 |
| 3.5.4 架构债：一源多 Host | 3.5.14 | 通用能力模型，Source-Beta K8S 与其他多 Host Source 一并解决 |
| 3.5.10 Source Realm Draft | 3.5.11 | 3.5.10 已被标签修复占用 |

## 7. 版本门禁

1. 3.5.11 未通过前，不进入 3.5.12 的 Host 实现。
2. 3.5.12 只证明三个 Host 可用，不宣称第一批完整支持。
3. 3.5.13 完成前，不宣称第一批 Registry 全部支持。
4. 3.5.14 完成前，不为任何 Source 手工添加多 Host 标签。
5. 3.5.15 完成前，不宣称支持私有 GitLab / NVCR / 私有 ECR。
6. 3.5.16 默认关闭，不能用全局 Hedge 隐式开启。
7. 3.5.17 不是交付承诺，未评估通过不排期。

## 8. 仍然冻结的非目标

- 一条 Source 同时服务多个 Host：3.5.14 之前不做
- 无前缀跨 Registry 自动搜索或猜测仓库归属
- 自动发现第三方镜像站
- GCR legacy 区域子域的自动支持或自动映射
- 不同 Registry Host 之间互相 fallback 或竞速
- Blob 竞速
- 为 Registry 功能重写通用代理协议、Scheduler 主架构或已有缓存系统
- 在当前一源一 Host 模型下提前建设通用站点映射表
- 在本轮新增 Azure ACR、NVIDIA NGC、AWS Private ECR 或其他 Host

## 9. 待冻结问题

1. 3.5.14 的表名用 `source_host_caps` 还是 `source_registry_hosts`
2. 多 Host Source 的 Realm 策略是否完全按 `(source, host)` 独立
3. 多 Host Source 的 UI 是显示多个标签，还是显示“主 Host + 其他能力”
4. 3.5.14 是否需要同时迁移现有 Source 的 `repo_host` 语义
5. 3.5.12 的 K8S 验证是否使用独立 Source，还是等待 3.5.14
6. 3.5.13 与 3.5.14 是否合并为一个版本，取决于多 Host 改造对调度器的影响面

在上述问题冻结前，3.5.14 不进入实现。
