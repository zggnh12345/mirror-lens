# 阶段11 A10 补测 — 测试配置修正说明（2026-11-20）

## 修正性质
根因排查（`phase11-hedge-rootcause.md`）已证实：上一轮 NOT_MET 是**测试配置缺陷**——
`ml318-config.yaml` 缺少 `registry.race_after_ms`，导致第二路用默认 1000ms 耐心预算，
而首路尾部延迟仅 300ms，第二路来不及启动（rank2 wins=0）。这不是产品代码 bug，
也不是阈值/场景参数调整。本次仅补齐该缺失配置项，使测试真正测到 A 竞速的 hedge 机制。

## 修正内容
- `ml318-config.yaml` 的 `registry:` 段新增 `race_after_ms: 50`（单位毫秒）。
- 场景其余参数全部沿用冻结值（`a10-scenario-freeze.md`）：
  首路 base 0ms / 20%×300ms 尾部、次路稳定 20ms、8 个 digest、并发 8、每组 10 轮、
  竞速 OFF/ON 配对、冷缓存、阈值 P95 相对改善 ≥50% 不变。

## race_after_ms=50 的选择依据（非凑阈值）
- 单元测试夹具用 `race.After=10ms`（`registry_race_test.go:41`），证明毫秒级是合理量级；
- 50ms < 首路尾延迟 300ms：首路尾部命中时，第二路在 50ms 启动、约 20ms 后（~70ms）
  抢先返回，能体现 hedge 收益；
- 50ms > 首路正常延迟（0–3ms）：正常情况下第二路不启动，不产生无谓请求放大。
- 该值在测试前一次选定，不根据结果调整。

## 承诺（证据纪律）
- 修正后只跑**这一次**正式样本；无论结果如何都如实判定 PASS / NOT_MET，
  不二次调参、不改阈值、不伪造。
- 原始冻结文档 `a10-scenario-freeze.md` 与其 sha256 保持不变（历史可追溯）。
- 判定口径：P95 相对改善 = (基线P95 − 竞速P95)/基线P95 ≥ 0.50 为 PASS，
  同时报告 P50/P95/P99、错误率、请求放大（extra upstream）与胜者分布。
