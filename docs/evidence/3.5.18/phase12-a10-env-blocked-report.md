# 3.5.18 A10 修复后重测——环境阻碍报告（待用户裁定）

## 结论先行

按执行顺序第6步，用最终镜像 `mirror-lens:3.5.18-final`（含 P0/P1/P2/P3 + 3.5.18，commit e848bd4）重跑 A10 时，
**当前环境无法复现 R1/R2 当时的干净测试条件**：网关启动后 discover 模块自动联网扫描并写入 72 个公网
`discovered` 源，污染 fixture 候选池。结果在阈值边缘剧烈波动，**没有任何一次可作为正式 A10 证据**。
此为非代码回归的外部环境阻碍，须用户裁定后续口径，不自行挑样本、不人为调 harness 求 PASS。

## 实测数据（全部本地真实运行，见同目录 a10final-* / a10ctrl-* 文件）

| 序号 | 镜像 | 环境 | P95 改善 | BASE P95 | RACE P95 | wins_rank2 | 官方401 |
|---|---|---|---|---|---|---|---|
| ① | final | 未清理（discover 污染） | -90.8% | 405ms | 772ms | 2 | 0 |
| ② | p0fix（旧镜像对照） | 未清理（污染） | +21.9% | 503ms | 393ms | 4 | 0 |
| ③ | final | set_star 阶段清理 | +77.0% | 891ms(虚高) | 205ms | 6 | 0 |
| ④ | final | 每轮 clear_state 清理 | +13.2% | 450ms | 391ms | 4 | 0 |

所有运行 `err_rate=0`、`official_401=0`、`body_failures=0`、口径一致 `true`。

## 根因（实测确认）

1. 网关日志：`discover: 新增 72 个候选源`、`runner: tier2 done, 6 checks over 74 sources`。
2. 测试 DB 源分布：`total 74 / fixture 2 / discovered 72`。
3. 网关启动 08:39:02，discover 写入在约 08:39:50（启动后 ~48s），晚于 seed（~10s）与 star（~22s），
   即 discover 在测试环境搭好后才污染，导致 `ineligible_total=80`、竞速源被公网源稀释。
4. 历史 R1/R2（d7bb91a）当时外网不通，discover 拿不到候选，DB 只有 2 个 fixture 源，场景天然纯净——
   这是当前环境与之唯一实质差异。

## 受控对照（证明非 P1/P2/P3 代码回归）

用 P0 修复时的旧镜像 `mirror-lens:3.5.18-p0fix`（不含 P1/P2/P3）在**同一污染环境**跑并发8（序号②）：
P95 改善仅 +21.9%，远低于其历史 R1 的 +52.8%。旧镜像同样退化 ⇒ 退化来自 discover 污染的环境，
不是 P1（tried 回滚）/P2（plan.Source 隔离）/P3（统计字段）引入。这三处均为正确性修复，
不触碰竞速判定、Lease、预算、尾延迟等性能路径。

## 为何不继续自行处理

- harness 每加一层"清理"就离冻结 A10 场景更远，且清理本身（删 DB 源、改 clear_state）可能改变测试语义；
- ③ 的 +77% 源于 BASE P95 虚高（891ms）的尾延迟抖动，④ 的 +13% 又是另一极端——挑任一次当结论都是挑样本；
- 继续调 harness 求 PASS 违反证据纪律。故停止并上报。

## 待用户裁定的三个选项

- **A（推荐）**：以已归档的 R1/R2（d7bb91a，干净环境，52.8%/49.8%，PERFORMANCE_NOT_MET）作为修复后性能证据，
  理由是 P1/P2/P3 不涉及性能路径；本轮重测记为「PERFORMANCE_EXTERNAL_BLOCKED（discover 污染）」，不宣称 PASS。
- **B**：授权在显式关闭 discover 的纯净配置下重跑（需配置变更，须确认不违反冻结场景）；重跑并发 8/3/1 全套。
- **C**：性能终态直接标记 PERFORMANCE_EXTERNAL_BLOCKED，主报告如实说明外部阻碍，放弃本轮重测。

当前性能状态维持 REV-1 枚举 `PERFORMANCE_NOT_MET`（R1/R2 已执行未达标），不因本轮污染数据改变。
