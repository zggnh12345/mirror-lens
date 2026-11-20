# 18c 阶段证据：B 动态排序（W1/F02/F03/G3）

版本 3.5.18 / 阶段 18c / 分支 feat/3.5.18

## 范围与裁定落实

- 公式（冻结，用户选 1 恢复归档 W1/F02/F03/G3）：
  - `Score = 0.6×SuccessRate + 0.4×LatencyScore`
  - `SuccessRate = 成功样本/有效样本总数`
  - `LatencyScore = 1/(1+P50(TTFB)/800ms)`；无可测 TTFB 则为 0
  - 调用频率权重 0%、无时间衰减、并列按固定顺序兜底不随机
  - 公式版本常量 `B-DYNAMIC-SCORE-FORMULA`
- 样本窗口 W1：每映射独立最近 50 有效样本；映射身份/可信关系变化清零；不足 50 用固定顺序。
- 有效样本 F03：真实回源最多每映射一样本；首站命中计入；成功须过 Manifest 身份校验；排除缓存命中/取消/断开/策略拒绝/认证拒绝/本地准入失败/未发出/因他人胜出取消。
- TTFB 口径（本次补充为唯一可测试定义）：起点=发出请求(cli.Do)，终点=收到响应头；排除本地排队/Lease/Body；有响应头的失败样本参与 P50；超时无响应头只进成功率分母、不虚构 TTFB。

## 存储载体（用户裁定方案 1）

- 单一 settings 内部键 `cross_mapping_dynamic_state`，JSON 含：Version、Revision、SnapshotRevision、FormulaVersion、UpdatedAt、Mappings（每映射 50 样本）、Snapshot（排名派生）。
- **不新增表、不改 Schema、不改 CanonicalSchemaHash、不加 db-migrate baseline/迁移版本**（Schema 仍 3.5.17，hash bfc7f2f9…）。
- 不做用户配置项暴露、不进静态配置导出、不进 Canonical 配置 Hash。
- 原子一致：样本写入+窗口裁剪+评分+快照更新在同一状态、同一 Revision；进程内 mutex + SQLite 事务保护完整读改写；快照修订号与样本修订号恒等。
- 启动恢复：Load 校验版本/公式/容量/修订；损坏/未知版本/不一致 → 拒绝动态排名、回退固定顺序、不阻断服务。
- 映射变化：ActiveMappingIdentities 外的旧映射状态在下次写入时清除、不迁移；ResetDynamicStateMapping 显式清理。
- 持久化失败：不影响当前下载请求，不发布未持久化排名，记可诊断日志。

## 四列核对

| 项 | 设计已冻结 | 代码已存在 | 门禁已存在 | 测试已存在 |
|---|---|---|---|---|
| 评分公式/50 门槛/样本口径 | §3.3+归档 | store/dynamicscore.go ScoreOf | 全量+单元 | TestDynamicScoreFormula 等 |
| 样本持久化/原子/并发 | 用户裁定 | ApplyDynamicSample | race | TestDynamicConcurrentUpdate 等 |
| 启动恢复/损坏回退 | 用户裁定 | LoadDynamicState/validate | 单元 | RestartRecovery/CorruptJSON/UnknownVersion |
| 动态排序应用（只重排 M） | §3.3/§7.7 | replica.go replicaTargets | gateway 测试 | TestB18Dynamic* |
| Web 展示 | §7.7 | API dynamic_state + index.html | api 测试 | TestCrossRegistryAPIDynamicState |

## 命令与结果

- gofmt -l . ：空
- go vet ./... ：OK
- go build ./... ：OK
- go test ./... -count=1 ：34 包 ok
- CGO_ENABLED=1 go test -race ./... -count=1 ：34 包 ok，0 DATA RACE
- 完整输出见 18c-gate.log / 18c-race.log。

## 门禁状态

- 全量门禁：PASS
- Race：PASS（0 DATA RACE）
- 生产：WAITING_FOR_USER_DECISION（本授权排除所有生产操作）
- B15 性能外部对照：EXTERNAL_BLOCKED（无真实公共多 Registry 配对环境）
