# 3.5.25 发布就绪最终裁定（RELEASE_READY）

> 本文是 3.5.25 发布状态机的唯一最终裁定。`g7-release-readiness.md` / `lock-release.md` / `g7-archive-index.md` 为各阶段时点快照（保留原貌，含补验前的 BLOCKED/PARTIAL 措辞），以本文结论为准。审查方式：在「即将交付的最终 HEAD」上现场重跑门禁，不沿用门禁日志锚定的旧代码 SHA；逐条核对证据路径与 SHA 有效性。

## 版本锚点（全部经 `git cat-file -t` 校验为有效对象）

| 项 | SHA / 状态 |
|---|---|
| 基线（3.5.24） | `84b013d23f0c11eaf86de9d5e776789467cb4192` |
| 任务书 commit | `5020ec557300a0dbdc4e6a1a2834d2b938da0f90` |
| 任务书 blob | `02bff3b3f4851be612cf9f883bbf616da3a29b6a` |
| G0/G1 冻结 | `74ef211170e1e9e8dcaac235750b6d3de2feae47` |
| G4 门禁锚定代码 | `0caf9da177e0cf9988ebcd0f10f43c5fc2847802` |
| 最终 HEAD（本裁定所在提交） | `git rev-parse HEAD` |
| 工作树 | CLEAN |
| 推送 / 合并 / 部署 / 发布 | 均 NOT_DONE / NOT_AUTHORIZED（§19.2） |

## 第 21 节 26 项总门禁逐条核对

| # | 门禁 | 结论 | 证据路径 |
|---|---|---|---|
| 1 | 3.5.24 基线 commit SHA 核实归档 | PASS | `baseline-evidence.md`、`final-sha-reconciliation.md` |
| 2 | G0 代码取证完整 | PASS | `execution-state.md` G0 段、任务书 §0/§4 |
| 3 | 兼容参数和过渡态普查闭合 | PASS | `compatibility-inventory.md`、`transitional-state-inventory.md` |
| 4 | D1–D5 决策全部明确 | PASS | `decisions.md` |
| 5 | G1 设计冻结完成 | PASS | `design-freeze.md` |
| 6 | F1 已完成 | PASS | `execution-state.md` Finding 闭环、`findings.md` F-REV-01 |
| 7 | F2 已完成 | PASS | `findings.md` F-REV-02 |
| 8 | F3 已完成 | PASS | `findings.md` F-REV-03 |
| 9 | F4 已完成 | PASS | `findings.md` F-REV-04 |
| 10 | F1b 已完成 | PASS | `findings.md` F-REV-05 |
| 11 | Host 配置与运行时能力单一事实源 | PASS | `host-source-matrix.md`、`transaction-boundary-audit.md` |
| 12 | 不存在旧 API 兼容路径 | PASS | `compatibility-inventory.md`；现场 grep 旧字段残留为 0 |
| 13 | 不存在旧配置/环境变量 fallback | PASS | `compatibility-inventory.md`；现场 grep `ML_HOSTPROBE_*` 生产代码为 0 |
| 14 | 不存在双轨执行和双写 | PASS | `transaction-boundary-audit.md` |
| 15 | 新增/编辑/探测写回满足事务边界 | PASS | `transaction-boundary-audit.md` |
| 16 | TLS 错误底层到 API/UI 完整保真 | PASS | `tls-root-cause-investigation.md`、`findings.md` F-REV-02/03、`g5-real-acceptance.md` |
| 17 | SparkCR 真实验收通过 | PASS（口径见下） | `g5-real-acceptance.md`、`g5-three-rounds.json`、`g5-api-3.5.25-sources.json` |
| 18 | SUNBALCONY 真实验收通过 | PASS | `g5-real-acceptance.md`、`g5-fakeip-control-full.json` |
| 19 | 独立复核通过且无未关闭 P0/P1 | PASS | `findings.md`（5 Finding 全 CLOSED，delegate 复核链） |
| 20 | `go build ./...` | PASS | 最终 HEAD 现场重跑，RC=0 |
| 21 | `go vet ./...` | PASS | 最终 HEAD 现场重跑，RC=0 |
| 22 | `go test ./...` | PASS | 最终 HEAD 现场重跑，全包 ok |
| 23 | `go test -race ./...` | PASS | 最终 HEAD 现场重跑，RC=0 |
| 24 | 性能对照完成且无未解释退化 | PASS | `g6-real/result.json`（8 门禁全过）、`g6-comparison.md`、`g6-methodology.md` |
| 25 | 文档与证据归档完整 | PASS | 本目录全部证据文件 |
| 26 | 工作树干净、提交可追溯 | PASS | `git status --porcelain` 为空；提交链可追溯 |

## 最终 HEAD 现场重跑记录（发布必做）

在最终 HEAD 的干净导出上（golang:1.27.3 容器，tar 传源码，非 bind mount）重跑四条门禁 + diff-check：

- `gofmt -l .` → **空**（修复前 `internal/store/g6_bench_test.go` 未格式化，见下方「发布前已修复项」）
- `go build ./...` → RC=0
- `go vet ./...` → RC=0
- `go test ./... -count=1` → 全包 ok
- `go test -race ./... -count=1` → RC=0
- `git diff --check` → 无输出

## 发布前已修复项（本次发布就绪审查发现并闭合）

1. **gofmt 阻塞（P0）**：G6 阶段引入的 `internal/store/g6_bench_test.go` 手写了单行 `if err != nil { b.Fatal(err) }`，未按 gofmt 格式化。g4 门禁日志锚定 `0caf9da`，该文件在后续 `b27d83e` 提交引入，之后未重跑 gofmt。修复：容器内 `gofmt -w` 展开为标准多行，语义不变；最终 HEAD 现场重跑四门禁全绿。
2. **无效 SHA（P1）**：`final-sha-reconciliation.md` 的 G4 代码 SHA 原写 `0caf9da...fc224`（`git cat-file -t` 报 Not a valid object），更正为有效对象 `0caf9da...fc2847802`。
3. **跨文档结论矛盾（P1）**：`g7-release-readiness.md` / `lock-release.md` 保留补验前 NOT READY / BLOCKED_REAL_ENV 时点快照，已在顶部加「补验后状态更新」块指向本文与 g5/g6 补验证据，不回改历史结论。
4. **findings.md 重复粘贴（P2）**：4 条 Finding 的 CLOSED 链重复 4 次，收敛为单次。

## SparkCR 口径说明（唯一带弹性的门禁）

`g5-three-rounds.json` 显示 SparkCR 三轮均为 `degraded`（"探测失败，未达判死阈值"），非 `usable`。本裁定判 PASS 的依据是任务书 §9.6 的通过条件第二条「源站自身存在证书或网络问题时，UI 准确报告唯一根因」：

- TLS 健康（`tls_ok=true, cert_days=79`），非证书误报；
- `g5-api-3.5.25-sources.json` 中 `last.docker.error` 精确记录 blob CDN URL、`last.github.error` 精确记录 404 —— UI 后端数据源准确报告唯一根因；
- degraded 链路本身是 3.5.25 错误保真修复的正确行为（不再误报 TLS_HANDSHAKE_FAILED）。

**2026-11-27 实测对照（用户要求「去试试」后，只读 GET，结果可复现）** —— 用完全相同的探测格式 `/https://raw.githubusercontent.com/octocat/Hello-World/762941318ee16e59dabbacb1b4049eec22f0d303/README`：

| 目标 | 结果 | 定性 |
|---|---|---|
| 直连 raw.githubusercontent.com | 200，13 字节 | 探测样本本身有效 |
| ghfast.top | 200，13 字节 `Hello World!` | 同格式在同类站可用 |
| gh-proxy.com | 200，13 字节 `Hello World!` | 同格式在同类站可用 |
| ghproxy.net | 200，13 字节 `Hello World!` | 同格式在同类站可用 |
| SparkCR（docker.sparkcr.cn） | 307 把 `//` 折成 `/` → 404 `page not found` | 源站自身不提供该路由 |

SparkCR 四种写法（`/https://...`、`/https:/...`、`/http/https://...`、裸路径）全部 404 `page not found`（Go `http.NotFound`），证明它压根没有 GitHub raw 代理路由，非格式兼容问题。生产数据中 SparkCR `github_cap=0`，而上述三站 `github_cap=1` 且探测成功——系统对 SparkCR 的能力判定本就正确。

docker blob 失败的真实原因亦经实测纠正：非「CDN 404」，而是 SparkCR 把 blob 重定向到的第三方 CDN `cache-esa.cr.tongritech.com` **TLS 证书已过期**（curl `certificate has expired`）。属该 CDN 运营问题。

两项均为源站自身问题，与 3.5.25 代码无关：`git diff 84b013d..HEAD -- internal/probe/{github,docker}.go` 显示 URL 拼法一字未改，3.5.25 仅改「错误保真」（`if res.Error == ""` 保护、TLS 错误不被后续 HTTP 错误覆盖）。`/<完整 URL>` 探测格式是本项目加速器协议约定（`docs/cache-regression-matrix.md`、`docs/accelerator.md`），在三个独立 GitHub 代理站上实测 200，格式无误。

若产品语义要求 SparkCR 必须达到 `usable` 才算「真实验收通过」，则本项应回退为 FAIL 并重新定级。**用户已于 2026-11-27 确认采用标准 A（按错误保真判定），本项定为 PASS**，不影响其余 25 项判定。

## 结论

**`RELEASE_READY = true`**（SparkCR 口径按上节理解）。

- 交付物：3.5.25 修复版本，从基线取证、真实验收、性能对照到证据归档均可独立复现。
- 边界：本裁定只表示「代码与证据达到发布条件」，**不等于授权推送 / 合并 / 打 tag / 建 Release / 切换生产**。这些是独立授权事项；当前生产仍为 3.5.24，未触碰。
