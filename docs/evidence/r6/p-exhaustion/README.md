# R6 · P 耗尽证据归档

本目录是第六轮（核心运行时证据）中 P 预算耗尽项的**验收证据**，全部由本轮
重新执行的真实测试生成，没有从旧报告复制结果。

## 测试环境

- 仓库：`zggnh12345/mirror-lens`，分支 `feat/3.5.17-unified-framework`
- 包：`./internal/unified`（测试源码 `p_exhaust_integration_test.go`）
- 上游：`httptest.NewServer` 本地真实 HTTP 服务，每个请求返回带真实
  `Content-Length` 与 Body 的响应
- 预算：`PTransport` 包装真实 `http.Transport`，真实 `RoundTrip`
- 注入：`ScopeConfig{PMax: 2}`（仅测试构造路径）；生产路径恒为 8192

## 覆盖的测试

| 测试 | 验证点 |
| --- | --- |
| `TestPExhaustRealRoundTrip` | 主链与 Hedge 两路真实派发在途，第三次被 P 拒绝；Scope 取消、Body 全关、无新派发、无结果账本写入 |
| `TestPExhaustNoCachePutAfter` | 耗尽后不再产生任何结果/缓存提交 |
| `TestMidfillRangeSoleUseP` | Preflight 与 Range 共用同一 Scope 预算；耗尽后 Preflight/Range/trySwitch 均被拒 |
| `TestReservePCommitP` | 提交前取消释放预留；已提交不返还；并发不超额；重复结束只释放一次 |
| `TestProductionPMaxFrozen` | 生产 PMax 恒为 8192，注入额度只存在于测试构造 |

## 文件

- `commands.txt`：实际执行命令、时间、Go 版本
- `p-exhaustion.log`：`-v` 原始完整输出与退出码
- `result.json`：从上述日志解析出的结构化摘要（非手工填写）
- `production-budget.txt`：生产 8192 的代码位置与测试依据
- `README.md`：本说明

## 本轮实测结果（摘自 result.json）

- Hedge 场景：`pmax_injected=2`、`dispatched=2`、`rejected=1`、
  `bodies_created=2`、`bodies_closed=2`、`on_exhaust_calls=1`、
  `ledger_rows=0`
- Midfill 场景：`preflight_runs=1`、`range_work_used=4`、
  `range_work_max=1024`、`segments=1`、`committed=2`、
  `reserve_after_exhaust_rejected=2`
- Reserve/Commit：`committed=1`、`outstanding=1`、
  并发 `committed=3`、`rejected=5`（不超额）
- 生产预算：`frozen=8192`、`scope_max=8192`

## 口径说明

- `claims_dispatched=0`、`chains=0`：本包测试直接驱动 `PTransport`，不建
  Chain；Claim/Chain 生命周期由 gateway 集成测试覆盖，不能据此推断为缺陷。
- `bodies_closed=0`（Midfill）：两个 Body 在测试内主动关闭，耗尽时无在途
  Body，因此 Scope 无需再关。
- 本包不接 Cache；"未提交缓存"由 `ledger_rows=0` 与无 CachePut 通道断言，
  不是通过计数真实 CachePut 得出。

## 复现

```
cd <repo>
go test ./internal/unified \
  -run 'TestPExhaustRealRoundTrip|TestPExhaustNoCachePutAfter|TestMidfillRangeSoleUseP|TestReservePCommitP|TestProductionPMaxFrozen' \
  -count=1 -v
```
