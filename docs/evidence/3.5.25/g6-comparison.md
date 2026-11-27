# G6 性能对照（同 golang:1.27.3 容器 / 同硬件 / benchtime=100x / count=5）
# 门槛：P50 增幅≤10%，P95 增幅≤15%（§17.3）；仅适用缓存类 benchmark，非真实源站
benchmark | old_P50_ns | new_P50_ns | P50_delta% | old_P95_ns | new_P95_ns | P95_delta% | verdict
BenchmarkDockerCandidates300 | 1615579 | 1667668 | +3.22% | 1838105 | 1770606 | -3.67% | PASS
BenchmarkLegacyPerSourceSamples300 | 21019240 | 20883776 | -0.64% | 21066962 | 20926138 | -0.67% | PASS
BenchmarkRegistryCandidates300 | 1937166 | 2010506 | +3.79% | 2113403 | 2035463 | -3.69% | PASS

## 针对性基准 AddSourceHost（同 DB 循环，排除建库开销，benchtime=100x count=5）
- 3.5.24 样本(ns): [754644, 797149, 717822, 705526, 665969]
- 3.5.25 样本(ns): [863939, 668960, 657059, 644495, 686089]
- P50: 717822 → 668960 (-6.81%)
- P95: 788648 → 828369 (+5.04%)
- 门槛 P50≤10% / P95≤15%: PASS
- 注：该基准隔离了 AddSourceHost 单事务（3.5.24 事务外读 base_url vs 3.5.25 事务内读），样本量 5 轮×100 次；未包含真实网络/TLS，不替代 §17.1 真实源站对照。
