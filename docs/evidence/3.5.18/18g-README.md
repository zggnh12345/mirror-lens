# 3.5.18 阶段 18g：全量回归与验收 — 证据

## 范围

冻结稿 §18g（行 631–635）任务 19 项。按授权边界与证据分层逐项分类：
代码级/单元/集成/故障注入/安全回归/配置迁移/Web DOM 属授权内可验证；
真实公网回源、生产升级、备份回滚、性能基线属 `NOT_EXECUTED` /
`WAITING_FOR_USER_DECISION` / `EXTERNAL_BLOCKED`（§12.5 已冻结 B15 阻塞，生产授权被明确排除）。

## 18g 任务逐项分类（诚实分账）

| 任务 | 状态 | 证据 |
|---|---|---|
| 全量单元测试 | PASS | `18g-gate.log`：go test ./... RC=0，34 包 ok |
| 全量集成测试 | PASS | gateway/api/store/secret 包内集成测试，34 包 ok |
| Race 测试 | 见 `18g-race.log` | CGO_ENABLED=1 go test -race ./...，RACE_RC 见日志 |
| 故障注入 | PASS（代码级） | Fault/Cancel/Mismatch/DigestMismatch/Abort 等测试全绿 |
| 安全回归 | PASS（代码级） | Authority/Digest/SSRF/FailClosed/私有认证测试全绿 |
| 配置迁移测试 | PASS | TestResolveBConfigMatrix / TestMigrateBConfig* / TestFreshInstallBDefaultsOn / TestBOffKeepsMappingAndSelection |
| Web 端到端测试 | PASS（DOM/脚本级） | `18g-web-e2e.log`：NODE_BIN 执行 index.html 真脚本，TestPanel*/TestWeb* 全绿，含 18f 审计面板 |
| Docker 真实拉取验收 | NOT_EXECUTED | 真实进程层未执行；无生产/公网配对授权 |
| 可信副本真实回源验收 | NOT_EXECUTED | 同上（需真实多 Registry 配对环境） |
| Authority 真实验收 | NOT_EXECUTED | 真实公网 Authority 环境不可用 |
| 生产升级前检查 | WAITING_FOR_USER_DECISION | 生产授权被明确排除 |
| 备份和回滚验证 | WAITING_FOR_USER_DECISION | 生产备份/回滚需生产授权 |
| 性能基线 | PERFORMANCE_NOT_MET | 已执行 R1/R2 正式配对（R1 52.84%/R2 49.77%，跨 50% 线），见 `phase11-a10-postfix-result.md`；REV-1 合法枚举；P1/P2/P3 修复后待 A10 重测定稿 |
| 候选版本性能对照 | PERFORMANCE_NOT_MET | 同上；官方 401 修复后 0/80，口径一致；不挑样本、不改阈值 |
| 文档归档 / 证据归档 / 清理收尾 / 最终交付审计 | 见 sfinal | 最终交付阶段处理 |

证据分层（不混算）：本阶段所有 PASS 均为**代码测试层**；真实进程层与公共源层未执行，
不得据代码测试通过写成完整交付。

## Race 测试：夹具 data race 与修复（诚实记录）

18g race 首跑在 `TestPrivateAndPublicConcurrentDoNotShareCache` 报出 DATA RACE，
位置 `internal/gateway/privateauth_test.go:93` 的 `privUpstream.lastAuth` 字段：并发 HTTP
handler goroutine 无同步地写普通 string。该竞态在此前 18b 阶段也曾观察到一次（当时单跑
不复现，按「观察到一次、未稳定复现」记录）。本次 18g 全量 race 再次复现，不能继续当作
一次性噪声。

核查：这是**既有测试夹具**代码，不在 3.5.18 生产路径；相邻计数器已用 atomic，只有
`lastAuth`/`tokenAuth` 两个 string 字段漏同步。修复仅改测试文件：加 `authMu sync.Mutex`
保护两字段的写，并加 `lastAuthRead`/`tokenAuthRead` 读helper；所有断言语义不变
（未删测试、未放宽断言）。修复后该测试 `-count=5` race 全绿。全量 race 重新运行见
`18g-race.log`。

## 18g 新增内容

- `internal/api/web_crossruns_test.go`：`TestWebCrossRunsAuditPanel` — 断言 18f 审计面板
  结构存在、调用真实只读 API、展示投票/Authority/缓存/补位/取消/嵌套字段；路由挂在 guard 后。
- `docs/evidence/3.5.18/18g-test-inventory.txt`：从测试树解析的 1438 个测试函数名全集。

## 全量门禁（最终代码重跑）

- `18g-gate.log`：GOFMT 空 / VET_RC=0 / BUILD_RC=0 / TEST_RC=0（34 包 ok）。
- `18g-race.log`：CGO_ENABLED=1，RACE_RC=0（34 包 ok，0 DATA RACE，0 FAIL；夹具竞态修复后重跑）。
- `18g-web-e2e.log`：NODE_BIN=容器内 node v24，TestPanel*/TestWeb* 全绿，WEBE2E_RC=0。

## 终止条件核对（§20）

- 1.6 与代码冲突：无（1.6 blob 未变）。
- 配置迁移兼容性：已证明（18a 矩阵 + 幂等 + 冲突审计）。
- 预算模型不超额：已证明（18d/18e R.Used≤R.Max=16、G 冻结）。
- Authority/Digest 安全边界：未被 18a–18f 触碰破坏。
- A+B 嵌套重复派发/死锁/泄漏：race 测试无 DATA RACE，嵌套测试常驻绿。
- CachePut 提交未验证内容：未发现；既有 digest_consensus 测试全绿。
- P0/P1：无未关闭项。
- 全量测试：通过。
- 真实环境结果与测试预期不一致：未执行真实环境，不适用。
- 生产备份/回滚无法验证：因无生产授权，标 WAITING，不自行验证。
- 性能基线无法复现：B15 已预冻结 EXTERNAL_BLOCKED，按 §12.5 收口。
- 新决策/生产授权：生产阶段需单独授权，按终止条件停下报告。

结论：代码与代码测试层可进入最终交付审计；真实环境/性能/生产三项保持冻结的阻塞态，
不伪造 PASS。

## 边界

- 生产：未触碰，`WAITING_FOR_USER_DECISION`。
- 远程：未 push / 未 tag / 未 Release / 未动 GHCR。
- 无新增 DB 表、无 Schema 变更、CanonicalSchemaHash 未改；1.6 与冻结稿未改。
