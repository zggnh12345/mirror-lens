# 阶段11 A10 性能配对实测记录（2026-11-20）

## 环境（同环境配对，§12.2）
- 网关：候选镜像 mirror-lens:3.5.18-local（commit da8eba5），网络 ml318-perf
- 源：race-fast 固定延迟 0ms、race-slow 固定延迟 80ms；两者返回同一合法 manifest，
  digest `sha256:c523440f7149bf332dcb740c52b0aadd78c585e9856d74a72025e0fb98264bc2`（实测一致）
- 资格链：两源 caps=Supported/native、recovery=ok、usafe/preferred，host=quay.io 加入 multi_origin_hosts
- 开关：基线组 `registry_star_race:quay.io` 关闭；竞速组开启；B 跨站关闭（保证走 A 竞速主路径）
- 每轮清冷缓存（cache_entries/blobs + blob 文件），每组 10 轮

## 原始样本
- 基线组：`baseline.json` / `a10-baseline.log`（10/10 成功，全部 src=race-fast）
- 竞速组：`race.json` / `a10-race.log`（10/10 成功，全部 src=race-fast）
- 竞速触发指标：`registry_race_started_total{layer=same_host,host=quay.io}=10`，
  `registry_race_wins_total{rank=1}=10`、`{rank=2}=0`

## 统计（相邻秩线性插值，§12.3.0）
| 组 | P50 TTFB | P95 TTFB | P99 TTFB | 胜者 |
|---|---|---|---|---|
| 基线（race off） | 20.172ms | 20.941ms | 20.980ms | race-fast 10/10 |
| 竞速（race on）  | 20.650ms | 21.941ms | 21.999ms | race-fast 10/10 |

P95 相对改善 = (20.941 − 21.941) / 20.941 = **−4.8%（恶化，未达 ≥50% 门禁）**

## 结论与偏离说明（如实，不伪造）
1. 竞速确实按预期触发（10/10 started），但第二路从未胜出（rank2 wins=0）——
   这与 1.6 §12.3.1「Race 触发率高不等于加速有效；第二路经常启动但第一路最终胜出」
   的警告完全吻合，是真实负结果，不是执行失败。
2. 根因：race-fast 固定 0ms 且稳定最快，顺序模式总是先命中它；竞速只是多并发一路，
   带来少量额外开销，故 P95 略升。固定快慢源不产生竞速收益。
3. 需标注的偏离（相对冻结方案 test-plan-phase10-11.md）：
   - 并发数=1（方案 §4「建议 8」）——未达建议并发。
   - 1.6 §12.4 必测场景仅覆盖「第一候选正常/第二候选较慢」，未覆盖
     「第一候选延迟/第二候选较快」「第一候选快速失败」等能体现收益的场景。
4. 因此本组数据**不能判为 PASS**（P95 未改善），也不足以完整否定 A10；
   按证据纪律标记为 **NOT_MET / 场景覆盖与并发数偏离冻结方案**。

## 待用户裁定的新决策
要得到能检验 A10 ≥50% 门禁的正式数据，需构造「排序第一的源存在尾部延迟、
第二源稳定」的非理想场景（如第一源 20% 请求注入 300ms 尾延迟，第二源稳定 20ms），
并把并发数改为方案建议的 8。该场景参数会直接决定 PASS/FAIL，属未裁定的新决策，
不单方拍板，避免为凑阈值调参（p-hacking）。等待用户决定方向。
