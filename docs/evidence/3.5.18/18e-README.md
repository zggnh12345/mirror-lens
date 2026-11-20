# 18e：安全、Authority、Digest 与预算闭环

版本 3.5.18 / 分支 feat/3.5.18 / 阶段 18e

## 目标与范围（冻结稿 §18e，行 615-621）
保证 A+B 嵌套调度不破坏 1.6 安全和预算边界。18e 的绝大多数安全/预算基础设施
（Authority 唯一 Attempt、Digest 三方校验、Tag Authority 比对、CachePut 请求级
提交、Redirect 重新门控、R/P/G 预算、私有认证 fail-closed）在 3.5.17 已落地并
验收；18e 重点是复核它们未被 18d 嵌套改动破坏，并补齐嵌套特有的不变量。

## 18d 嵌套改动对安全面的影响面（实测）
18d 核心重构只改 `internal/gateway/replica.go`，未触碰：
- 投票状态机 `internal/unified/crossrace.go`（CloseWindow/Offer/quorumWinner）；
- Claim/Chain 生命周期 `internal/unified/claim.go`/`chain.go`/`scope.go`；
- 预算常量与账本 `internal/unified/budget.go`（RMax=16/PMax=8192）；
- Authority/Digest/Tag/Redirect/CachePut 的所有判定代码。

因此这些既有安全测试仍是 18e 的回归门禁（均存在且 PASS，见 `18e-test.log`）：
- Authority：`TestB02VoteSingleCandidateNeedsAuthorityCompare`、
  `TestCrossRaceMismatchFillThenAuthorityOnce`、`TestT63AuthoritySingleAttempt`、
  `TestReplicaAuthorityNotInFirstRound`、`TestReplicaAuthorityNotCountedInM`。
- Digest/Tag：`TestReplicaTagRequiresAuthorityAgreement`、
  `TestReplicaDigestSkipsAuthority`、`TestB18DegradedStillEnforcesDigestGate`、
  `TestCharB_WrongDigestReplicaNotCommitted`、`TestDigestMismatchEverywhereNotCached`。
- Claim/投票主体去重：`TestCharB_SingleRequestClaim`、`TestReplicaCandidatesShareSingleClaim`、
  `TestT51SameChainSharesClaim`、`TestUnifiedSharedClaimSameChain`。
- 预算：`TestB06BudgetOutranksEverything`、`TestT60PBudgetNeverExceeds`、
  `TestPExhaustNoCachePutAfter`、`TestRegistryBudgetExhaustedIs503`、
  `TestCharHelper_ScopeBudgetObservable`。
- 私有认证 fail-closed：`TestB19PrivateAuthorityNoCandidatesAndHostScopedCreds`、
  `TestPrivateAuthNoReauthAfterCrossHostRedirect`。
- Redirect 重新门控：`TestRecheckRedirectPolicySameHost`、
  `TestRecheckRedirectPolicyCrossHostSkips`、`TestApplyRedirectPolicyCDNRejectsUnsafe`。
- CachePut 边界：`TestPExhaustNoCachePutAfter`、`TestBogusUpstreamDigestNotCached`。

## 18e 嵌套专项新增断言
在 `TestNestedBRegistryRunsASourceRace` 加入可直接观测的不变量：
1. `R.Max == 16`：确认冻结预算常量未被 18d 改动。
2. `R.Used <= R.Max`：嵌套后预算不超额（RequestScope 统一账本）。
3. `replicaTargets("quay.io")` 只产出 **1 个** B 候选，且携带 `alts`：证明同一
   Registry 的两个 Source 只算一个候选/一个投票主体，不增加 M/票数。

诚实性说明（不做超出证据的断言）：
- 该单元测试的 plan 不携带完整 manifest 身份，`beginUnifiedAttemptAs` 会跳过
  Claim，因此实测 `R.Used=0`、`DispatchedClaims=0`（已用 t.Logf 记录）。
  故本测试**不**断言固定票数，也不声称已验证"每个 Source Attempt 实际计入
  R"——这一点只能由带完整身份的集成路径证明，属 18g 真实环境验收范围。
- 18e 在此阶段能证明的是：预算常量未改、候选层一 Registry 一候选、既有全套
  安全回归未破坏。完整预算执行证明见 18g。

## 验证结果
- `gofmt -l .` 空；`go vet ./...` RC=0；`go build ./...` RC=0。
- `go test ./... -count=1`：`18e-test.log` 末尾 `TEST_RC=0`（34 包 ok）。
- `CGO_ENABLED=1 go test -race ./... -count=1`：`18e-race.log` 末尾 `RACE_RC=0`，
  34 包 ok，0 DATA RACE，0 FAIL。
- 18d 遗留的 `plan.Source` 写/loggw 读 DATA RACE 已在 18d 核心阶段用内层
  plan 浅拷贝修复；18e 全量 race 复跑确认不再出现。

## 状态
- 18e 安全/预算回归：通过（无新增 P0/P1，嵌套未破坏既有安全门禁）。
- 生产/远程：未触碰（WAITING_FOR_USER_DECISION）。
