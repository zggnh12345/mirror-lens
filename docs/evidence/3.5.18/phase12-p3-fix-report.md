# 3.5.18 P3 修复报告：补位统计语义（backfill / truncated_candidates 分离）

## 缺陷

`replica.go:974` 原为 `rec.Backfill = out.Truncated`，而 `out.Truncated`
（`crossrace.go:103` = `len(cands) - MaxCandidates`）是**竞速开始前的候选截断数**，
与 `CrossRunState.Backfill`（crossrun.go:27，注释"失败补位次数"）语义不符。
结果：`/api/cross-runs` 与 `mirrorlens_cross_run` 把候选截断伪装成补位次数，
误导 18f 运行审计。

## 修复（加字段，不破坏 API）

- `crossrace.go`：
  - `CrossRaceOutcome` 新增 `Backfill int`（真实补位次数）。
  - `RunCrossRace` 改为命名返回值 + `defer func(){ out.Backfill = backfillCount }()`，
    覆盖所有 early return。
  - 新增 `runBackfill(i)` 闭包统一封装首轮 C 之外的补位 `runCandidate` 调用
    （每次 +1），替换 `VoteMismatch`（:252）和
    `VoteWindowClosed/VoteAuthorityCompare`（:309）两处补位循环。
  - 首轮（:117/:162）与单候选调用仍用 `runCandidate`，不计补位。
- `replica.go:974`：`rec.Backfill = out.Backfill`（真实值）；
  新增 `rec.TruncatedCandidates = out.Truncated`。
- `crossrun.go`：`CrossRunState` 新增 `TruncatedCandidates int`
  （JSON `truncated_candidates`），**保留 `backfill` 字段不删**，API 向后兼容。
- `internal/api/web/index.html:924`：补位仍显示 `r.backfill`，
  另有截断时追加显示 `r.truncated_candidates`。

未新增 DB 表、未改 Schema（18f no-schema-change 约束保持）。

## 测试

- `TestCrossRaceMismatchFillQuorumBeforeAuthority`：补位 1 次成立 →
  断言 `out.Backfill == 1`。
- `TestCrossRaceTruncatesToM`：截断 2 个但首轮即一致 →
  断言 `out.Backfill == 0`（截断 ≠ 补位，语义分离）。
- API Web crossruns 字段集合测试通过。

## 验证（全部实测）

- `go build ./...` RC=0
- `go test ./internal/unified/ -run '...' -count=1` PASS
- `go test ./internal/api/ -run TestWebCrossRuns -count=1` PASS
- `go vet ./...` RC=0
- `go test ./... -count=1` RC=0
- `go test -race ./... -count=1` RC=0
