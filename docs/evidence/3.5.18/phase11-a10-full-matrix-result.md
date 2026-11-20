# 阶段11 A10 性能配对 — "最全"多并发档实测结果（2026-11-21）

## 本轮性质与用户指令

用户指令："以后这种测速都按最全的试"。按此要求，本轮不挑最容易通过的单一配置，
而是覆盖冻结稿的必测场景 + 多个并发档位，P50/P95/P99 全报，对每个配置如实记录结果
（含流量口径不一致的负结果），不用补充档位覆盖或抹掉最全档的结果。

## 冻结口径（未改动）

- 场景：`a10-scenario-freeze.md`（首选源 base 0ms / 20%×300ms 尾延迟，次选源稳定 20ms，
  两源返回完全相同的 8 个合法 manifest，tag t1..t8 内容计算 digest，只注入延迟不伪造内容）。
- 阈值：§12.3.0，竞速组 P95 TTFB 相对基线组改善 ≥50%，10 轮，P50/P95/P99 三维报告，
  只按 P95 判定，相邻秩线性插值，异常值不剔除。
- 流量口径：§12.5，基线与候选的官方源流量必须一致，不得因实现缺陷让候选组额外打官方源。
- 配置：`race_after_ms: 50`（`a10-config-fix-note.md`，测试前选定，非凑阈值）。
- 环境：候选镜像 `mirror-lens:3.5.18-local`，网关 + 两自建 HTTP 源（真实 manifest 内容）
  + 冷缓存（cache_entries/blobs/breaker/source_events + blob 文件）+ 每档每组重启网关清零 counter。

## 关键夹具修正（本轮之前）

前几轮 NOT_MET 中混入一个**测试夹具手抄错误**：请求 digest 手抄时把 `...c585e9856d...`
漏抄成 `...c5856d...`，导致源返回内容与请求摘要不符、竞速两路都判 body 失败→回退官方 401。
本轮改为**从源脚本动态取 digest**（`a10_full_pair.py:get_digests()`，与源内容同源），
绝不手抄。修正后验证：8/8 请求 200、正文 SHA-256 与 digest 一致、0 body failure、0 fallback，
竞速双路真实触发（STARTED=8，rank2 真实胜出）。这证明此前那部分 body 失败是夹具错误，
不是产品缺陷。

## 实测结果（三档，每档基线/竞速各 10 轮，冷缓存）

| 档位 | 组 | 样本 | 成功 | 错误率 | P50(ms) | P95(ms) | P99(ms) | 胜者分布 | P95 相对改善 |
|---|---|---|---|---|---|---|---|---|---|
| 并发8（冻结档） | 基线 OFF | 80 | 80 | 0% | 96.96 | 434.59 | 719.20 | fast 80 | — |
| 并发8（冻结档） | 竞速 ON | 80 | 60 | **25%** | 107.89 | 431.46 | 434.00 | fast 20/slow 40 | **0.7% → NOT_MET** |
| 并发3（补充档） | 基线 OFF | 30 | 30 | 0% | 39.51 | 343.07 | 347.20 | fast 30 | — |
| 并发3（补充档） | 竞速 ON | 30 | 30 | 0% | 60.39 | 342.83 | 344.40 | slow 28/fast 2 | 0.07% → NOT_MET |
| 并发1（补充档） | 基线 OFF | 10 | 10 | 0% | 20.69 | 322.88 | 325.19 | fast 10 | — |
| 并发1（补充档） | 竞速 ON | 10 | 10 | 0% | 43.14 | 71.66 | 88.31 | fast 2/slow 8 | **77.8% → MET** |

## 判定与机理（诚实、有据）

1. **冻结档并发8：NOT_MET**。竞速组 20/80（25%）返回 401 回退官方，基线组 0 错误——
   官方源流量口径不一致，违反 §12.5，不能作为正式 PASS 样本。counter 硬证据（同配置
   补跑，`a10full-conc8-race-counterverify.json`）：`started=80`、`extra_requests=6`、
   `rank1=54`、`rank2=6`、`cancelled=8`、`body_failures=0`、
   **`fallback=20`、`budget_skipped=22`**——与 20 个 401 一一对应。代码因果已核：
   `registryrace.go:41` `errRaceNoBudget` → `gateway.go:1174` `RegistryRaceFallback()` →
   顺序路径候选源同样受预算限制 → 回退官方 401。这是**并发8超过单源设计预算**
   （`max_per_source` 默认4、普通派发实际3）导致的真实预算失配，不是夹具错误、不是产品 bug。
   即便忽略口径，本次 P95 改善也不稳定（一次运行 0.7%、补跑基线 434ms vs 竞速 305ms），
   但 25% 官方回退的口径不一致是稳定复现的硬伤，判定 NOT_MET 不变。

2. **补充档并发3：NOT_MET**。0 错误、0 回退，流量口径一致，但 P95 改善仅 0.07%。
   slow 源虽赢 28/30，但并发3下其稳定 20ms 被排队放大，尾延迟请求未被有效抢先。

3. **补充档并发1：MET**。0 错误、0 回退、口径一致；基线 10 轮中 2 轮命中 300ms 尾延迟
   （319/325ms）抬高 P95 到 322ms；竞速组 slow 源稳定胜出（8/10）把这两轮压到 96/43ms，
   P95 降到 71.66ms，相对改善 77.8%（≥50%）。counter 佐证（同配置补证）：
   `started=10`、`extra_requests=1`、`rank1=9`、`rank2=1`、`cancelled=1`、
   `body_failures=0`、`fallback=0`、`budget_skipped=0`。
   这正是冻结场景"首选源尾延迟、次选源稳定"的预期机理，在无预算争抢的低并发下成立。

4. **跨档结论**：竞速机制本身有效（并发1 达标 + 全档 winner_dist 显示 slow 真实胜出），
   但冻结的并发8配置在本环境触发单源预算失配与流量口径不一致，不能宣布性能验收通过。
   按证据纪律保留全部负结果，不以并发1 的 MET 覆盖并发8 的 NOT_MET。

## 待用户裁定（未执行）

并发8 是实现冻结条件，降低并发或提高 `sources.max_per_source` 都会改变冻结条件，
本轮不擅自决定。可选方向：
- 方案A：把正式验收并发数改为 ≤3（偏离 `a10-scenario-freeze.md` 的并发8）；
- 方案B：在测试环境把 `sources.max_per_source` 提到 ≥8 后按并发8重测（改的是测试配置，
  不是产品默认；需确认不改冻结的资源预算定义）。
两者都需用户裁定后才能作为正式达标口径。

## 采集脚本缺陷披露

首轮 `a10_full_pair.py` 的 metrics 正则缺 `\s+`，把全部 counter 记为 0，且未匹配无 label
的 counter 行。TTFB、错误率、胜者分布不受影响（来自客户端实测样本）。修正正则（`\s+`
+ 无 label 分支）后对 conc=1、conc=8 竞速档分别同配置补证了准确 counter，conc=3 的
counter 因网关重启已清零无法回溯，在 JSON 中记为 `null`，不填假值，竞速发生由
`winner_dist(race-slow 胜出)` 佐证。

## 证据文件

- 编排脚本：`a10_full_pair.py`
- 原始日志：`a10full-run.log`
- 逐档样本：`a10full-conc8.json` / `a10full-conc3.json` / `a10full-conc1.json`
- counter 补证：`a10full-conc8-race-counterverify.json`（conc=8 竞速档准确 counter）
- 汇总：`a10full-summary.json`
- 夹具修正探针：`perf_race_probe.py`、`perf_setstar.py`
- 冻结口径：`a10-scenario-freeze.md`、`a10-config-fix-note.md`（未改动）
