# 18d 第一阶段记录：A/B characterization 锁行为 + 嵌套 RED 证缺口

版本 3.5.18 / 阶段 18d 第一阶段 / 分支 feat/3.5.18
基线 HEAD：56335abf1dc66ab6c3425ee3411339d7a053d5a2
**本阶段只新增测试文件 `internal/gateway/nested_phase1_test.go`，未改任何生产调度代码。**

## 用户裁定（执行依据）

选 1：实现完整嵌套竞速，路径「先锁测试 → RED → 重构 → 全量回归」，不降级、不延期。
本阶段为授权限定的第一阶段：先证明 A、B 独立行为被锁定，再证明嵌套缺口存在；
禁止改调度器核心、禁止重算冻结的 R/G/C/A、禁止缩小功能范围。

## 一、只读专项复核（P2-3，纯只读）

| 复核项 | 结论 | 代码事实 |
|---|---|---|
| Claim 释放门 = 链关闭 + ActiveAttempts==0 + 从未 Dispatched | 成立 | `internal/unified/chain.go:206-246`、`:248-262`、`:274-287` |
| Chain 生命周期 Open→Closing→Closed，Closed 拒绝注册 Attempt | 成立 | `chain.go:37-43`、`:166-175` |
| ReleaseClaim 门 + Dispatched 永久保留 | 成立 | `chain.go:229-246` |
| 投票状态机 CloseWindow 后拒绝 Offer | 成立 | `unified/crossrace.go:201` |
| B.3 三方 Digest / 失败分类 / Fail Closed 不回退 | 成立（设计已冻结） | 冻结稿 §4.3 失败类型表 |
基础设施 3.5.17 已落地并验收，无需改动。

## 二、A/B characterization 测试（8 项，全部 PASS，锁外部行为）

文件：`internal/gateway/nested_phase1_test.go`。只锁外部可观察行为，不依赖函数调用顺序/内部变量。

| 测试 | 锁定行为 | 结果 |
|---|---|---|
| TestCharA_RaceEligibilitySingleSource | A 资格门：单星标源单路、成功不回官方 | PASS |
| TestCharA_AtMostTwoSources | A 并发上限 A<=2（3 源最多启动 2） | PASS |
| TestCharA_FailureBackfillsWithinHost | A 失败立即补位（首路 500 → 次路交付） | PASS |
| TestCharA_BadWinnerNotCached | A 坏赢家正文不污染缓存、回退正确内容 | PASS |
| TestCharB_AgreementConfirmsWithoutAuthority | B 两副本一致直接确认、权威零派发 | PASS |
| TestCharB_MismatchFallsBackToAuthority | B 不一致唯一 Authority 兜底、只交付权威正文 | PASS |
| TestCharB_AuthorityNotCountedInM | B M=4、权威不占名额 | PASS |
| TestCharB_SingleRequestClaim | B 副本共享唯一 Claim（单请求单提交权） | PASS |
| TestCharB_WrongDigestReplicaNotCommitted | B 错 Digest 副本不成为胜者、不污染缓存 | PASS |
| TestCharHelper_ScopeBudgetObservable | RequestScope 的 R/P 预算与 Claim 可观测（供重构断言） | PASS |

以上均在未改生产代码的基线 56335ab 上通过，作为重构回归硬门禁。

## 三、嵌套 RED（编译成功 + 因行为缺失失败）

`TestRedNestedBRegistryRunsASourceRace`：
- 前置：给副本 Registry `quay.io` 配 2 个可信星标 Source，A 星标竞速开关打开；
  实测「合格星标 Source=2（cands=2）」——排除 Mock 配置错误导致的假 RED。
- 用 `blocked` channel 并发屏障让 source-1 卡住、source-2 立即可用，不依赖固定 Sleep。
- 期望（冻结语义）：A+B 双竞速时两个 Source 都应被尝试（hits2>=1）。
- 实测：`hits2=0`，断言失败。定性：编译成功、前置条件成立、因目标行为缺失失败。
- 隔离：默认 `t.Skip` 以保全量门禁绿；设 `ML_NESTED_RED=1` 运行取证。
  开关只控制执行与否，不放宽断言；18d 核心重构阶段将移除开关转为常驻。

## 四、缺口精确位置与重构影响面（file:line，实测）

- A 站内竞速 `raceManifest`（`registryrace.go:267`）唯一调用点 `gateway.go:1143`，
  深度依赖 `fetchSeq` 的 `targets/tried/saveBody/settleTarget` 局部状态（:1133-1180），
  **无法被 B 直接复用**。
- B 跨 Registry 竞速已实现：`replica.go:729-752` 构造 `[]unified.CrossCandidate` 调
  `unified.RunCrossRace`，候选对象是 Registry；`replica.go:591` 设 `unified.ChainB`。
- 缺口：`replicaTargets` 对同一副本 Registry 的多个可信 Source 只取 `picked[0]`
  （`replica.go:303-305`）；`fetchReplicaArtifact`（:388-:516）每副本只发一次
  `openUpstreamAs`。
- ⇒ skill `b-budget-claim-model.md` §B08 定性逐字一致：B08 = NOT_IMPLEMENTED
  （代码路径不存在），不是缺证据。

### 复用边界与风险
- 可复用：unified 执行层已存在（Scope/RBudget/PBudget/PTransport/Chain/Claim/
  Attempt/CrossRace），无需新建；R=16、G 公式已按 A=2 推导冻结，不需重推参数。
- 真正难点：把 A 的站内 Source 竞速从 `fetchSeq` 抽成「按 Registry 可调用」的原语，
  同时保证每个 Source Attempt 进同一 RequestScope 的 P/R 核算（不是各自独立计数）。
- 主要风险：改 `fetchSeq`/`raceManifest`/`replicaTargets` 会触及已通过的 A、B 独立路径；
  用第二部分 8 项 characterization 作硬回归门禁，任一回归即停止、回退到 56335ab。

## 五、约束验证方式（供重构阶段落地）

- C/A 边界：A<=2 复用现有 `raceEligibleTargets`（最多 2）；B C=2 由
  `crossReplicaC` 与 `RunCrossRace.FirstRound` 约束；C×A=4 由两原语嵌套自然得出。
- 两层补位：Source 失败释放 A 槽位、同 Registry 内补 Source；Registry 整体失败
  释放 B 槽位、补其他 Registry；Source 失败不得释放 B 槽位；同一 Registry 多 Source
  只算一个 B 候选（投票仍按 source_id 去重）。
- RequestScope 统一预算：断言所有 Source Attempt 后 `sc.P().Committed()` 与
  `sc.R().Used()` 在同一 Scope 上增长、不超限（P=8192、R=16）。
- Authority 唯一：断言单请求 `sc.DispatchedClaims()==1`、身份为权威逻辑 Registry。
- Digest：每个候选正文仍经 `fetchReplicaArtifact` 的 ExpectSHA / checkDigest 校验。
- CachePut：仅在请求级验证完成后由 `commitReplicaArtifact` 写唯一 CacheKey。

## 六、全量门禁与工作区

- gofmt 空、go vet RC=0、go build RC=0、go test ./... -count=1 RC=0，无 FAIL。
- 原始日志：18d-phase1-gate.log、18d-phase1-char-red.log、18d-commands.txt。
- 工作区：新增 nested_phase1_test.go 与上述证据；生产代码零改动。

## 七、18d 状态

- 第一阶段（锁测试 + RED）：完成。
- 核心重构阶段：完成（见第八节）。
- 生产/远程：未触碰。

---

## 八、18d 核心重构（按 Registry 嵌套 A 站内竞速）

### 实现（生产代码仅改 `internal/gateway/replica.go`）

1. `replicaTarget` 增加 `star`（该 Source 是否星标合格）与 `alts []replicaSourceAlt`
   （同 Registry 内可参与 A 站内竞速的其他星标 Source，最多 `crossReplicaA-1=1` 个）。
   `crossReplicaA=2` 为冻结的每 Registry Source 并发上限（§4.2）。
2. `replicaTargets` 填充 alts：只要该 Registry 的 A 站内竞速开关 `starRaceOn` 打开
   （与 B 的 race/sequential 模式无关，§4.2 两种模式内部都由 A 调度），就从该
   Registry 的星标合格 Source 中取最多 2 个；主候选优先取星标 Source。每个
   Registry 仍只 `out = append` 一次 → 仍是一个 B 候选、一个投票主体，不增加 M。
   - race 模式的资格仍是「调度器选中 ∩ 星标 ∩ 跨站参选勾选」（18b 语义不变）；
     sequential 模式不受参选勾选限制（18b 语义不变）。A 站内竞速额外要求星标。
3. `fetchReplicaArtifactCore`（由原 `fetchReplicaArtifact` 抽出）接受一个
   `replicaSampleGuard`，使嵌套竞速能抑制「因同 Registry 另一 Source 胜出而取消」
   的尝试样本（F03 排除项）。原 `fetchReplicaArtifact` 保留为薄封装，行为不变。
4. 新增 `fetchRegistryArtifact`：无 alts 时直通原路径（零行为变化）；有 alts 时
   并发启动最多 `crossReplicaA=2` 个 Source，第一个通过完整校验（code 200 且无 err）
   的正文胜出，其余取消并抑制样本、清理临时文件。所有内层 Source 都派生自调用方
   ctx（已由 `fetchWithReplicas:769` 绑定 `unified.ChainB`），因此：
   - 每个 Source 的 `openUpstreamAs → beginUnifiedAttemptAs` 都在 **ChainB** 上
     `sc.Claim(chainB, authorityIdentity)`，同链共享同一 Claim（claim.go:85-108），
     不新建 ChainA → 不与 ChainB 争用权威 Claim 而被 errUnifiedSuppressed 抑制；
   - 每个 Source Attempt 通过 `ch.RegisterAttempt` 进入同一 RequestScope 的 R/P
     核算（`openUpstreamAs:1604` 起），共享 P=8192、R=16 预算，不各自独立计数。
5. B 的两个派发路径都改调 `fetchRegistryArtifact`：
   - sequential（`replica.go:808`）：A ON + B sequential 主链并发 A=2（§4.2）；
   - race（`replica.go:840` 槽位）：A ON + B race 每 Registry 内部竞速，C×A=4。
   外层 `unified.RunCrossRace` 的投票/Authority/CachePut 完全未动。

### 关键不变量（已核对代码事实）
- 预算参数未改：R=16、C=2、A=2、M=4、P=8192、200ms 窗口、`digest_only` 均未被触碰。
- 安全门禁未动：每个 Source 仍走 `fetchReplicaArtifactCore` 的派发前仓库身份检查
  与响应后三方 Digest 校验；胜者才由 `commitReplicaArtifact` 写唯一 CacheKey。
- Authority 唯一：外层 `run(0)` 仍由 `sync.Once` 记忆化，单请求至多一次实派发。
- 两层补位：内层某 Source 失败不释放 B 槽位（仍在同 Registry 的 alts 内竞争）；
  内层全失败 → 该 Registry 整体失败 → 外层 `RunCrossRace` 补其他 Registry。
  赢家确定后取消 loser 并抑制其样本；样本抑制在取消前设置（避免 F03 竞态）。

### RED → 常驻 GREEN
- `TestRedNestedBRegistryRunsASourceRace` 改名为
  `TestNestedBRegistryRunsASourceRace`，移除 `ML_NESTED_RED` 跳过开关，常驻执行。
- 基线 56335ab：`hits2=0` FAIL（证据 `18d-phase1-char-red.log`）；
  重构后：同一断言 `hits2>=1` PASS（`18d-core-test.log` 中
  `TestNestedBRegistryRunsASourceRace ok`）——证明缺口已闭合。

### 验证结果
- `gofmt -l .` 空；`go vet ./...` RC=0；`go build ./...` RC=0（`18d-core-vet-build.log`）。
- `go test ./... -count=1` RC=0，34 包 ok、0 FAIL（`18d-core-test.log`）。
- A/B 8 项 characterization 与全部原有 `TestReplica*`/`TestCross*` 独立路径全绿 →
  **重构硬门禁通过**（无回归）。
- race 检测：定向 `18d-core-race-focus.log`（RACE_RC=0，0 DATA RACE，0 FAIL）；
  首跑曾发现一处真实 DATA RACE（`plan.Source` 写 vs loggw 读，内层 goroutine
  共享 *Plan），已用内层独立 plan 浅拷贝修复并重跑为绿；全量
  `18d-core-race-full.log`（见文件内 RACE_FULL_RC）。
- 原始证据均为本次重新运行生成，未从其他日志拼贴。

### 18d 内部专项复核（P2-3，四列核对）
逐条把冻结规则翻译为「设计已冻结 / 代码已存在 / 门禁已存在 / 测试已存在」四列，
结果 10/10 通过（`18d-core-result.json`，由脚本从 `18d-core-targeted-v.log`
的 `--- PASS:` 行与代码符号解析生成，非手填）：
- A 并发上限 A=2、同 Registry 内层嵌套竞速、复用 ChainB 无新 ChainA、
  每 Source Attempt 进同一 Claim/Scope、取消 loser 不计样本(F03)、
  抑制先于取消 + plan 隔离(无 race)、A+B sequential 内部 A=2、
  坏赢家不污染缓存、A 失败立即补位、预算 R/P/Claim 可观测——均命中真实代码
  符号且对应测试 `--- PASS`。
- 全量门禁：gofmt 空 / vet / build / test(34 包) / race(0 DATA RACE) 全绿。
- 结论：18d 核心重构无 P0/P1、无预算绕过、无安全语义冲突；各阶段证据均为
  最终代码重新运行。生产/远程仍未触碰（WAITING_FOR_USER_DECISION）。
