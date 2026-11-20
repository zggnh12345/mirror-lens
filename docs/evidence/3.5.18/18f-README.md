# 3.5.18 阶段 18f：运行可观测性与 Web 审计 — 证据

## 范围与口径

冻结稿 §18f（行 623–629）目标：把运行时状态完整暴露给 API 和 Web。
任务 15 项：逐候选状态 / 投票状态 / Authority 状态 / 失败补位 / 胜者 / 取消 / 缓存提交
状态 API；请求级审计关联；Generation 关联；Web 运行状态页；Web 审计查询；指标补齐；
指标基数与敏感信息检查；日志脱敏；历史状态过期与清理策略。

验收 10 条：每次跨站调度可关联逻辑请求；候选派发与结果准确；投票状态可解释；
Authority 可解释；补位记录完整；胜者唯一；缓存提交可追溯；指标和 API 一致；
日志无认证凭据泄漏；不因高基数标签导致指标失控。

## 落地结构

- `internal/gateway/crossrun.go`：`CrossRunState`（低基数判定字段）+ 固定容量 128 的
  环形缓冲 `crossRunRing`；`recordCrossRun` 统一写入环形缓冲并同步 `metrics.CrossRun`；
  `sanitizeHint` 去查询串 + 截长（≤120）防止凭据/高基数进入关联键。
- `internal/gateway/gateway.go`：`Gateway.crossRuns` 字段，`New()` 初始化容量 128。
- `internal/gateway/replica.go`：`crossRegistryManifest` 用 defer 统一记录所有返回路径，
  覆盖 sequential / race 两条路；记录 mode（在 race 空参选退化为 sequential 之后赋值，
  保证与实际生效模式一致）、候选数、嵌套 Source 数、投票结论
  （`out.State.String()`）、Authority、补位（`Truncated`，诚实标注为近似）、取消数、
  胜者 Registry/SourceID、Digest、缓存提交结果（committed/rejected/skipped）、
  Outcome（ok/fallback/fail）、Generation。
- `internal/metrics/metrics.go`：`cross_run_total{mode,outcome,vote}` 计数器，三个标签
  均折叠为封闭集合；不携带 URL/源 ID/凭据。`mode|outcome|vote` 由同一份状态写入，
  保证指标与 API 一致。
- `internal/api/api.go` + `crossregistry.go`：`GET /api/cross-runs[?limit=]`（只读、需
  登录、limit 收敛到容量上限 128）。
- `internal/api/web/index.html`：跨站副本区新增「运行审计（最近跨站调度，只读）」
  折叠面板，按 `toggle` 惰性加载；展示胜者/投票/候选/权威/缓存/补位/取消/嵌套。

## 历史状态过期与清理策略（诚实说明）

运行状态是**进程内内存环形缓冲**，不做持久化：容量固定 128，新记录覆盖最旧记录，
进程重启即清空。这是有界留存策略——内存和指标基数恒定，不会随时间/请求数增长。
它不满足「跨重启的长期审计历史」，但 3.5.17 已有的 settings 审计表负责**配置变更**
的持久化审计，二者职责不同；本阶段未新增数据库表、未改 Schema（符合冻结边界）。
若后续需要跨重启的运行审计历史，属独立决策，需单独裁定与版本规划。

## 四列核对（设计已冻结 / 代码已存在 / 门禁已存在 / 测试已存在）

| 项 | 设计已冻结 | 代码已存在 | 门禁已存在 | 测试已存在 |
|---|---|---|---|---|
| 候选派发与结果 | §18f | replica.go rec.Candidates/Sources | 有 | crossrun_test（结构设计）+ 既有 replica 测试 |
| 投票状态可解释 | §18f | rec.VoteState=out.State.String() | 有 | 既有 crossrace/vote 测试 + 指标折叠测试 |
| Authority 可解释 | §18f | rec.Authority / fallback 置 true | 有 | 既有 crossrace 测试 |
| 失败补位记录 | §18f | rec.Backfill=out.Truncated（近似，已标注） | 有 | 既有 crossrace 测试 |
| 胜者唯一 | §18f | rec.Registry/WinnerID/Digest 单一赋值 | 有 | 既有 race 测试 |
| 取消状态 | §18f | rec.Canceled | 有 | 嵌套 phase1 既有测试 |
| 缓存提交结果 | §18f | rec.CachePut committed/rejected/skipped | 有 | 既有 commitReplica 测试 |
| 请求级审计关联 | §18f | rec.RequestHint（脱敏）+ Seq | 有 | TestCrossRunSanitizeHint |
| Generation 关联 | §18f | rec.Generation=gen | 有 | 既有配置代次测试 |
| 指标补齐 | §18f | cross_run_total | 有 | TestCrossRunClosedLabels |
| 指标基数与敏感检查 | §18f | 封闭标签折叠 | 有 | TestCrossRunClosedLabels |
| 日志脱敏 | §18f | sanitizeHint + 不记录 URL/凭据 | 有 | TestCrossRunSanitizeHint |
| 过期与清理 | §18f | 环形覆盖 + 进程内 | 有 | TestCrossRunRingBoundedNewestFirst |
| Web 运行/审计页 | §18f | 运行审计折叠面板 | 有 | API TestCrossRunsAPI |

说明：`rec.Backfill` 用 `out.Truncated`（被 M 截断的候选数）作为失败补位的近似计数，
因为 `RunCrossRace` 未单独暴露「补位成功次数」。按证据纪律不虚构精确值，README 如实标注
为近似；若要精确补位计数，需改 unified 层结果结构，属超出 §18f 观测范围的改动，未做。

## 全量门禁（最终代码重跑）

见 `18f-gate.log`：GOFMT 空 / VET_RC=0 / BUILD_RC=0 / TEST_RC=0（34 包 ok）。
见 `18f-race.log`：RACE_RC=0（CGO_ENABLED=1，34 包 ok，0 DATA RACE，0 FAIL）。

## 边界

- 生产：未触碰，阶段状态 `WAITING_FOR_USER_DECISION`。
- 远程：未 push / 未 tag / 未 Release / 未动 GHCR。
- 无新增数据库表、无 Schema 变更、CanonicalSchemaHash 未改。
- 未改 1.6 权威文档；冻结稿与 sha256 清单未改。
