# 3.5.17 17b 实施进度（无人值守执行体维护）

> 分支：`feat/3.5.17-unified-framework`（从 c01b3ca 起）
> 权威输入：docs/3.5.17-{pre-freeze-audit,unified-framework-design,schema-design,test-matrix,implementation-plan}.md + freeze-manifest.sha256（5/5 校验通过）
> 1.6 权威文档 `docs/registry-roadmap-v1.6.md` SHA 76b81e99… 全程未修改。

## 阶段状态

| 阶段 | 内容 | 状态 | 提交 | 门禁测试 |
| --- | --- | --- | --- | --- |
| 1 基础设施 | RequestScope/R/P 两阶段门控/G 容量/P 耗尽 503/LeasePool/Preflight 共享 | **完成（框架+计量点接线）** | 见下 | T52/T57/T60 全绿 |
| 2 核心状态机 | Claim 三态、Chain、Attempt 四态、Body 幂等关闭、Lease 释放 | **完成（框架包，未接入 A 竞速执行路径）** | 见下 | T50/T51/T53/T56 全绿 |
| 3 跨站与身份 | Authority 唯一 Attempt、B Digest 投票、结果账本与全 Miss 仲裁 | **完成（框架包，未接入 Manifest 执行路径）** | 见下 | T63/B02/B06 全绿 |
| 4 路径接入 | A/B/fb/Hedge/Midfill/RetryToken 接入现有执行链 | 未开始 | — | — |
| 5 Schema 与迁移 | trusted_source_mappings 三表、Generation、API、迁移 | 未开始 | — | — |
| 6 测试与验收 | 全量回归、独立复核、性能验收 | 部分（全量 build/vet/test 已跑） | — | — |

## 阶段 1 变更

- 新增 `internal/unified`：`errors.go`（冻结常量与错误分类）、`budget.go`（R=16、P=8192 两阶段门控、
  RangeWorkCap=1024、分段 ≤256、每分段 ≤4 目标）、`scope.go`（RequestScope、候选/Generation 快照、
  每 Scope 一轮 Preflight）、`pool.go`（G 公式与全局池、LeasePool）、`ptransport.go`
  （P 计量点包裹底层 Transport、503+no-store+REQUEST_BUDGET_EXHAUSTED）。
- `internal/gateway`：`/v2/` Manifest/Blob 请求建 RequestScope 并挂 Context；P 计量点接线
  `registryAuthTransport.base` → `unified.PTransport` → 底层 Transport（Token 获取/认证重发/重定向都计入，
  Transport 内部重试不多算）；Scope 容量满返回 503 REQUEST_SCOPE_CAPACITY；预算耗尽在未发响应头的路径
  返回 503，且 `registryAuthTransport` 的 Token 分支不再把预算错误吞成 401。
- `main.go`：按 `G = N × max(W+1, C×A, 2) + 4` 建池，N 取 `limits.global_concurrency`、W 取 `parallel.workers`。

## 阶段 2/3 变更

- `claim.go`：ClaimKey=(Scope,Identity)，三态 Reserved/Dispatched/Released，epoch 保证旧句柄失效；
  同链多 Attempt 共享，跨链只允许一条获 Dispatched；Chain 关闭时统一释放。
- `chain.go`：Chain 生命周期 Created→Active→Closing→Closed，Closing 后 RegisterAttempt 被拒；
  Attempt 四态，Prepared 取消不消耗 R、Dispatched 取消阻止全 Miss；`CloseBody` 幂等。
- `vote.go`：200ms 投票窗口、正文已校验才计票、不同逻辑 Registry + 不同 source_id、窗口外丢弃、
  不一致转 Mismatch 继续补位、单候选转 Authority 比对；Authority 单次 Attempt + 结果复用。
- `ledger.go`：统一结果账本（保留 Chain 归属），仲裁顺序 成功 > P 耗尽 > policyErr > 未完成 >
  Auth > Cancelled > 全 Miss(404) > Fail；永不带负缓存。

## 验证（本阶段实际执行）

- `gofmt -l`：无输出（internal/unified、internal/gateway、main.go）。
- `go vet ./...`：通过。
- `go build ./...`：通过。
- `go test ./... -count=1`：全部通过（含 `internal/gateway` 71.7s 全量）。
- `go test ./internal/unified/ -count=1 -race`：通过。

## 明确未完成

- 阶段 4 未开始：A 站内竞速共享 Claim、B 跨站竞速/顺序补位、fb 静态抑制、Hedge 三指标拆分并接 P、
  Midfill 一轮 Preflight 共享（框架已提供 `Scope.Preflight`，但 `tryParallel`/`midFill` 尚未改用它）、
  RegistryRetryToken 接入顺序重试。
- 阶段 5 未开始：F08 三表、Generation、管理 API、迁移与回滚演练。
- 阶段 6 未完成：`go test -race ./...` 全量、独立复核、性能配对验收（B15）。
- 已知边界：P 耗尽在已发正文的流式路径只做入口检查（入口未耗尽、中途耗尽时由底层派发错误
  反映），尚未实现「取消剩余 Attempt + 清理部分文件」的完整收尾（属阶段 4）。
