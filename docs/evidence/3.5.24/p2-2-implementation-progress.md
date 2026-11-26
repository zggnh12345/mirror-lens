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
- 过渡期回落：`clientFor` 未钉住分支不再复用裸 `privClient`，改用继承本机 Guard 的 SafeHTTP client（`g.fallbackClient()`，拨号层逐 IP 检查 + 每跳重定向检查）。
- 测试迁移：`pin_test.go` 三个连接语义用例改用 `pinIPChecker` 测试缝注入（生产 checker 不变，t.Cleanup 恢复），RED 的 `127.0.0.1` pin 拒绝用例保留严格路径。

### probe 语义保持（`internal/probe/probe.go`）
- `Configure(allowPrivate)` 原本语义即"允许探测内网/回环"，其 `AllowPrivate=true` 在旧 CheckIP 里会放行 loopback。收窄后同步设置 `AllowLoopback=allowPrivate` 以保持既有语义，避免测试桩/自建局域网源被误伤。生产默认 `allowPrivate=false` 不受影响。
- **裁定点**：这是对上层 API 的语义映射，非对 safehttp Guard 层裁定的修改；如产品要求 `Configure(true)` 仍拒回环，需单独改判（会影响 probe/runner 的本地探测能力）。

## 门禁结果
- `go build ./...`：PASS（日志 `gate-p2-2-build-vet.log`，exit 0）
- `go vet ./...`：PASS（同上）
- `go test ./internal/safehttp`：PASS（0.004s）
- `go test ./internal/api`：PASS（10.5s）
- `go test -race ./internal/safehttp ./internal/api(Pin|Bogus) ./internal/gateway(Pin|Redirect)`：PASS
- Pin 相关 RED + 迁移用例：全部 PASS（PIN-10/14/15 及 PIN-02..08 创建阶段）
- `go test ./... -count=1`：34 个包 OK，1 个包（gateway）34 个测试失败

## 未验证 / 已知 BLOCKED（不得据此标 RELEASE_READY）
- **gateway 34 个失败**：全部为 `逻辑 Registry Host "127.0.0.1:port" 无法规范化：不允许带端口` 及同类的 URL 规范化错误，属 `e8a10e1` 基准即存在的失败（git stash 对比确认：基准 37 失败、20 个"不允许带端口"，本阶段无新增此类回归）。与 P2-2 安全改动无关，但整包未全绿。
- **FakeIP 独立信任域**：NOT_IMPLEMENTED（discover 专用 Guard、Canary 三态、专用 Resolver、viaDomain 拨号决策）。
- **DNS Rebinding / Redirect 完整 Origin 的五组边界（BND）**：仅完成了 Pin 侧 Redirect，其余未测。
- **wiring（gateway/probe/pin 不接 FakeIP Resolver）**：待 FakeIP 实施后统一验证。
- **全量 `go test -race ./...`**：NOT_RUN（仅跑了关键包）。
- **独立复核**：NOT_RUN（须基于最终提交由复核 Agent 独立检查，实施 Agent 不自签 PASS）。
- **P1-2 / P2-5 / P2-6**：NOT_STARTED（用户要求 P2-2 完成后再进入）。

## 时间序列基线证据
- 基准（e8a10e1，stash 全部改动）：全仓 43 个 FAIL，含 safehttp/api 的 RED 失败 + gateway 的 URL 规范化失败；probe/runner 单独跑 OK。
- 本阶段：safehttp/api 的 RED 转 GREEN；gateway 仍 34 个 URL 规范化失败（基准同源）；probe/runner 经语义映射保持 OK。
- 原始日志：`docs/evidence/3.5.24/gate-p2-2-fulltest.log`、`gate-p2-2-build-vet.log`、`gate-p2-2-race-*.log`。

## 结论
P2-2 的 **AllowPrivate 收窄 + Pin 三层严格 + 兼容性迁移** 子部分已实施并通过定向门禁；P2-2 整体因 FakeIP/Canary/BND/wiring 未完成、gateway 基准失败未消除、独立复核未跑，**保持 NOT_CLOSED**。不标 RELEASE_READY，不进入 P1-2，等待后续授权与推进。
