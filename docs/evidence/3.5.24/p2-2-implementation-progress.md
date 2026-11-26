# 3.5.24 P2-2 阶段实施记录（AllowPrivate 收窄 + Pin 三层严格）

- 分支：`3.5.24-impl`（worktree `/opt/data/cache/scratch/review-wt`）
- 基准提交：`e8a10e1`（P2-2 RED 规格、兼容性基线、失败用例）
- 本阶段提交范围：8 个代码文件 + 5 个门禁日志
- 生产授权：`PRODUCTION_DEPLOYMENT=NOT_AUTHORIZED`；未推送、未合并 main、未发布、未部署

## 已完成并验证（PASS）

### safehttp（`internal/safehttp/safehttp.go`）
- `AllowPrivate` 收窄为只放行 RFC1918 / IPv6 ULA，不再提前 `return nil` 越权放行回环/特殊用途段（修复 `safehttp.go:112-114` 缺陷）。
- 回环由 `AllowLoopback` 独立控制；CGNAT、FakeIP(198.18/15)、文档段、基准测试段、链路本地、组播、IANA 特殊段一律不由 `AllowPrivate` 放行。
- 按 IANA 特殊用途登记表系统补齐 `extra` 清单（含 `100::/64`、`2001::/23`、`3fff::/20`、`5f00::/16`、`64:ff9b:1::/48` 等）。
- 新增严格 API：`CheckIPStrict` / `CheckStrict` / `StrictDialer` / `StrictClient` / `OrderIPs`；严格路径忽略 AllowPrivate/AllowLoopback/FakeIP。
- 兼容迁移：`family_test.go` 的 localhost 用例 `AllowPrivate` → `AllowLoopback`；`safehttp_test.go` 的 `198.18.0.4`/`198.19.255.255` 由放行改为默认拒绝。

### Pin（gateway + api）
- 创建阶段 `internal/api/relay_pin.go`：`bogusPinAddr`、`checkPinList`、`resolveRelayHost` 统一改走 `CheckIPStrict` 单一事实源，消除"创建放行/拨号拒绝"半开状态。
- 拨号阶段 `internal/gateway/pin.go`：每个候选 IP `ParseAddr` + `CheckIPStrict`；`net.Dialer.Control` 在 TCP 建连前复查实际 IP（DNS Rebinding 闭合）；无合法候选返回 `ErrBlocked`。
- Redirect：`sameOrigin` 用完整 Origin（Scheme + 规范化 Host + EffectivePort），任一变化在第二次拨号前返回 `ErrBlocked`；超 5 次仍返回次数错误。
- 过渡期回落（F2 修复）：`clientFor` 未钉住分支的 `fallbackClient()` 显式使用 `AllowPrivate=false`、`AllowLoopback=false` 的严格策略（`safehttp.Default().StrictClient()`），**不继承 `g.guard` 的放宽权限**。回落路径无其他绕过严格拨号检查的分支。
- 测试迁移：`pin_test.go` 三个连接语义用例改用 `pinIPChecker` 测试缝注入（生产 checker 不变，t.Cleanup 恢复）；需要连本机回环桩的测试改用测试专用 `setFallbackClientForTest` 注入（生产 `fallbackClient` 仍严格）。

### probe 权限隔离（F1 修复，`internal/probe/probe.go`）
- **原错误裁定（已作废）**：此前把 `Configure(true)` 同步设置 `AllowLoopback=true` 记为"语义映射、非缺陷"。该判断错误——它让 `AllowPrivate` 隐式开启回环访问，与冻结规格"AllowPrivate 与 AllowLoopback 完全独立"冲突。
- **修正**：`Configure(allowPrivate, ...)` 不再隐式开启回环；`AllowPrivate=true` 时 `127.0.0.1`、`::1` 仍拒绝。生产调用路径不通过任何参数/默认值放开回环。
- 需要回环的测试改用显式测试专用 Guard/构造，不新增用户可见配置项。
- RED 测试：`AllowPrivate=true` 时 `127.0.0.1`、`::1` 必须拒绝（修复前失败、修复后通过）。

### Origin 按请求隔离（F3 修复，`internal/gateway/pin.go`）
- 删除跨请求共享的 `origin atomic.Value`；每次 Redirect 用当前请求链的 `via[0].URL` 作为 Origin 基准。
- 不使用客户端实例级状态保存某次请求的 Origin；同一 Client 连续处理两个不同初始端口请求时判断互不影响。
- 保留完整 Origin 比较：Scheme、Hostname、EffectivePort。
- RED 回归测试：同一个 Client 连续两个不同初始端口请求的 Redirect 判断互不影响（修复前失败、修复后通过）。

## 本轮 F1/F2/F3 修复门禁（叠加于 371cdbf）
- `go build ./...`：PASS（exit 0）
- `go vet ./...`：PASS（exit 0）
- `go test -race ./internal/probe ./internal/api ./internal/runner`：全部 PASS，无 DATA RACE
- `gofmt -l`（改动文件）：无输出，PASS
- RED→修复→GREEN：F1/F2/F3 三个 RED 修复前失败、修复后通过；未通过只改测试使其通过

## 34 个 gateway 基准失败的根因修复（2026-11-26）
- **根因**：测试夹具用本地 httptest 地址（`127.0.0.1:port`）作 Registry 逻辑身份，
  而 `registryhost.Normalize` 依冻结契约拒绝 IP+端口（`ErrBadPath`）。
  与 P2-2 安全改动无关，属 `e8a10e1` 基准即存在的失败。
- **修复（RED→GREEN）**：新增 `internal/adapter/registryadapter/registry_canonical_key_test.go:
  TestLocalUpstreamWithCacheOrigin`——修复前因字段不存在编译失败（RED），
  修复后规划成功、回源 host 保持本地地址、cache-key 用规范域名身份（GREEN）。
- **实现**：`Adapter.CacheOrigin` 测试专用字段（`registry.go`），非空时只替代 cache-key
  的规范化身份；回源 host / foreign 判定 / 调度身份全部保持原值。生产零值 = 冻结原语义，
  不新增用户可见配置项，不改拨号/安全链。多源夹具 `moFixture.get` 每请求把 CacheOrigin
  同步为当前逻辑 Host `f.host`，避免 8 个 Host 共用一个 cache-key 互相吃缓存。
- **差分（旧基准日志 `gateway-current2.log` vs 修复后 `gw-after2.log`）**：
  旧基准 34 个失败；修复后 0 个；`新增失败集合 = []`（无新增），`已消除 = 34`。
  34 个失败名清单见 `docs/evidence/3.5.24/gateway-baseline-diff.json`。
- `go test ./internal/gateway -count=1`：PASS（0 失败，112.9s）
- `go test ./... -count=1`：PASS（35 个包 OK，0 失败）
- `go test -race ./... -count=1`：PASS（35 包 OK，0 个 DATA RACE，见 `race-all-final.log`；修复过程中曾暴露 3 个由夹具并发写 CacheOrigin 引起的竞态，已改为构造期 setHost setter 消除）

## 门禁结果（上一阶段）
- `go build ./...`：PASS（日志 `gate-p2-2-build-vet.log`，exit 0）
- `go vet ./...`：PASS（同上）
- `go test ./internal/safehttp`：PASS（0.004s）
- `go test ./internal/api`：PASS（10.5s）
- `go test -race ./internal/safehttp ./internal/api(Pin|Bogus) ./internal/gateway(Pin|Redirect)`：PASS
- Pin 相关 RED + 迁移用例：全部 PASS（PIN-10/14/15 及 PIN-02..08 创建阶段）
- `go test ./... -count=1`：34 个包 OK，1 个包（gateway）34 个测试失败

## 未验证 / 已知 BLOCKED（不得据此标 RELEASE_READY）
- **gateway 34 个失败：已修复（2026-11-26）**。根因为测试夹具本地地址作逻辑身份、
  经 `CacheOrigin` 测试缝修复；差分 `新增失败集合=[]`、`已消除=34`，
  `go test ./internal/gateway -count=1` 与 `go test ./... -count=1` 均 PASS。
  详见上节与 `gateway-baseline-diff.json`；原始日志 `gw-after.log`/`gw-after2.log`/`test-all-after.log`。
- **FakeIP 独立信任域**：NOT_IMPLEMENTED（discover 专用 Guard、Canary 三态、专用 Resolver、viaDomain 拨号决策）。
- **DNS Rebinding / Redirect 完整 Origin 的五组边界（BND）**：仅完成了 Pin 侧 Redirect，其余未测。
- **wiring（gateway/probe/pin 不接 FakeIP Resolver）**：待 FakeIP 实施后统一验证。
- **全量 `go test -race ./...`**：PASS（35 包 OK，0 DATA RACE）。
- **独立复核**：**PASS**。新独立复核 Agent（deleg_daa74ffa）对最终 HEAD `403ddf692cbb59b74a07e870f463f184557d58cd` 只读复核，7 项检查全 PASS，`new_failures=[]`，`data_races=0`，实施者不自签；结果原文见 `review-f1f2f3-final.json`。三个前期 Agent 的独立复核结果见 `review-agents-prefix.json`。
- **P1-2 / P2-5 / P2-6**：NOT_STARTED（用户要求 P2-2 完成后再进入）。

## 时间序列基线证据
- 基准（e8a10e1，stash 全部改动）：全仓 43 个 FAIL，含 safehttp/api 的 RED 失败 + gateway 的 URL 规范化失败；probe/runner 单独跑 OK。
- 本阶段：safehttp/api 的 RED 转 GREEN；gateway 仍 34 个 URL 规范化失败（基准同源）；probe/runner 经语义映射保持 OK。
- 原始日志：`docs/evidence/3.5.24/gate-p2-2-fulltest.log`、`gate-p2-2-build-vet.log`、`gate-p2-2-race-*.log`。

## 结论
P2-2 的 **AllowPrivate 收窄 + Pin 三层严格 + 兼容性迁移** 子部分已实施并通过定向门禁与全量门禁（gateway 34 基准失败已消除、新增失败集合为空、全量 build/vet/test/race 通过）；独立复核已跑且 PASS。但 P2-2 整体因 FakeIP/Canary/BND/wiring 未完成，**保持 NOT_CLOSED**。不标 RELEASE_READY，不进入 P1-2，等待后续授权与推进。
