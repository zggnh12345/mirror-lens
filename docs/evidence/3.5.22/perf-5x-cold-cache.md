# 3.5.22 五轮冷缓存性能对照（2026-11-24）

## 结论

| 场景 | 基线 3.5.21 中位 | 候选 3.5.22 中位 | 变化 | 换源次数（基线/候选） | 结果 |
|---|---:|---:|---:|---:|---|
| S1 慢源 100KB/s → 快源 1.7MB/s | 615.104s | 45.048s | **-92.68%** | 0 / 5 | 达标（≥50%） |
| S2 当前 2.5MB/s，候选 1.7MB/s | 24.087s | 24.087s | 0.00% | 0 / 0 | 达标（5/5 不误换） |
| S3 最快源 500 不可用 → 次优 1.7MB/s | 35.402s | 35.404s | -0.01% | 5/5 接管（基线/候选） | 达标（5/5 接管） |

- 30/30 次下载均为 HTTP 200、63,000,000 字节、SHA-256 与官方 digest 一致。
- S1 候选 5/5 次均在约 1.06MB 处换源成功，基线 5/5 次不换源。
- S2 基线/候选均 5/5 次不换源；候选侧日志明确 `speed_rejected below_threshold`。
- S3 基线/候选均 5/5 次由 `perf-fast-206` 接管；最快源 `perf-fastest-fail` 每次返回 HTTP 500。
- 换源空档：S1 候选新源在旧源断开前约 1.276–1.279s 已开始接收，按 `max(0, 新源首字节-旧源断开)` 计 **P95 = 0s**（无空档）。

## 版本与对象

- 基线镜像：`mirror-lens:3.5.21-rollback`（3.5.21）
- 候选镜像：`mirror-lens:latest` = `abdf94b60ed0`（3.5.22，commit `c47c12a`）
- 测试 blob：63,000,000 字节（60.08 MiB，> `switch_min_file_mib=60`）
- SHA-256：`c14a7542a8f219871ff26b839971b99af7ef5af26d09a9b7655a389269ac0d00`
- 请求路径：`/v2/library/rust/blobs/sha256:<digest>`

## 测试条件

- 本机假源，独立限速；候选与基线使用同一 blob、同一请求路径。
- S1：`perf-slow-200` 100KB/s、仅 200 无 Range；`perf-fast-206` 1.7MB/s、支持 Range。
- S2：`perf-medium-206` 2.5MB/s；`perf-fast-206` 1.7MB/s。
- S3：`perf-fastest-fail` 持续返回 HTTP 500；`perf-fast-206` 1.7MB/s；`perf-slow-200` 100KB/s。
- 每轮使用全新数据库 + 空 cache 目录；`cache_entries`、`blobs`、`pending_blobs` 均为空，无 `.part`，基线与候选分别独立，不共享缓存。
- 并行分片关闭（`parallel.enabled=false`），Hedge 关闭，确保测的是单流换源路径。
- 每场景独立 5 次完整下载；耗时从客户端发起请求到完整文件结束。

## 原始每轮耗时

- S1 基线：615.108, 615.087, 615.097, 615.108, 615.104 s
- S1 候选：45.054, 45.055, 45.048, 45.044, 45.039 s
- S2 基线：24.086, 24.084, 24.087, 24.089, 24.095 s
- S2 候选：24.087, 24.091, 24.099, 24.078, 24.072 s
- S3 基线：35.402, 35.401, 35.402, 35.403, 35.396 s
- S3 候选：35.409, 35.404, 35.404, 35.402, 35.380 s

## 关键日志摘录

S1 候选（5/5 均出现，`sent` 为换源时已发送字节）：

```
换源成功 perf-fast-206 reason=switch_success sent=1063056 verified=false s_bps=1782000 p_bps=0 c_bps=105472 threshold_bps=1048576
换源成功 perf-fast-206 reason=switch_success sent=1055816 verified=false s_bps=1782000 p_bps=0 c_bps=105472 threshold_bps=1048576
换源成功 perf-fast-206 reason=switch_success sent=1063056 verified=false s_bps=1782000 p_bps=0 c_bps=105472 threshold_bps=1048576
换源成功 perf-fast-206 reason=switch_success sent=1063056 verified=false s_bps=1782000 p_bps=0 c_bps=105472 threshold_bps=1048576
换源成功 perf-fast-206 reason=switch_success sent=1063056 verified=false s_bps=1782000 p_bps=0 c_bps=105472 threshold_bps=1048576
```

S2 候选（5/5 均拒绝换源）：

```
换源拒绝 perf-fast-206 reason=speed_rejected detail=below_threshold kind="docker" s_bps=1782000 p_bps=0 c_bps=2666496 threshold_bps=3199795 sp=[1782 1782 1782]
单流判慢：没有可换的星标源
```

S3 候选/基线（5/5 均出现，故障为持续 HTTP 500）：

```
源返回错误，换下一个 ... source=perf-fastest-fail status=500
```

> S3 的 `switch_success=0` 是统计口径：初始源故障转移不计“中途换源”，接管按最终供数源 `X-Ml-Source=perf-fast-206` 统计，5/5 成功。

## 限制

- 本结果只用于本轮工程验收，5 次样本不宣称统计意义上的长期可靠性。
- 假源为本地合成流量，不代表公网抖动、DNS、TLS、上游限流等全部真实因素。
- S3 的故障类型明确为持续 HTTP 500，覆盖整个下载尝试；不是瞬时抖动。

## 证据文件

- `perf-5x-results.json`：30 条逐轮记录（状态、字节、SHA、耗时、源、换源次数、日志路径）
- `perf-logs/`：30 份网关原始日志
- `perf-5x-summary.json`：按场景汇总
