# 阶段11 P0 修复：竞速预算不足回退错误回退官方源（2026-11-21）

## 问题定性（代码级证据）

用户判断：并发8 下竞速开启后，部分请求连正常顺序回退都无法完成，`errRaceNoBudget`
后回退官方源 401。经代码审查确认根因，并排除两种常见猜测：

- **排除 Lease 泄漏**：`registryrace.go:282-289` 每 attempt 一个 `sync.Once` 收尾，
  Body 关闭后 `doRelease` 归还；拿不到 lease（`registryrace.go:320 lease==nil`）时
  根本没注册 release，不存在占用。
- **排除预算设计/重复消耗**：顺序回退路径 `fetchURL`（`gateway.go:1795`）完全不申请
  Lease，直接 `openUpstream`。设计注释 `registryrace.go:310` 明确"基线 Manifest 回源
  不占 Lease"。所以顺序回退本该能走通。

**真正根因：`tried` 预留状态在竞速放弃时未回滚（状态机 bug）。**

1. `gateway.go:1145`：竞速一进入即无条件 `tried[ia], tried[ib] = true, true`，
   把两个镜像候选标记为已试。
2. `registryrace.go:416`：第一路 `TryAcquireLease` 返回 nil → `errRaceNoBudget`。
3. `gateway.go:1172-1182`（修复前）：回退分支只记 `RegistryRaceFallback()`，
   **未撤销 `tried[ia]/tried[ib]`**。
4. `gateway.go:1206-1208`：顺序循环 `if tried[i] { continue }` 跳过两个镜像源。
5. `gateway.go:804`：`targets` 顺序是 `[官方源, 镜像源1, 镜像源2]`，镜像源全被跳过后
   只剩官方源 → `fetchURL(官方)` → quay.io 401。

因果与实测 counter 吻合：`budget_skipped=22` ≈ `fallback=20` = 20 个官方 401，
`body_failures=0`（非正文校验问题）。

## 修复（commit f22a50a）

`gateway.go` 回退分支：`errRaceNoBudget` 时 `delete(tried, ia)`、`delete(tried, ib)`，
撤销从未拨号候选的预留；只有实际发起过请求并失败的源才保留在 tried 里。
改动 11 行 + 注释。

## 测试（RED → GREEN）

新增 `TestRaceNoBudgetFallsBackToMirrorNotOfficial`（`registry_race_test.go`）：
- MaxPerSource=1，预占两个镜像源的唯一 lease → 竞速第一路必拿不到 → `errRaceNoBudget`；
- 官方源返回明显不同的 body（含 `SHOULD_NOT`）作为哨兵；
- 断言：status=200、body 来自镜像源、officialHits 不增加、命中至少一个镜像源、
  记录 budget_skipped。

**RED（修复前）**：`status=502 body="内容与客户端给的摘要不符：期望 b3f42... 实得 da81..."`
——正是回退官方拿到错误内容。
**GREEN（回滚 tried 后）**：PASS。

这是区分"状态 bug"与"资源/预算问题"的决定性实验：只回滚 tried 就让测试转绿，
证明与 Lease 生命周期、预算额度无关。

新增测试同时填补了覆盖空白：修复前全仓无任何 `errRaceNoBudget` 回退路径测试。

## 回归验证

- `go test ./internal/gateway/ -count=1`：ok（72.8s）
- `go test ./... -count=1`：全 34 包 ok（gateway 77.4s）
- `go vet ./...`：干净
- `go test -race ./internal/gateway/ -count=1`：ok（132.9s）
- `gofmt`：干净

## 真实环境复测（候选镜像 mirror-lens:3.5.18-p0fix，commit f22a50a）

复现 A10 冻结场景：并发8、10 轮×8 digest、冷缓存、两真实 HTTP 源（动态 digest，
非手抄）、race_after_ms=50、star race ON。动态 digest 取自源脚本，杜绝手抄夹具错误。

| 指标 | 修复前（a10full-conc8-race-counterverify） | 修复后（p0fix-real-verify-clean） |
|---|---|---|
| 成功/总样本 | 60/80 | **80/80** |
| 错误率 | 25% | **0%** |
| 官方 401 | 20 | **0** |
| fallback | 20 | 20 |
| budget_skipped | 22 | 20 |
| body_failures | 0 | 0 |
| rank2 胜出 | 6 | 6 |
| 胜者分布 | fast 20/slow 40 | fast 34/slow 46 |

**关键解读**：修复后 `budget_skipped=20`/`fallback=20` 仍存在（并发8超过单源预算是
既定设计），但这些降级请求**全部成功顺序回退到镜像源**，不再回退官方。
所有 80 个成功样本来自 race-fast/race-slow，`official_401_count=0`，
`body 含 official 标记=0`。即：预算不足从"请求失败"降级为"正常顺序回退"。

第一轮复测末尾出现 2 个 429（入口每 IP 300/分限流，环境噪声，非官方回退），
第二轮每轮加 0.8s 间隔后 80/80 全绿，确认 429 与本 bug 无关。

## 结论

- P0（竞速预算不足后顺序回退失败）已修复并以代码级单测 + 真实环境 A/B 复测双重证明。
- 未改 `max_per_source`，未改任何冻结预算/阈值/场景参数。
- 未打 tag、未 push、未构建生产镜像、未碰生产（`mirror-lens:3.5.18-p0fix` 仅为本地候选）。
- 并发8 的 P95 性能是否达标（原 A10 NOT_MET 的另一维度）不在本次 P0 修复范围：
  本次只修复"请求失败/口径不一致"，不改延迟注入与预算配置，性能判定仍按
  `phase11-a10-full-matrix-result.md` 的诚实记录。

## 证据文件

- 修复提交：`f22a50a`（`internal/gateway/gateway.go`、`registry_race_test.go`）
- 复测编排：`p0fix_real_verify.py`
- 复测数据：`p0fix-real-verify.json`（含 2 个 429 噪声）、
  `p0fix-real-verify-clean.json`（干净 80/80）
- 镜像构建日志：`rebuild-p0fix.log`
