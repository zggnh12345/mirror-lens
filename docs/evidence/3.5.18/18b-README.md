# 3.5.18 阶段 18b 执行证据

## 结论：PASS（18b 范围内）
- 分支 `feat/3.5.18`；起点 `6770711`；代码基线 `6ca6e76`；18a 提交 `10cfc11`。
- 1.6 权威文档、冻结稿、sha256 清单、Schema：未修改（18b 不新增表、不改 Schema）。
- 生产：未触碰。

## 范围
18b = 候选来源模型（按派发模式分离）+ 资格门禁 + 顺序回退 + A/B 接管边界 + 固定排序接入 + Web 管理 UI + 前后端配置一致性。
动态排序（18c）、嵌套调度（18d）、安全预算（18e）、运行可观测性 API（18f）不在本阶段。

## 冻结语义四列核对（skill：实施自报 ≠ 冻结语义落地）
冻结条款 → 代码/门禁/测试落点：

| 冻结条款 | 设计已冻结 | 代码已存在 | 门禁已存在 | 测试已存在 |
|---|---|---|---|---|
| sequential 候选来自可信映射、不受参选集合限制（§1.3/§1.6、反向测试1/3） | ✓ §1.3/§1.6 | ✓ replica.go `raceEligible` 分支 | ✓ 仅 race 要求星标∩参选 | ✓ TestB18SequentialIgnoresEmptySelection / …SelectionChangesDoNotAlterCandidates |
| race 候选须 Source 级求交（调度选中∩星标∩参选，方案二） | ✓ §1.5/§1.6 | ✓ replica.go `if raceEligible` 门禁 | ✓ 派发前门禁 | ✓ TestReplicaRequiresSelection（race 未勾选不进候选） |
| race + 空参选退化为按可信映射顺序回退（§1.6、反向测试2） | ✓ §2.1/§10 | ✓ crossRegistryManifest `mode` 降级 | ✓ 退化仅改派发形态 | ✓ TestB18RaceEmptySelectionDegradesToSequential / TestReplicaEmptySelectionDegrades |
| 退化仍执行 B.3 安全门禁、不是裸回退（反向测试4） | ✓ §10 | ✓ 退化走同一 fetchReplicaArtifact | ✓ 响应后三方 Digest | ✓ TestB18DegradedStillEnforcesDigestGate |
| 撤星后陈旧勾选不授予竞速资格，但顺序候选仍来自可信映射 | ✓ §1.5 | ✓ EffectiveCrossSelected 读时过滤 | ✓ race 用交集 | ✓ TestCrossSelectedStaleEntryFiltered |
| B 总开关/模式/排序三字段 Web 可编辑，dynamic 只读 | ✓ §7.10 | ✓ index.html + crossregistry API | ✓ SetBEnabled/SetBMode/SetBSortMode 校验 | ✓ TestCrossRegistryAPIBConfigFields |
| 固定排序 Generation/事务审计/并发保护/删除级联 | ✓ §8 18b | ✓ replicas.go/trustedmap.go（3.5.17 已有） | ✓ 同事务 bump + 审计 | ✓ replicas_test.go/trustedmap_test.go（含并发） |
| 未星标 Source 不能成为跨站参选 | ✓ §1.5 | ✓ SetCrossSelectedSources | ✓ 星标校验 | ✓ crossselect/crossconfig 测试 |

## 关键改动
- `internal/gateway/replica.go`：`replicaTargets` 按模式分支候选来源——sequential 来自可信映射（不查参选），race 才要求星标∩参选（方案二）；`crossRegistryManifest` 中 race + 空参选降级为 sequential 分路，两处用同一降级条件，安全门禁不变。
- `internal/gateway/b_candidate_model_test.go`：新增 4 个反向测试（1/2/3/4）。
- `internal/gateway/replica_test.go`：按冻结稿"旧 B20 测试更新预期"要求，更新 `TestReplicaEmptySelectionDegrades`、`TestCrossSelectedStaleEntryFiltered` 断言（不降低覆盖）。
- `internal/api/web/index.html`：B 总开关、sequential/race、fixed/dynamic 控件；dynamic 只读提示（版本变更流程启用）；文案区分"顺序不受参选限制 / 竞速才用参选"。
- `internal/api/crossregistry_test.go`：新增 `TestCrossRegistryAPIBConfigFields` 前后端一致性测试。

## 测试（本次重新运行，见 18b-gate.log / 18b-race.log / 18b-race-rerun.log）
- 定向：4 个反向测试全 PASS；旧 B20 两测试更新后 PASS；API 三字段一致性 PASS。
- 全量门禁：gofmt 空、vet/build OK、`go test ./... -count=1` 34 包 ok。
- race：首次全量在 `TestPrivateAndPublicConcurrentDoNotShareCache`（privateauth_test.go:93，与本阶段改动无关的既有并发测试）观察到 1 次 DATA RACE；该测试单跑 3 次不复现，全量 race 原样重跑 1 次零 DATA RACE、34 包 ok。按 skill 口径记为"观察到一次、未稳定复现"，不判为确定缺陷，也不宣称首次全绿。重跑结果为 18b-race-rerun.log。

## 未覆盖 / 边界
- 真实公共 Registry 端到端、生产配置：无授权，未实测。
- 动态排序算法（18c）、嵌套调度（18d）、Authority/Digest/R-P-G 预算（18e）、运行可观测性 API（18f）未在本阶段实现，不得据此推断。
- B.3 响应后三方 Digest 校验本身在 3.5.17 已实现（fetchReplicaArtifact）；本阶段只保证退化路径仍走该门禁，未重做校验逻辑。
