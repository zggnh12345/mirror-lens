# 3.5.17 17b 实施进度（无人值守执行体维护）

> 分支：`feat/3.5.17-unified-framework`（从 c01b3ca 起）
> 权威输入：docs/3.5.17-{pre-freeze-audit,unified-framework-design,schema-design,test-matrix,implementation-plan}.md + freeze-manifest.sha256（5/5 校验通过）
> 1.6 权威文档 `docs/registry-roadmap-v1.6.md` SHA 76b81e99… 全程未修改。

## 阶段状态

| 阶段 | 内容 | 状态 | 提交 | 门禁测试 |
| --- | --- | --- | --- | --- |
| 1 基础设施 | RequestScope/R/P 两阶段门控/G 容量/P 耗尽 503/LeasePool/Preflight 共享 | **完成** | 307e3e0 | T52/T57/T60 全绿 |
| 2 核心状态机 | Claim 三态、Chain、Attempt 四态、Body 幂等关闭、Lease 释放 | **完成（框架包）** | 307e3e0 | T50/T51/T53/T56 全绿 |
| 3 跨站与身份 | Authority 唯一 Attempt、B Digest 投票、结果账本与全 Miss 仲裁 | **完成（框架包）** | 307e3e0 | T63/B02/B06 全绿 |
| 4 路径接入 | A/fb/Hedge/Midfill/RetryToken 接入执行链；B 框架完成、网关入口未接 | **基本完成（B 网关入口未接）** | c4ec936 | T50/T51/T55/T61/T62 全绿 |
| 5 Schema 与迁移 | F08 三表、Generation、管理 API、迁移/回滚演练 | **完成** | 46f44d7 | 生命周期/G 代次/并发/API/迁移演练全绿 |
| 6 测试与验收 | 全量回归、独立复核、性能配对验收 | **部分** | — | build/vet/test 全绿；race 见下 |

## 阶段 1 变更

- 新增 `internal/unified`：`errors.go`（冻结常量与错误分类）、`budget.go`（R=16、P=8192 两阶段门控、
  RangeWorkCap=1024、分段 ≤256、每分段 ≤4 目标）、`scope.go`（RequestScope、候选/Generation 快照、
  每 Scope 一轮 Preflight）、`pool.go`（G 公式与全局池、LeasePool）、`ptransport.go`
  （P 计量点包裹底层 Transport、503+no-store+REQUEST_BUDGET_EXHAUSTED）。
- `internal/gateway`：`/v2/` Manifest/Blob 请求建 RequestScope 并挂 Context；P 计量点接线
  `registryAuthTransport.base` → `unified.PTransport` → 底层 Transport（Token 获取/认证重发/重定向都计入）。
- `main.go`：按 `G = N × max(W+1, C×A, 2) + 4` 建池，N 取 `limits.global_concurrency`、W 取 `parallel.workers`。

## 阶段 2/3 变更

- `claim.go`：ClaimKey=(Scope,Identity)，三态 Reserved/Dispatched/Released，epoch 保证旧句柄失效。
- `chain.go`：Chain Created→Active→Closing→Closed；Attempt 四态；`CloseBody` 幂等。
- `vote.go`：200ms 窗口、正文已校验才计票、不同逻辑 Registry + 不同 source_id、不一致转 Mismatch。
- `ledger.go`：统一账本与仲裁（成功 > P 耗尽 > policyErr > 未完成 > Auth > Cancelled > 全 Miss > Fail）。

## 阶段 4 变更（c4ec936）

- `adapter.Plan` 增 `Repo`/`Ref`；`registryadapter` 用 `name`/`ref` 填。Claim Identity =
  规范化 `LogicalHost` + Repo + Ref；**CacheKey 不变**（设计 §2.2：身份只用于 Claim 仲裁）。
- `gateway/unifiedwire.go`（新）：`planIdentity`、`errUnifiedSuppressed`、链绑定/查询、
  `beginUnifiedAttempt`、`suppressFallbacks`、`takeRetryToken`、`hedgeAllowed`、`closeUnifiedChain`。
- 发送提交边界登记 Attempt：`openUpstream` 在 `g.cli.Do` 之前 Dispatch；`upstream.finishAttempt`
  在每次 `saveBody` 之后按 `(code, err)` 分类回写。同链多 Source 共享 Claim（A 站内竞速），
  跨链同 Identity 由 `errUnifiedSuppressed` 抑制，且按中性事件处理（不罚源、不写失败）。
- A 竞速：`raceManifest` 绑定 `ChainA`；竞速整场结束（胜者正文失败/未能成赛）时 `closeUnifiedChain`
  释放 Claim，顺序补位才能重新预占。所有落选路径 `att.abort()`，避免 Scope 在途计数与 R 账本悬挂（T54）。
- fb 静态抑制：`fetchSeq` 入口对 `suppressFallbacks` 拍快照，同 Identity 的备用目标整条剔除并计数（T61/F18/F19-14）。
- Hedge：`hedge_triggered_total`（决策）/`hedge_extra_requests_total`（真的多发的请求）/
  `hedge_wins_total{rank}` 三者拆分（D-03）；启动第二路前用 `hedgeAllowed` 检查 P 余额。
- Midfill：`tryParallel` 与 `midFill` 改调 `parPreflightShared`，每 Scope 一轮 Preflight；
  `rangeAdmit` 统一 RangeWork 准入（每 Range ≤4 目标、总 ≤1024、分段仍 ≤256）。
- B16：`fetchURL` 遇到网络类失败且该 Registry 令牌未用时，重试一次并计数；明确响应（4xx/5xx/无效正文）不重试。
- 部署期全局开关 `registry.unified_framework`（默认 true）：关闭时 `newRequestScope` 直接返回无 Scope，
  整条请求回到旧链，不存在半新半旧（T62）。
- B 跨站竞速：新增 `internal/unified/crossrace.go`（M≤4、首轮 C=2、票窗 200ms、
  不一致时只问官方权威一次再顺序补位、落选 Abort），7 个用例覆盖确认/不一致/单候选/截断/全失败/并发。
  **网关尚未接入**：缺「跨 Host 候选来源」的产品语义（F08 只表达「某 Host 的可信 Source」，
  没有跨 Host 同镜像等价关系），不在本轮自行发明，留待 17b 续跑与设计确认。

## 阶段 5 变更（46f44d7）

- `canonical.sql` 新增 `trusted_source_mappings` / `trusted_mapping_generation` / `trusted_mapping_audit`；
  `schema.Version` = 3.5.17，Canonical hash = `f040a8d2…`（DDL 追加后实测重算）。
- `store/trustedmap.go`：映射 CRUD + Generation 递增 + 审计快照同事务；`valid_from=0` 立即生效、
  `valid_until=0` 永久有效；Source 删除由外键级联，审计历史保留；代次行首次写入建立（=1）。
- API：`GET/POST/PUT/DELETE /api/trusted-mappings`、`GET /api/trusted-mapping-audit`；
  沿用管理员会话 + CSRF；409（已存在）/404（不存在）/400（校验失败）。
- `db-migrate`：新增 3.5.14 输入指纹与「直接复制」路径；迁移只建 Generation 行、不猜映射；
  3.5.14→3.5.17 迁移与回滚演练（隔离临时库，源库字节不变、旧库对新二进制 fail closed）通过。

## 验证（本阶段实际执行）

- `gofmt -l internal cmd main.go`：无输出。
- `go vet ./...`：通过。
- `go build ./...`：通过。
- `go test ./... -count=1`：全部包 ok（gateway 72.8s、api 15.9s、store 13.0s、db-migrate 19.1s）。
- `go test -race ./... -count=1`：见下方「最终一轮」结果。
- 环境：Go 在 `/opt/data/cache/scratch/toolchain/go`，构建前
  `export PATH=/opt/data/cache/scratch/toolchain/go/bin:$PATH GOCACHE=/opt/data/cache/scratch/gocache GOMODCACHE=/opt/data/cache/scratch/gomod`。

## 明确未完成

- **B 跨站竞速的网关入口**：框架与测试已就绪，但缺「跨 Host 候选发现」的权威语义（见阶段 4 说明）。
  这是 3.5.17 唯一的 P0 未闭环项；不得当作已完成，也不得用 A 竞速或单元测试顶替。
- 性能配对验收（B15）：未执行。没有真实外部 Registry 与生产同构负载，只能给出受控夹具结论，
  需在下一轮用 `tools/baseline` 做配对采样。
- P 耗尽在已发正文的流式路径仍只做入口检查；「取消剩余 Attempt + 清理部分文件」未实现。
- 真实进程验收（专用 Registry 夹具 + 真实公共源）本轮未做：本环境无外网 Registry，
  阶段 4/5 结论仅为**代码测试**层；不得写成 REAL_PASS。

## 17b 第三轮 · 阶段 7：F19 Claim 生命周期 + ManifestIdentity（P0）

- `internal/unified/claim.go`：`Release` 只允许「从未 Dispatched」的 Claim 释放；
  已 Dispatched 返回 `ErrClaimDispatched` 并保持状态。`Acquire` 对 Released 记录
  统一换 epoch（含原持有者），旧句柄失效。
- `internal/unified/chain.go`：新增 `active`（ActiveAttempts）、`attemptFinished`、
  `releaseClaimsLocked`、`ActiveAttempts`、`Dispatched`。`Close` 仅在 active==0 时释放；
  有在途 Attempt 时延后到最后一个 Attempt 结束。Prepared 取消不再直接释放 Claim，
  释放统一由链级条件决定。
- `internal/gateway/gateway.go`：删除 A 竞速结束时 `closeUnifiedChain(ChainA)`；
  竞速阶段结束后顺序补位继续绑定同一条 A 链（Race → SequentialFallback → Closed）。
- `internal/unified/manifestid.go`（新）：`ManifestIdentity{Ref, Accept}` + `NormalizeAccept`，
  保留媒体类型、参数、q 与顺序；q=1 与省略等价，大小写/空白/参数顺序无语义差异。
  `planIdentity` 从 `plan.Header["Accept"]` 取值，Claim 身份含内容协商。
- 测试：`claim_lifecycle_test.go`（Dispatched 粘滞、Prepared 释放、关闭延后释放、
  同链竞速→顺序补位、Closed 拒注册、并发收尾）、`manifestid_test.go`（q/顺序/等价）、
  `gateway/unifiedwire_test.go`（planIdentity Accept、A 阶段结束不释放 Claim）。
- 处置旧测试：`TestClaimEpochAfterRelease` 改为验证「Dispatched 拒释放 + 未派发换代」；
  `TestAttemptPreparedCancelKeepsR` 改为「活跃链不让出、关闭后让出」。
- 验证：`go vet ./...` 通过；`go test ./internal/gateway ./internal/unified -count=1` 全绿。

## 17b 第三轮 · 阶段 8：跨 Registry 副本映射 + B 网关入口

- Schema：`canonical.sql` 新增 `trusted_registry_replicas`、`trusted_replica_generation`、
  `trusted_replica_audit`；`Version` 仍 3.5.17，Canonical hash 重算为
  `bfc7f2f9e1c1e489c294ac8ee4b7f7d463017e789f14e703ff3e4e6e400bb4ae`。
- `internal/store/replicas.go`：精确匹配（无通配符、无同名隐式）、priority 固定排序、
  有效期、事务写 + 代次递增 + 同事务审计；校验覆盖通配符/路径穿越/副本=权威/非法字符。
- API：`GET/POST/PUT/DELETE /api/registry-replicas`、`GET /api/registry-replica-audit`，
  沿用管理员会话 + CSRF；409/404/400 语义与 F08 一致。
- 迁移：三张新表纳入 `allowedMissingSourceTable`（旧库空表补齐，不推断映射）；
  3.5.14 fixture 构造同步删除新表，仍逐字节命中冻结指纹 `7e5d71a7…`。
- `internal/gateway/replica.go`：跨 Registry 副本入口。先 `trusted_registry_replicas`
  解析仓库→仓库，再 `trusted_source_mappings` 解析 Host→Source；候选共享权威
  Claim 身份、缓存键仍是权威 CacheKey，认证按候选自己的逻辑 Registry 解析。
  两模式：`sequential` 按 priority 顺序、`race` 首轮两路 + 200ms 投票 + 顺序补位 +
  唯一一次 Authority（复用候选 0 的官方派发与其已校验正文）。
- `internal/unified/crossrace.go`：不一致分支改为「先剩余副本顺序补位 → 多数一致即确认
  （不同 Registry + 不同 Source）→ 仍不一致才用唯一一次 Authority」，禁止分歧时提前问权威。
- 配置：`registry.replica_mode`（off/sequential/race，默认关闭）+ `main.go` 接线 +
  `config.example.yaml` 说明；不创建映射、不改变旧行为。
- 测试：`store/replicas_test.go`（生命周期/校验/精确+排序/并发）、`api/replicas_test.go`
  （401/403/409/400/删除/审计）、`gateway/replica_test.go`（一致确认、分歧走权威且
  权威只派发一次、无 Source 映射不参与）。全部为受控夹具，记 SIMULATED_PASS。
- 处置旧测试：`TestCrossRaceMismatchUsesAuthorityOnceThenFills` 按新冻结顺序拆成
  「补位多数先确认」与「补位仍不一致才问权威」两条。

## 17b 第三轮 · 阶段 9：P 耗尽流式清理

- `internal/unified/scope.go`：新增 `bodies` 登记与 `TrackBody`、导出 `Exhaust`；
  首次耗尽时取消 Scope 派生 Context、关闭全部未完成 Body（幂等）；耗尽后
  新 Body 立即关闭。
- `internal/unified/ptransport.go`：`ReserveP` 失败即触发 `markExhausted("P")`，
  不再只返回错误。
- `internal/gateway/gateway.go`（openUpstreamAs/saveBody）：耗尽时不发起新派发；
  每次尝试用 `context.AfterFunc(sc.Context())` 取消；saveBody 读中断或提交前
  发现耗尽时清理临时文件并返回 `ExhaustedError`，绝不写缓存。
- `internal/gateway/stream.go`（streamBody/passThrough/fillStream/fail）：
  正文已输出后耗尽 → 中断传输、`DropPartial`、不提交缓存、不追加伪造 503；
  不记上游失败（outcome 中性）；耗尽后禁止换源/中途并行。未发头时统一 503。
- 准入：`hedgeAllowed`/`parPreflightShared`/`rangeAdmit` 全部先看 `Exhausted()`。
- 测试：`unified/exhaust_test.go`（多 Body 同时打开、取消在途、幂等、耗尽后拒登记）、
  `gateway/stream_exhaust_test.go`（中途耗尽不伪造 503、CachePut=0、.part 清理、
  零 checks 失败样本、耗尽阻断 Range/Preflight/Hedge、RangeWork 生命周期级不重置）。
- P=8192、Reserve/Commit 与计量点位置未改动。

## 17b 第三轮 · 阶段 10：独立复核与缺陷收口

独立复核（不复用实施者自评）对 `2217c6c..HEAD` 全量 diff 逐条核对冻结规则，结论为
「1 个 P0 + 5 个 P1」，已全部修复并补测试：

- **P0：副本路径漏 `ExpectSHA` 校验**（`internal/gateway/replica.go`）。内容寻址请求
  （`/manifests/sha256:…`）经副本取回时只校验上游自报 Digest，不比对客户端给的摘要；
  配合 `Immutable` 缓存会被永久命中。已按官方路径同一把闸修复，并加
  `TestReplicaSequentialRejectsWrongDigest`。
- **P1：整份落盘路径把预算耗尽记成上游失败**（`settleTarget`）。已加
  `IsBudgetExhausted` 早退，不记失败/不进冷却/不喂 AIMD。
- **P1：200ms 投票窗口不约束等待**（`crossrace.go`）。改为窗口内 `select` 等待，
  超时候选丢弃；迟到产物由 `slot.abandoned` 自行清理。新增
  `TestCrossRaceWindowBoundsSlowCandidate`。
- **P1：B 入口绕过 flight 合并**。新增 `replicaFlights`，同 CacheKey 并发只跑一轮
  竞速；领跑者失败不传染，跟随者各自回落 `fetchOnce`。新增并发合并用例。
- **P1：耗尽收尾顺序与覆盖面**。`markExhausted` 改为「先 cancel 再关 Body」；
  `doUpstream` / `rangeReq` 也挂 `withScopeCancel`，分片与 blob 主流随 Scope 取消；
  `streamBody` 换源后重新登记 Body。
- **P1（测试有效性）**：收紧断言——赢家固定为下标 0（不按完成先后）、补位复用首轮
  候选、同 Registry/同 Source 不计票、全分歧不猜交付、`.part` 先证存在再证清理、
  耗尽后 Preflight 真实零上游命中、迁移副本三表必须为空、代次一次变更只 +1、
  审计行数/操作者/快照、PUT 修改与 404、旧句柄不能释放新持有者的 Claim。
- **顺带修正**：`PUT /api/registry-replicas` 不再隐式新建（不存在返回 404）；
  `ReserveP` 只对真正的额度耗尽触发收尾；`classifyOutcome` 非 2xx 不再一律记 OK。

已知且已记录的边界（不阻塞本版）：`NormalizeAccept` 的 q 值语义在生产路径上被
`registryadapter.UpstreamAccept` 先截断，因此「q 值保序」目前只在 unified 单元层
成立；副本 Generation 只做观测快照，不参与内容身份（与 F19 身份定义一致）。

## 迁移来源核验（阶段 6）

- 未访问、未修改生产库（权限边界）；生产实际版本/指纹记为 **NOT_EXECUTED**。
- 隔离证据：`inputSchemaFingerprints["3.5.14"] = 7e5d71a7…` 与
  `docs/3.5.14-implementation-status.md` 记录的最终 Canonical Hash（生产副本
  `prod-14c2.db` 实测）逐字节一致；3.5.15/3.5.16 未改 Schema，生产 3.5.16 的
  Schema 仍是 3.5.14，因此 3.5.14→3.5.17 就是生产升级路径。
- `TestMigrate3514To3517AndRollback` 实测通过：升级后 `schema_hash` 匹配 3.5.17，
  副本三表为空，回滚库指纹保持 `7e5d71a7…`，新二进制对 3.5.14 库 fail closed。

### 复核第二轮（针对修复提交）

第二轮独立复核在 `5d7adca` 上复现出一个新 P0：竞速窗口从「竞速开始」计时，
首轮两路都慢于 200ms 时窗口先关、零票 → 无结论，而竞速已派发的 Claim 是
`dispatched`，顺序回退换到别的链被 F19 抑制 → 502。已修复：

- `crossrace.go`：窗口从**首票**起算（`firstArrived`），超时只丢弃慢候选。
- `gateway/replica.go`：副本路径与它的顺序回退统一绑定 `ChainB`（同链顺序补位），
  并在无结论时真正使用同链 `fetchOnce`。
- `dropAll`/`slot.abandoned` 的交接放进同一把锁（`close(done)` 在锁内），
  消除「既没删也没标记」的永久临时文件窗口；`settleTarget` 之后的兜底读取
  也改为持锁 + `done` 判据。
- leader panic/空结果时 followers 回落 `fetchOnce`，不再返回 `(nil,false,nil)`。
- `doUpstream` 补上 `Exhausted()` 前置检查与 `withScopeCancel`（上一轮声明未落地）。
- 新增回归：两路 300ms 仍确认且 <1s、竞速无结论时同链回退不 502、
  两路慢速网关端到端无 502。

### 最终门禁（17b 第三轮）

- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...` 通过。
- `go test ./... -count=1`：33 个包全部 ok。
- `go test -race ./... -count=1`：33 个包全部 ok，`EXIT=0`（日志 `race-final.log`）。
- T50–T63 定向：`TestClaim*`/`TestChain*`/`TestClosedChain*`/`TestSameChain*`（T50/T51/T53）、
  `TestCrossRace*`（T63/B 竞速）、`TestLedger*`/`TestB02*`/`TestB06*`（T52/T59/T60）、
  `TestHedge*`（T54）、`TestMid*`/`TestChunkLease*`/`TestLeaseBody*`（T55/T56）、
  `TestFallbackStaticSuppression`（T61）、`TestUnifiedOff*`（T62）、
  `TestRangeLedgerIsScopeLifetime`/`TestExhaust*`（T60 的 P/Range）、`TestReplica*`（B 入口）全部通过。
- 性能：无真实外部 Registry，按边界记 **EXTERNAL_BLOCKED**；未做同环境 ≥5 组配对，
  不声称任何 P95 改善。`TEST_PASS` 与 `EXTERNAL_BLOCKED` 分开。

## 第四轮（17b 最终复核前收敛）

### 阶段 A：1.6 权威文档同步

- 目标文件 `docs/registry-roadmap-v1.6.md`（唯一权威正文）。
- 修改前 SHA-256：`a1c11472660b22915e63fa73e9bd4ee4945470ff81e1c0d8da9b710f3dab90a2`
- 修改后 SHA-256：`1b38a071a7de36d5d47817f53e81859df54b155e6493339f99c9ab49d90e92bc`
- 改动只做收敛与同步，不改已冻结语义：
  - 第 3 行状态与 3.5.17 节状态：改为「3.5.17 实现已进入最终复核；全服务 G 与 F08 Schema
    已冻结并入第 11 章」。
  - 11.1 表的「实现状态」列：M/C/权威站/fb/B16/F18/F19 由「待 17a」改为已实现待复核；
    R 与 F19 标注「已实现并有门禁用例」。
  - 新增 11.6 预算与资源常量（D-01~D-06、AUD-04-1~7、R=16，RangeWorkCap=1024 /
    每 Range≤4 / Segment≤256 明确为 RequestScope 生命周期级，不是每 Range 重算）。
  - 新增 11.7 F08 三表与跨 Registry 三表、两类 Generation 互不替代、迁移与回滚边界。
  - 新增 11.8 投票窗口计时起点（首个**正文已校验**候选起算）与三层分账
    （Identity Claim 各逻辑 Registry 各自 / RequestScope 唯一提交权 / CacheKey 验证后归属）。
  - 11.4 由「尚未完成设计」改为「已全部冻结，正文见 11.6–11.8」；11.5 标题标注 17b 已落地。

### 阶段 B/C：权威站边界、FlightKey、AfterFunc 回收、B03/B17/B19/B20

代码事实与修改（提交见下一条）：

1. **权威站不进首轮 C / 不占 M**（`internal/gateway/replica.go`）
   - 旧：`targets[0]`（权威官方）也被塞进 `cands`，`FirstRound=2` 时首轮 = [权威, 副本1]。
   - 新：`cands` 只由 `targets[1:]` 构造（副本），`Authority` 仍是记忆化的 `run(0)`；
     新增冻结常量 `crossReplicaM=4` / `crossReplicaC=2`，M 只数副本。
   - 胜者下标 `out.Winner` 是副本下标，落槽位时 `+1` 映射回 `targets`。
   - 测试：`TestReplicaAuthorityNotInFirstRound`（两副本一致时官方 0 派发）、
     `TestReplicaAuthorityNotCountedInM`（5 条映射只出 4 个副本）、
     `TestReplicaCandidatesShareSingleClaim`（同一请求仅 1 条 Claim Dispatch、身份是权威逻辑 Registry）。
2. **Claim 身份注释**（`replica.go` 文件头）：改为「共享权威 Identity 的同一份提交权」，
   并明确不合并不同 Registry 的 Claim；逻辑见 1.6 §11.8。
3. **投票窗口计时起点**（`internal/unified/crossrace.go`）
   - `firstValid` 只在正文已校验后关闭；失败候选只触发安全网 `anyDone`（一个窗口内
     仍等有效候选，最多 2 个窗口），不再让失败候选提前关闭窗口。
   - 测试：`TestCrossRaceWindowStartsAtFirstValidCandidate`、`TestManifestVoteOfferAfterCloseIgnored`
     （窗口关闭后 Offer 被拒、Votes/Agreed 不变）。
4. **FlightKey 按执行策略命名空间**（`replica.go::crossFlightKey`）
   - `replica|CacheKey` → `cross-registry-<race|sequential>|CacheKey`，不改 CacheKey。
   - 测试：`TestCrossFlightKeyNamespaced`（两档互异、与 A 的 `\x00policy=registry-race` 互异）。
5. **AfterFunc 回收**（`internal/unified/scope.go::RegisterCleanup` + `gateway.go`）
   - `withScopeCancel` 与 `openUpstreamAs` 的 stop 全部登记到 Scope，派发结束即注销、
     Scope 关闭统一回收；`CleanupCount()` 供断言。
   - 测试：`TestScopeCancelCleanupReleased`、`TestScopeCancelFiresOnExhaust`。
6. **B03 可变 Tag 版本一致性**：`mutableTagRef` 判定；race 模式下副本胜者若是可变 Tag，
   必须与唯一一次 AuthorityAttempt 的 Digest 一致才提交，否则交回顺序路径。
   测试：`TestReplicaTagRequiresAuthorityAgreement`、`TestReplicaDigestSkipsAuthority`。
7. **B17 两档模式**：settings `cross_registry_mode`（sequential 默认 / race / off），
   UI 设置优先于部署期 `registry.replica_mode`，10s 快照 + API 主动失效。
   `GET|PUT /api/cross-registry`，面板 `#crossSec` 两档 `sseg` + 参选勾选列表。
   测试：`TestCrossRegistryAPIMode`、`TestReplicaModeDefaultsAndLiveSwitch`。
8. **B19 私有 Host 隔离**：权威站或副本站是私有认证 Host 时直接不产生跨站候选。
   测试：`TestReplicaExcludesPrivateAuthority`。
9. **B20 参选勾选**：`cross_registry_selected_sources`（settings 单键、默认空、读时
   与星标求交），写入要求目标已星标，设置与审计同事务。
   测试：`TestCrossRegistryAPISelection`、`TestReplicaEmptySelectionDegrades`、
   `TestReplicaRequiresSelection`、`TestCrossSelectedStaleEntryFiltered`。

**未判定为缺陷的行为**：Authority 失败后回到 `fetchOnce`（原有顺序路径）会再发一次
官方 Attempt。这不是第二个 AuthorityAttempt（`run(0)` 由 `sync.Once` 记忆化），
而是 B.4「全部候选失败后按权威 Registry 原有错误语义返回」，其用量仍受 R 约束。

B17 的 UI 只做了静态接线与 API 验证，没有浏览器点击验收 → 记 `SIMULATED_PASS`。

### 阶段 D/E：G0 矩阵与 G 边界

- 新增 `docs/3.5.17-g0-matrix.md`：8 项 G0 逐条映射到实际测试名（含反向核对：
  文档引用的每个 `Test*` 都存在于测试树，差集为空）。第 7 项此前只有实现、没有
  专用断言，本轮抽出 `pickPartial` 并补 `TestPickPartialRejectsOtherSource`。
- G 边界：`TestGlobalCapacityBoundaries`（W=2 → 804，段数由 C×A 决定；W=1 回填 4；
  C 传大值收敛到冻结 2；N 缺省 200）、`TestPoolNeverUnbounded`（容量 0/负数回填 1004、
  真信号量占满即拒）、`TestDisabledStillKeepsInternalCapacity`（limits.enabled=false
  时 N 仍回填 200，内部 G=1004，不存在「关闭外部限流 = 无上限」）。
- `GlobalCapacity` 的 C 上限收敛到 2（与 A≤2 同口径）：此前只 clamp A，传入更大 C
  会按倍数放大 G。

### 阶段 F/G：最终门禁与性能

- `gofmt -l .` 无输出；`go vet ./...`、`go build ./...` 通过。
- `go test ./... -count=1`：33 包 ok，`EXIT=0`。
- `go test -race ./... -count=1`：33 包 ok，无 FAIL，`EXIT=0`。
- T50–T63 定向与 `TestReplica*`（19 个）全部通过；Schema 升级/回滚演练
  （`TestMigrate3514To3517AndRollback` + staging 失败清理 4 子项）通过。
- 真实进程重启演练：**NOT_EXECUTED**（不改变代码基线）。
- 性能：**EXTERNAL_BLOCKED**（无真实外部 Registry）。进程内配对 5 组：
  B 关闭 6096 ns/op（中位），M=4 全配置 122811 ns/op（中位）——只作冷请求
  候选发现成本的量级参考，不作端到端时延结论。
- 详细复核表、未关闭风险与判断依据：`docs/3.5.17-17b-r4-review.md`。
- 状态：**READY_FOR_FINAL_REVIEW**。

## 第五轮（最终证据闭环）：真实进程 / 浏览器 / G0 流式

本轮只做取证，不改产品语义。所有演练在隔离临时库
（`/opt/data/cache/scratch/r5/run1|run3|run4`）与本地构建二进制上完成，未触碰生产库与容器。

### 1) 真实进程重启演练 — REAL_PASS

- 证据：`docs/evidence/r5/restart-drill.json`、`restart-drill-checks.json`、
  `mirror-lens-first.log`、`mirror-lens-second.log`、`upA.jsonl`、`upB.jsonl`。
- 内容：真实二进制起两次（同 DB），HTTP API 建跨站设置/副本/可信映射，
  SIGTERM 停、同 DB 重启。核验结果（脚本比对，非目测）：设置 mode=race 与
  selected=[1,2] 保持、副本 `generation=2` 与条目逐字段一致、映射 `generation=2`
  与条目一致、审计在重启后可读、`schema_meta` 三次快照均为
  `3.5.17 / bfc7f2f9…`。重启后仍改 mode 并生效（sequential）。
- 请求层：sequential tag 与 race digest 两次均 200，`X-ML-Source=quay-mirror`，
  正文 SHA-256 与 digest 一致。

### 2) 浏览器 B17 点击验收 — REAL_PASS（chromium 131 headless）

- 证据：`docs/evidence/r5/browser/`（4 张截图、`b17-result.json`、`b17-drill.json`、
  控制台与 29 条 API 网络记录、进程与上游日志）。
- 8 项断言全绿：默认顺序回退、切竞速并落库（PUT 200 + `configured=race`）、
  刷新恢复、未星标 id 被 400 拒绝且 UI 不提供该勾选项、撤星后勾选项数量减一且
  id 不在 starred、B 模式变化不改 A 的 `multi_origin_hosts` 与其它源星标。
- 未完成：**参选为空退化 / 竞速真的并行**未能在本夹具取证——进程日志为
  「并行填充：退回单流（只有官方源可用）」，即受控源没被选进候选，
  上游命中数为 0。属夹具限制，不是产品结论。

### 3) G0 真实流式演练 — PARTIAL

- 证据：`docs/evidence/r5/g0-stream-drill.json`、`docs/evidence/r5/g0/`（每场景
  一份 mirror-lens 日志与上游 JSONL）。
- 已真实覆盖：慢响应头（1500ms 延迟仍 200 且摘要正确）、慢 Body（400KB 分段 100ms）、
  多路 Body 并发（3 个不同仓库同时开流，均摘要正确）、200 前缀坏字节
  （日志「内容校验失败，不入缓存」，缓存无条目）、断流（半途截断，客户端拿到
  短正文，`cache_entries` 无条目、无残留 `.part`）。
- 未覆盖：中途换源与 206 长度不符。原因：`trySwitch` 只换到「最近样本 ≥
  switch_min_mbps」的源，而速度样本要求单次 ≥16MB（`minProxySpeedBytes`），
  本夹具预热后 `proxy_speed` 仍为空行，日志固定输出「没有可换的星标源」；
  206 续传在源被计一次失败后即退出候选（`source_health_scope.fails=1`），
  第二次请求落到官方 401。
- P 耗尽时仍有 Hedge/Midfill 在跑：真实进程无法凑出 P（8192 为进程级，
  夹具不可达），仅代码测试覆盖。

### 4) B19 私有认证矩阵 — TEST_PASS

- `internal/gateway/b19_private_matrix_test.go`：权威 Host 私有不入跨站候选、
  副本 Host 私有不入候选、Authority 凭据不问副本、副本凭据不问 Authority、
  私有请求不写共享缓存、显式私有前缀两次都走 BYPASS。
- `go test ./internal/gateway/ -run TestB19 -count=1 -v`：3 个用例全 PASS。

### 5) G 公式与限流关闭边界 — TEST_PASS

- `TestGlobalCapacityFormula`（200/4→1004、200/16→3404）、
  `TestGlobalCapacityBoundaries`（200/2→804、W=1 回填 4、C 收敛 2）全 PASS。
- `internal/gateway/scope_capacity_limits_off_test.go`：`limits.enabled=false` 时
  Scope 仍以 G 为硬准入上限（并发派发数 ≤ G，超出的排队而非放行）——PASS。

### 6) 交付信息

- HEAD 与提交链、变更文件 `--stat` 见本轮报告；未推送、未打 tag、未建 Release。

### 7) 独立复核

- `go build ./...`、`go vet ./...` 通过；`go test ./... -count=1` → 33 包 ok，EXIT=0
  （`docs/evidence/r5/full-test.log`）；`go test -race ./internal/gateway ./internal/unified
  -count=1` → ok（`docs/evidence/r5/race.log`）。
- 结论按证据层分账，未用单测代替真实进程，也未用真实进程覆盖不可达项。

### 状态：BLOCKED

缺口仅第 3 项的两条：中途换源、206 长度不符（其余子场景已真实通过），
以及「P 耗尽仍有 hedge/midfill」只能代码级覆盖。复现路径：
`R5_CASES=switch_ab,switch_lenmismatch python3 /opt/data/cache/scratch/r5/drill3b.py`。

---

## 第六轮（17b 核心运行时证据闭环，2026-11-20）

第五轮遗留的三条缺口（跨站副本真实并行、真实中途换源、206 长度不符）与
P 耗尽集成，本轮全部用**实际二进制 + 本地假上游 + 真实 SQLite + 真实 HTTP**
补齐。全程未改任何生产调度/缓存/回退/预算规则；为定位问题新增的日志只在
debug/INFO 记录事实，不改变选择结果；唯一的产品行为修复是补上
`Plan.LogicalUpstream`（见下）。B15 仍为 EXTERNAL_BLOCKED，生产 Schema/迁移仍
NOT_EXECUTED，未访问生产库。

### 1) 跨站多 Registry 副本真实并行 — 通过（受控上游）

- 证据：`docs/evidence/r6/crossrace-runtime/`（候选诊断 JSON、A/B/C 三份上游 JSONL、
  Gateway 日志 `副本候选诊断`/`副本候选失败`、请求结果、Digest 校验、并行重叠判定）。
- 诊断定位：原候选为 0 的根因是**假上游 A 的 URL 与逻辑 Host `gcr.io` 不一致**，
  `sched.Rewrite` 拿官方基址改写候选后仍指向同源而被跳过；逐一输出
  `trusted_registry_replicas`/`trusted_source_mappings`/`cross_registry_selected_sources`/
  星标/`source_host_caps.effective_status`/健康/recovery gen/Scheduler 选中/Rewrite
  逐项后确认——**不是**星标/健康/私有认证/M/C 门禁问题。修正夹具（让 A 成为真正的
  `gcr.io` 上游）后，在真实规则下两个不同 Registry（quay.io / registry.k8s.io）的
  副本 Source 成为有效候选。
- 断言（12/12 全过）：两副本 200 且 Digest 正确、起止时间区间重叠且第二在第一个完成前
  已启动、Authority 不进入首轮 C（请求级 `副本候选失败 target=official` 日志核对，
  并用"两副本内容不一致→真问一次官方 401"控制组证明该观察手段灵敏）、胜者 Digest 正确、
  落败 Body 已关（无 cache 临时 fd 残留）、无跨 Registry 错误缓存提交、缓存单条目；
  空参选（独立的 `seq/app`，`cross_registry_selected_sources` 为空）不派发副本、
  A+B+C 命中增量 ≤1、不自动把全部星标加入候选。
- 提交：`671fb75`。

### 2) 真实中途换源 — 通过（受控上游）

- 证据：`docs/evidence/r6/switch/`（switch-runtime.json、upA/upB JSONL、Gateway 日志、
  trySwitch 触发日志、Range 请求/响应头、最终 SHA-256、缓存与 `.part` 状态）。
- 产品行为修复（有据）：流式路径把 `plan.Upstream` 改写成本次供数源后，`targets`/
  `parallelTargetsN` 继续拿它当 `Rewrite` 基址，导致换源候选永远凑不到第二台
  （`Rewrite` 失败→只剩官方）。补上 `Plan.LogicalUpstream` 保留官方基址后，
  换源候选从 1（仅官方）恢复到 3。生产门禁 `minProxySpeedBytes=16MiB`、
  `switch_min_mbps=8`、`watch_mbps=3/watch_sec=10/switch_min_file_mib=60` 全部不动。
- 断言（10/10 全过）：B 用 3 次真实 ≥16MiB 整份下载形成速度样本并进入 Scheduler、
  中位数 46MB/s > 8MB/s 门限；A 慢源先发 12,582,912B 前缀后被判慢断连
  （`client_closed=true`），B 真发 `Range: bytes=11534336-` 并回 206
  （`Content-Range` 总长正确、实发 55,574,528B）；最终 64MiB 正文 SHA-256 == digest；
  无 `.part` 残留、单一缓存条目；未触发中途并行（分片未启用，符合预期）。
- 提交：`a2e696f`。

### 3) 真实 206 长度错误 — 通过（受控上游，以换源为前置）

- 证据：`docs/evidence/r6/range-validation/`（三个场景原始 JSONL + 日志 +
  range-validation.json 汇总）。均先让 A 判慢触发换源，再由 B 对 Range 返回缺陷 206。
- 三场景：① `bad206`（Content-Range 总长 `67108961≠67108864`）：`switchAcceptable`
  响应头校验阶段拒绝，B 字节未读（bytes_sent=0），A 源补全，最终 digest 正确，缓存由 A 写；
  ② `cl_bad`（Content-Length 多 512）：同样头校验阶段拒绝；③ `trunc206`（头合法但 body
  只发一半）：接手后未完整结束→日志「换源后未完整结束，丢弃续传点」，缓存无条目、
  `.part` 清空、坏字节未记为源成功（客户端 digest 不符，但不入缓存）。
- 断言 4/4 全过。提交：`43f7706`。

### 4) P 耗尽时 Hedge/Midfill 真实集成 — 通过（测试注入小 P）

- 测试：`internal/unified/p_exhaust_integration_test.go`（5 个用例全 PASS）。
  真实 `PTransport.RoundTrip` + 本地 `httptest` 上游 + 真实 `Response.Body`，
  **不用伪造计数器**：服务端实到请求数与 P 提交数交叉核对。
- 覆盖：主链/Hedge 两路真派发后在途耗尽→Scope Context 取消、2 个 Body 全关（各一次）、
  第三次派发在 ReserveP 被拒且服务端 0 新增请求、committed 不回退、结果账本 0 行
  （不污染 Source 失败指标）；Preflight+Range 共用同一 Scope P，耗尽后 ReserveP 拒绝
  新 Range/trySwitch；`ReserveP/CommitP` 提交前取消释放、已提交不返还、8 并发抢 3 名额
  零超额、重复结束只生效一次；生产构造路径零值 `ScopeConfig` 恒 `PMax=8192` 冻结。
- 提交：`43f7706`。

### 5) 清理与可重复性

- `race-final.log`（2618B，第五轮全量测试输出）判为历史证据，已归档到
  `docs/evidence/r6/archive/race-final.log`，未销毁。
- `r5drill/` 依赖 `.git/info/exclude` 绕过扫描：已移到 `testdata/r5drill/main.go`
  （Go 工具链不扫描 testdata），并从 `.git/info/exclude` 删除该行。
- `go list ./...` 确认不再出现 `r5drill` 包；临时容器/网络/DB/进程结束均为空。

### 6) 独立复核（最后一次代码修改后的全量重跑）

- `gofmt -l .`：空；`go vet ./...`：OK；`go build ./...`：OK
  （`docs/evidence/r6/verify/static.log`）。
- `go test ./... -count=1`：33 包 ok，EXIT=0（`verify/gotest.log`）。
- `go test -race ./... -count=1`：33 包 ok，EXIT=0，无 DATA RACE（`verify/gotest-race.log`）。
- 测试名全集 1397 行（`verify/test-names.txt`）；文档引用测试名反向核对：19 个"缺名"
  中 18 个为通配前缀（`TestHedge*`/`TestReplica*` 等，均有真实测试），1 个
  `TestCrossRaceMismatchUsesAuthorityOnceThenFills` 为 PROGRESS 明确记载的旧名，
  已按冻结顺序拆成 3 个现有测试——无虚报。
- T50–T63：T50/51/52/53/56/57/59/60/63 有同名测试；T54/T55/T58/T61/T62 为矩阵
  编号，映射到 `TestHedge*`/`TestMid*`、T58 注释、`TestFallbackStaticSuppression`、
  `TestUnifiedOff`，均有真实断言。B01–B20 有 `TestB02*`/`TestB06*`/`TestCrossRace*`/
  `TestDigestConsensus*` 等成组用例。B19 矩阵 `go test -run TestB19` PASS。
- G0 矩阵证据分账已更新：代码测试 PASS；真实进程层由本轮受控上游补齐
  （SIMULATED_PASS 口径）；公共第三方 Source 仍 NOT_EXECUTED。

### 7) 状态与交接

- 状态：**READY_FOR_FINAL_REVIEW**（内部证据齐全，不等于发布或生产完成）。
- 四个提交：`671fb75`、`a2e696f`、`43f7706`（+ G0/PROGRESS 文档提交见最终 git log）。
- 本地提交、未 push/tag/Release/生产切换。B15 EXTERNAL_BLOCKED；生产 Schema/迁移
  NOT_EXECUTED；未访问生产库。

## P2 收尾（第六轮最终两项）

### P2-1：P 耗尽证据归档 — 关闭

新增 `docs/evidence/r6/p-exhaustion/`（README、commands.txt、p-exhaustion.log、
result.json、production-budget.txt）。全部由本轮重新执行的真实测试生成：

- Hedge：`pmax_injected=2`、`dispatched=2`、`rejected=1`、`bodies_created=2`、
  `bodies_closed=2`、`on_exhaust_calls=1`、`ledger_rows=0`
- Midfill：`preflight_runs=1`、`range_work_used=4`、`range_work_max=1024`、
  `segments=1`、`committed=2`、`reserve_after_exhaust_rejected=2`
- Reserve/Commit：`committed=1`、`outstanding=1`、并发 `committed=3`/`rejected=5`
- 生产预算：`frozen=8192`、`scope_max=8192`（`internal/unified/errors.go:39`）

测试文件仅补 `t.Logf` 可观测字段（提交 `9e4c2bb`），无生产逻辑变更。

### P2-2：LogicalUpstream 提交拆分 — 关闭

- 备份分支：`backup/3.5.17-before-p2-cleanup-60677cd`
- 原 `a2e696f` 拆为：
  - `8d72bd4` `fix(gateway): preserve logical upstream across source switches`（生产行为）
  - `0d6a939` `test(gateway): switch drill evidence and candidate rewrite coverage`（证据/脚本）
- 新增回归测试 `internal/gateway/logical_upstream_test.go`（提交 `419dda2`）
- 树一致性：拆分前 `a2e696f^{tree}=9c93ac0d…`，拆分后同点 `9c93ac0d…`；
  rebase 完成后 `HEAD^{tree}=4063887d…`，与拆分前 HEAD 完全一致。
- 未使用空提交、未重复 Diff、未改变最终代码内容。

### 收尾后 HEAD

`9e4c2bbaff52f72d1a2ee13b69b19d75a767ec94`（工作区干净）

外部边界不变：B15 `EXTERNAL_BLOCKED`；生产 Schema/迁移 `NOT_EXECUTED`；
未 push、未打 tag、未发布、未触碰生产。

## 收口修订：B08 已知范围缺口（2026-11-20）

**结论变更：** 撤销无条件 `READY_FOR_FINAL_REVIEW`，改为
`CORE_IMPLEMENTED_WITH_KNOWN_SCOPE_GAP`。

**事实：** B08（A+B 嵌套竞速）未实现，且不是证据缺口——

- `internal/gateway/registry.go:166`：`fetchWithReplicas` 先于 `fetchOnce`，B 有结论即返回；
- `internal/gateway/replica.go:264`：`fetchReplicaArtifact` 每副本只发一路 `openUpstreamAs`；
- `raceManifest(` 唯一调用点 `internal/gateway/gateway.go:1143`，仅 A 路径可达。

**影响：** B13 的 A+B 组合预算 `NOT_VERIFIED`；B15 的 A+B 配对性能无从测量。

**处置：** 本版本不补实现。A+B 嵌套涉及 R/C/A/P/G/Claim/Authority/Cache
已冻结语义，须单独立项。1.6 已同步：状态行、3.5.17 实现范围、B08/B13 行、
11.1 行，并新增 11.9。

**边界不变：** 代码零改动（HEAD 树未变）；B15 `EXTERNAL_BLOCKED`；
生产 Schema/迁移 `NOT_EXECUTED`；未推送、未打 Tag、未发布、未触碰生产。
