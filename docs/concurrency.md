# 3.0 并发模型

本文定义 Mirror Lens 3.0 的并发账本。它是 Step 1～7 的实现依据，也是验收用例的命名来源。
**本文只描述模型，不描述算法优化**；AIMD 算法、跨任务公平份额不在 3.0 范围内。

## 1. 术语与分层

三个正交的维度，不要合并成一个枚举：

| 维度 | 取值 | 含义 |
| --- | --- | --- |
| **Tier** | A / B / B′ / C | 这条回源 Body 归属哪套并发策略 |
| **Kind** | normal / handover | 这次申请走哪一级准入 |
| **State** | Reserved / Active / Reclaiming / Released | Lease 的生命周期 |

### Tier

- **A**：普通镜像源回源。单流、并行 resp0/分片、midFill resp0/分片、换源。
  全部进 Lease 账本，受 `max_per_source` 约束。
- **B**：官方源、TrustedRelay、Git upload-pack。各自独立配额，**不进** `max_per_source`。
- **B′**：预检（`parPreflight`）。独立全局配额，只观测不计正式名额。
- **C**：缓存命中。没有上游 Body，不记账。

### Kind

- `normal`：日常派发，准入上限 `dispatch_cap`。
- `handover`：交接（换源、异源接管），准入上限 `hard_max`，且**必须非阻塞**（I6）。

## 2. 状态机

```
Reserved ──响应头到──> Active ──Close──> Released
   │                     │
   │                     └─ Transfer(同源、同 SourceID) ──> Active
   │                        （Owner 换、Generation+1、计数不变）
   └─拨号失败/取消──> Released

Reserved/Active ──无进展──> Reclaiming ──取消+关闭+body 全退出──> Released
```

- `Reserved` 在**拨号之前**取得，覆盖建连、TLS、等待响应头。
- `Active` 从拿到响应头开始，到 Body 关闭且读取协程退出为止。
- `Reclaiming` 是“已判定无进展、正在收尾”，**仍然占名额**。

## 3. 不变式

> I1～I8 每条对应一个测试名（见第 9 节）。

- **I1** `leases_held = Reserved + Active + Reclaiming`。
  这是 Manager 内部在**同一把锁内**计算的快照；导出到 Prometheus 的多个 gauge 允许存在极短的采样差异，面板不得假设严格一致。
- **I2** `a_bodies_active` = 尚未退出的 A 档上游 Body 数，**包含 Reclaiming 中的 Body**。
  与 Lease 数不等：同源 Transfer 的重叠窗口内，两个 Body 对应一个 Lease（`bodies = leases + 1`）。
  这个 +1 只对单次 Transfer 成立，多个交接重叠时不构成恒等式。
- **I3** `normal` 要求 `leases_held < dispatch_cap`；`handover` 要求 `leases_held < hard_max`。
  `observe` 模式下两者都只作为观测基准，不强制。
- **I4** Lease 进入 `Reclaiming` 后不得减计数；必须取消该 Lease 关联的**全部** Attempt、关闭 Body、确认读取协程退出，才可 `Released`。
- **I5** `Release(token)` 携带 `{LeaseID, Generation}`；Generation 不匹配即 no-op。
  `Transfer` 只允许 `Active → Active`，成功后旧 token 失效。
- **I6** `handover` 一律非阻塞：拿不到 `hard_max` 名额就保留旧 Body、取消本次交接，不得排队。
- **I7** 等待名额时不得持有同源 Lease；不得持有 A 源 Lease 等待 B 源。
- **I8** Lease 真正释放的条件是 `owner detached && bodyRefs == 0`。
  Generation 只解决“谁能释放所有权”，不能证明旧 Body 已退出；Body 必须单独登记生命周期引用。

## 4. 上限

```
hard_max       = max_per_source
capacity_limit = hard_max                  # 3.0 固定；AIMD 只允许在 hard_max 内调整它
dispatch_cap   = max(1, min(hard_max-1, floor(capacity_limit * 0.8)))
```

| hard_max | dispatch_cap | 交接预留 |
| --- | --- | --- |
| 1 | 1 | 0 |
| 2 | 1 | 1 |
| 4 | 3 | 1 |
| 6 | 4 | 2 |
| 8 | 6 | 2 |
| 10 | 8 | 2 |

`hard_max = 1` 时没有交接空间：同源 Transfer 可以（不新增名额），异源接管与换源必须软失败。

## 5. 准入模式

- `observe`：只记账、记峰值、记 cap_hit；不等待、不拒绝。**Step 3 默认。**
- `blocking`：等，语义对齐 2.6.19。
- `wait_then_fallback`：最多 `wait_timeout`（默认 2s），超时组内换源。**3.0 最终默认（Step 7 切换）。**
- `non_blocking`：不等待，直接换源。

等待规则：

- 每个源一条 FIFO 等待队列，释放时**只唤醒队首**，不广播。
- 等待超时不记源故障、不触发熔断、不喂 AIMD。
- 等待结束后必须重新检查候选源的实时容量，不能复用排序快照。

## 6. 交接

### 同源接管（midFill，且新旧 SourceID 相同）

```
旧 Body Active，Lease L(gen=g)
  → 新 resp0 建立并校验 206/Content-Range
  → L.Transfer(owner=new, gen=g+1)，计数不变
  → 旧 Body 关闭，retire(old token)
  → 两个 Body 在重叠窗口内都计入 a_bodies_active
```

同源判定用**稳定 SourceID**，不用 URL 字符串。

### 异源接管（midFill 选了别的源）

新 Lease（Kind=handover，+1）；旧 Lease 保持到旧 Body 关闭。拿不到名额 → 软失败，继续单流。

### 换源（trySwitch）

无论新旧是否同源，一律新开 Lease（Kind=handover，+1）；验证新响应能接续后才关闭旧 Body、释放旧 Lease。
拿不到名额 → 保留旧 Body，本次换源放弃。**不允许**先释放旧 Lease 再申请新的。

## 7. 回收

```
无进展（A 档看门狗 / 客户端断开）
  → 状态置 Reclaiming（仍占名额）
  → 取消该 Lease 关联的全部 Attempt 的独立 context
  → 关闭所有关联 Body
  → 等待读取协程退出（bodyRefs → 0）
  → Released（此时才减计数）
```

- 每个 Attempt 有独立 `context.Context`；回收针对 Attempt，不针对整个客户端请求。
- `Close()` 返回不等于读取协程退出；以 bodyRef 归零为准。
- 回收超时仍未退出 → 保持 `Reclaiming`，累加 `lease_reclaim_stuck`。
- 回收与正常 Close 竞争时只释放一次（`sync.Once` + Generation 栅栏）。

## 8. 指标口径

| 指标 | 口径 |
| --- | --- |
| `leases_held` | Reserved + Active + Reclaiming |
| `leases_reserved` / `leases_active` / `leases_reclaiming` | 分状态 |
| `a_bodies_active` | A 档未退出 Body 数（含 Reclaiming；不含 B/B′/C） |
| `cap_hit_normal_total` | 撞 `dispatch_cap` 次数 |
| `cap_hit_handover_total` | 撞 `hard_max` 次数 |
| `lease_max` | 进程生命周期内 `leases_held` 峰值（按源） |
| `lease_wait_seconds` | normal 实际等待时长 |
| `lease_wait_timeout_total` | 等待超时次数 |
| `lease_transfer_total` / `lease_transfer_failed_total` | 交接成功/失败 |
| `lease_reclaimed_total` | 完成回收 |
| `lease_reclaim_duration_seconds` | 回收耗时 |
| `lease_reclaim_stuck` | 超时仍处于 Reclaiming 的数量 |
| `preflight_inflight` / `preflight_skipped_total` | 预检并发与跳过数 |

## 9. 验收映射

| 不变式 | 测试 |
| --- | --- |
| I1 快照一致 | `TestSnapshotConsistent` |
| I2 Body 与 Lease 解耦 | `TestBodiesExceedLeasesDuringTransfer` |
| I3 两级准入 | `TestNormalCapAndHandoverCap` |
| I4 回收不提前释放 | `TestReclaimHoldsUntilBodyExit` |
| I5 Generation 栅栏 | `TestLeaseTransferFencesOldOwner` |
| I6 交接不阻塞 | `TestHandoverNeverBlocks` |
| I7 等待不持 Lease | `TestWaitWithoutHoldingLease` |
| I8 Body 引用计数 | `TestReleaseWaitsForAllBodies` |

## 10. B 档边界

- **官方源**：沿用 `privSem`（整条请求）与分片 sem。
- **TrustedRelay**：新增 `relay_concurrency`（默认 4），按排名截断参与竞速的站数；每条实际发出的 relay 请求持有一个名额，响应 Body 关闭后释放。
- **Git upload-pack**：新增 `git_concurrency`（默认 4），在发起 POST 前取得，响应体转发完毕后释放。

这三者都不进 `max_per_source`，也不进 `leases_held`。

## 11. 不在 3.0 范围

- 新 AIMD 算法（3.0 固定 `capacity_limit = hard_max`，`dynamic` 默认关闭）。
- 跨任务公平份额（单任务仍可用满 `dispatch_cap`）。
- 源端 429 与容量的联动策略。
