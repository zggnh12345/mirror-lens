# 3.5.18 P2 修复报告：B 层候选路径隔离共享 plan.Source

## 缺陷（静态审查发现、定向测试实测确认）

`crossRegistryManifest` 的 `run(i)` 把共享父级 `*plan` 传给
`fetchRegistryArtifact`。胜者在 `replica.go:1006`（及顺序分支 `:875`）写
`plan.Source`，而 abandoned 输家在 `RunCrossRace` 返回后仍可能在
`fetchReplicaArtifactCore` 的非 200 失败路径 `loggw` 读 `plan.Source`
（`gateway.go:1988` ← `replica.go:506`），两者无同步 → data race。

关键：竞速窗口关闭 ≠ 所有输家 goroutine 结束。cands 的 `Abort` 是空函数，
abandoned 输家继续跑，故不能靠"把写入移到 RunCrossRace 返回后"解决。

## 实测 RED（修复前）

定向测试 `TestReplicaRaceSlowLoserNoSharedPlanSourceRace`：快胜者 quay.io
（200）+ 慢失败副本 gcr.io（延迟 210ms 后 500）。`-race -count=40` 稳定命中：

```
WARNING: DATA RACE
Read  at ... loggw() gateway.go:1988
             fetchReplicaArtifactCore() replica.go:506
             fetchReplicaArtifact()     replica.go:460   ← 单 Source 分支
             fetchRegistryArtifact()    replica.go:608
             ... runCandidate / RunCrossRace goroutine
Previous write at ... crossRegistryManifest() replica.go:1006
```

证据：`phase12-p2-red-before-fix.log`。确认走的是**单 Source 分支**
（`:460`→`:608`），即两路径中最易漏的一条。

## 修复（fetchRegistryArtifact 入口隔离，覆盖两条路径）

- `fetchRegistryArtifact` 入口做一次独立快照 `base := *plan`（replica.go:606）。
- 单 Source 分支（`:608`）：传 `&base`，不再直接传共享父级 plan。
- 内层 A 竞速分支（`:643`）：每个内层 Source 从 `base` 派生 `pp := base`
  （不再 `pp := *plan` 读共享父级），保留内层 A 胜者 Source 记录语义。
- `fetchReplicaArtifactCore` 对 plan 只读（Validate/ExpectSHA/Header），
  浅拷贝安全，不改任何共享引用型字段内容。
- 外层 `crossRegistryManifest` 的 `:1006`/`:875` 仍是共享 plan.Source 的
  唯一回填点，只写最终胜者。abandoned 输家只操作自己的快照，不再读写父级。

七处既有读写点已逐一核对：gateway.go:418/419（X-ML-Source 响应头）、974、
1046、1976（loggw）、replica.go:875、1006。胜者回填后响应头/日志仍指向
最终胜者，语义不变。

## 验证（全部实测）

- RED：修复前 `-race -count=40` 命中 DATA RACE（见日志）。
- GREEN：修复后同命令 `-race -count=30` 全绿，无 data race。
- P2 单独回归：`TestReplica|TestCross|TestCharB|TestB19` RC=0。
- P2 单独全量：`go vet ./...` RC=0、`go test ./... -count=1` RC=0、
  `go test -race ./... -count=1` RC=0。

P1（c76c738）与 P2 分开实施、分开验证，未合并成一次全绿。
