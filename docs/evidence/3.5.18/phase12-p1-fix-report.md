# 3.5.18 P1 修复报告：首路真实失败 + 次路预算不足时 tried 未回滚

## 缺陷（f22a50a 的残留路径）

f22a50a 只覆盖「首路即无预算 → errRaceNoBudget → 回滚 tried」。
未覆盖：首路真实拨号失败（如 500）→ 快速失败补发次路 → 次路 Lease 被占满
（TryAcquireLease 返回 nil，`budgetOut=true; continue`）→ raceManifest 返回
首路真实错误（非 errRaceNoBudget）→ fetchSeq 的 `errors.Is(rerr, errRaceNoBudget)`
为假 → tried 不回滚 → 顺序回退跳过两个合格镜像源 → 错误回退官方源（401/摘要不符）。

## 关键陷阱：started[] 不能代表实际拨号

`launch()` 一进入就 `started[slot]=true`，发生在申请 Lease 之前。
预算跳过时 `started=true` 但从未调用 openUpstream。若复用 started[] 判断，
次路会被当成"已拨号"而不回滚 tried，P1 表面修复、实际残留。

## 修复

- 严格区分两个状态：
  - `started[slot]`：goroutine 已启动（保持原语义）。
  - `dialed[slot]`：真正进入 openUpstream 调用边界（goroutine 内调用前置位）。
- `raceResult` 新增 `dialed bool`，随 channel 结果传递；主 goroutine 仅在
  收到该 slot 的 raceResult 后回填 `dialedOut[slot]`（channel 建立 happens-before，
  不读并发写）。
- `raceManifest` 新增输出参数 `dialedOut []bool`。
- `fetchSeq` 只回滚 `dialed=false` 的候选：`!dialed[0] → delete(tried, ia)`、
  `!dialed[1] → delete(tried, ib)`；已真实拨号失败的源保留 tried，不被重复请求。
- 映射：slot0→ia、slot1→ib（raceTargets 保证 pair[0]→ia、pair[1]→ib）。

未改：预算模型、max_per_source、A/B 架构、阈值、场景。

## 测试

- 扩展 `mirror` fixture：新增 `mirrorWithStatus`（moMirror 已支持 status 字段），
  首路返回可控 500。
- 新增 `TestRaceFirstFailedSecondNoBudgetFallsBackToMirror`：
  - 首路 A：500 真实失败，dialed=true，tried 保留；
  - 次路 B：预占唯一 Lease → 预算跳过，dialed=false，tried 回滚；
  - 断言：命中 B、不重复请求 A（hitsA==1）、不回退官方、200 正文正确。
- RED 证据：`phase12-p1-red-before-fix.log`（修复前 status=502，实得摘要
  `da81ed46...` == officialBody 摘要，证实错误回退官方）。

## 验证（全部实测）

- RED：修复前定向测试 FAIL（见日志）。
- GREEN：修复后定向测试 PASS；原 P0 三项回归
  （TestRaceNoBudgetFallsBackToMirrorNotOfficial / TestRaceSecondCandidateBudget /
  TestRaceWinnerBodyFailureFallsBack）全 PASS。
- `go test -race ./internal/gateway/ -run '...' -count=10`：PASS，无 data race。
- P1 单独全量回归：`go vet ./...` RC=0、`go test ./... -count=1` RC=0。
