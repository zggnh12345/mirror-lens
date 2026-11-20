# 阶段11 A10 竞速 hedge 失效 — 只读根因排查（2026-11-20）

结论先行：**这是测试配置缺陷，不是竞速代码 bug。** 补测场景用 300ms 首路尾部延迟，
去测一个默认 1000ms 耐心预算才启动第二路的竞速机制，第二路永远来不及启动。

## 现象（补测数据）
- 竞速组 `registry_race_started_total=78`，但 `registry_race_wins_total{rank=2}=0`
- 竞速组 P95 391ms ≈ 基线 396ms（改善 1.2%），错误率 22.5%（回退官方 401）

## 根因证据链（代码逐行）
1. `internal/gateway/registryrace.go:391` — `raceManifest` 只先 `launch(0)` 启动**第一路**；
   第二路不是并发立即启动，而是靠两条路径补发：
   - `:424,:469` 第一路失败/非200 → 立刻补第二路；
   - `:474-480` `hedgeC` 定时器触发 → 补第二路。
2. `registryrace.go:243-251` `raceAfter()`：返回 `g.race.After`，若为0则 `g.hedge.After`，
   再为0则 **1 秒**。
3. `gateway.go:236-237`：`Hedge.After<=0` 时默认 **1 秒**（`Hedge.Grace` 默认 300ms）。
4. `main.go:512-514`：`Race.After = cfg.Registry.RaceAfterMS × ms`；
   `config.go:118-121` 字段 `race_after_ms`（文档名 `registry.race_after_ms`），
   **默认 0 = 复用 hedge.after_ms（1000ms）**——见 `config.go:442` 默认 Hedge.AfterMS=1000。
5. 本轮测试配置 `docs/evidence/3.5.18/ml318-config.yaml` **未设置 `race_after_ms`**，
   故第二路需等 1000ms 才启动；而首路尾部延迟仅 300ms，首路在 300ms 就返回，
   定时器（1000ms）根本没到 → 第二路从未启动 → rank2 wins=0。
6. 对照：单元测试 `internal/gateway/registry_race_test.go:41` 显式设
   `f.gw.race.After = 10 * time.Millisecond`，所以测试里第二路 10ms 启动、能正常竞速胜出。
   代码本身在有合理 `race.After` 时工作正常——测试全绿靠的就是这个夹具参数。

## 22.5% 错误（401）的成因
首路命中 300ms 尾部时，因 race.After=1000ms 第二路未启动；首路最终返回后若判失败，
补发第二路已晚，两路尝试耗尽后回退官方源（quay.io 匿名 → 401）。
这是配置缺陷的连带表现，不是独立的回退逻辑 bug。

## 指标口径说明（已知，非新 bug）
`registryrace.go:396` 在第一路 launch 成功时就记 `RegistryRaceStarted`，
所以该指标衡量的是"进入竞速路径"，不是"真正形成双路并发竞速"。
`started=78` 与 `rank2 wins=0` 并存正是这一口径差异，1.6 §12.3.1 已明确
「Race 触发率不是性能收益指标」。

## 澄清：200ms 投票窗口 ≠ race.After
冻结稿 §464/§532/§545 的 200ms 是 Digest 投票窗口（18c/18d 跨 Registry 一致性），
与 A 竞速第二路启动延迟 `race_after_ms` 是两个不同参数，不可混用。

## 建议的修复方向（未执行，待授权）
补测只需在 config 的 registry 段加 `race_after_ms: 10`（或 50，需 ≤ 首路尾延迟 300ms 才能
让第二路在尾部命中时抢先），重跑同一冻结场景。属测试配置修正，不改产品代码。
但按"补测只跑一次、不调参凑阈值"的既定纪律，本轮不再重跑；此方向留待用户决定。

## 排查性质
全程只读（read_file / grep / 已有日志与 JSON），未修改任何代码、配置或生产；
未新建/重建容器；未打 tag / push / Release / GHCR；未碰生产。
