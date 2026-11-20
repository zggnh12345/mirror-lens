# 阶段11 A10 补测（配置修正后）结果 — NOT_MET（有正面信号，口径不合规）（2026-11-21）

## 修正内容
依据 `phase11-hedge-rootcause.md` 根因与 `a10-config-fix-note.md`，
在 `ml318-config.yaml` 的 registry 段补 `race_after_ms: 50`（原缺失，默认1000ms 导致
第二路来不及启动）。场景其余参数全部沿用冻结值。修正后候选镜像重新本地构建
（commit=da8eba5，新 ID 34e76463c5fc，代码与之前相同）。

## 实测统计（并发8，每组10轮×8，冷缓存）
| 组 | 样本 | 成功 | 错误率 | P50 | P95 | P99 | 胜者(X-ML-Source) |
|---|---|---|---|---|---|---|---|
| 基线（race off） | 80 | 80 | 0% | 98.18ms | 414.60ms | 431.87ms | race-fast 80/80 |
| 竞速（race on）  | 80 | 60 | 25% | 106.69ms | 138.54ms | 215.11ms | fast 27 / slow 33 |

- P95 相对改善 = (414.60 − 138.54)/414.60 = **66.6%**（≥50% 门禁）
- 竞速触发 `registry_race_started_total=80`；`wins rank1=57, rank2=3`
  （第二路开始真正胜出，配置修正前为 rank2=0）；`extra_requests=3`、`cancelled=3`
- **但**：`registry_race_budget_skipped_total=20`，恰好等于 20 个失败样本。

## 关键问题：流量口径不一致（§12.5 明文禁止）
- 20 个失败样本均为客户端 401，网关日志 `源要求鉴权…source=quay.io status=401`，
  即回退官方源。竞速组无任何"源不可用/摘要不符"日志。
- `race_budget_skipped_total=20` 证实：并发8 请求同时竞速时，单源 Lease 预算不足，
  第二路无法形成双路，`raceManifest` 返回 errRaceNoBudget → 回退官方 401。
- 单源并发上限 `MaxPerSource` 默认 4、普通派发实际 3（20% 留给交接，
  `internal/scheduler/load_test.go:14`、`scheduler.go:32`）。并发8 超过该预算。
- 基线组 0 错误、0 官方回退；竞速组 25% 官方回退。按 1.6 §12.5，
  「不得因实现缺陷让候选组额外打到官方源」——**两组官方源流量口径不一致，
  本次样本不满足正式验收口径**。

## 判定：NOT_MET（有条件正面信号）
1. 配置修正后，竞速机制本身被证实有效：rank2 从 0 → 3 次胜出，P95 从 414ms → 138ms，
   66.6% 改善——这是配置文件缺失 `race_after_ms` 导致的假阴性，已排除。
2. 但本次 25% 请求因并发8超过单源 Lease 预算而回退官方，触发 §12.5 口径不一致，
   不能作为正式 PASS 样本。按证据纪律不隐藏错误率、不写 PASS。
3. 该预算失配是**测试环境并发设置超过单源设计预算**，不是产品代码 bug
   （默认单源并发3是既定设计）。

## 后续方向（未执行，待授权）
要得到合规正式样本，需让并发数与单源 Lease 预算匹配：
- 方案A：降低测试并发到 ≤3（偏离冻结方案 §4 的并发8）；
- 方案B：提高测试环境 `MaxPerSource` 配置（需确认该值是否可配置）后才并发8。
两者都改变冻结条件，需用户裁定；本轮不擅自重测。

## 证据文件
- 配置修正：`a10-config-fix-note.md`、`ml318-config.yaml`（race_after_ms: 50）
- 根因：`phase11-hedge-rootcause.md`
- 原始样本：`baseline8-fix.json` / `race8-fix.json`
- 日志：`baseline8-fix.log` / `race8-fix.log`、`rebuild-3.5.18.log`
