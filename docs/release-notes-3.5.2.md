# 3.5.2 升级说明（3.5.1 → 3.5.2）

> 状态：**定稿**。验收通过，发布范围 A1+A2；P2-2 已关闭，P2-3 开放并转入 3.5.3。

## 发布范围（冻结）

3.5.2 只包含 **A1（Registry Host Recovery）+ A2（Runner 持久化调度）**。Phase B（legacy 数据退出）与 Phase C（缓存身份隔离）**不在本版本内**，归入 3.5.3，本版本不交付其任何代码或行为变化。

## 首次升级的数据边界

**首次从 3.5.1 升级到 3.5.2 时，所有 `registry_cap=1` 的 Host 都会进入 `initial`，镜像候选暂时为空；必须等当前代次的 Host 探测成功，才重新具备候选资格。**

原因不是迁移失败，而是信息边界：3.5.1 的 `source_health_scope` 行在迁移后一律为 `gen=0`，与「无凭据探测」合法写出的 `gen=0` 落在同一整数空间，无法证明其属于当前代次。3.5.2 选择 fail-closed：不把无法证明代次归属的历史健康证据重新授予候选资格，宁可重新探测一次。

因此升级后会经历一次全量 Host 重探窗口：

- 窗口长度取决于 Host 数量、探测并发与上游响应，最短一轮扫描节拍（默认 30s）+ 整批探测耗时；
- 每个 Host 探测成功后立即恢复候选，不需要等全部 Host 完成；
- 已存在的 `registry_host_recovery` 行不被覆盖，重复启动不会重置任何进度。

不受影响的部分：

- `registry_cap=2`（内容不一致 / 硬隔离）→ `hard-excluded`，保持排除语义；
- `registry_cap=0`（明确不支持 Registry）→ `unsupported`，按既有周期重验（6h）；
- 其它非 Host 维度的缓存、路由与源配置不变。

## 不承诺的内容

- 不承诺候选连续性；
- 不承诺保留升级前的 Host 历史健康状态（`fails` / `last_success_at` 不参与种子派生）；
- 不回填 `registry_host_gen`。

## legacy Registry 数据说明

`source_health` 中 `family='registry'` 的历史行**仍然保留**。3.5.2 之后不再有任何写入路径（`NoteProbeScoped` 拒绝 registry），候选判定也不再读取它们；它们是待 3.5.3 Phase B 清理的历史数据。

**不要把这些行理解为已归档或已清理**：本版本既没有迁移它们，也没有删除它们。归档与清理在 3.5.3 交付。

## 已知限制

- P2-3：两个并发 Reclaim 可能把同一个 Lease 的 `LeaseReclaim` 指标和 `ReclaimSource`
  返回数各记两次；名额归还由 `set.drop` 按 ID 去重，不会泄漏或错状态。3.5.3 处理。

- 3.5.0 之前创建的非 Docker Hub 源，`repo_host` 为空串（建列默认值，无回填迁移），会被当作 Docker Hub：既不进入 Host 恢复种子集合，也不进入 Host 探测队列。这是 3.5.0 建列时的既有数据兼容问题，不是 3.5.2 引入，也未在 3.5.2 解决。
- 种子的一次性标记检查发生在 `BEGIN IMMEDIATE` 之前。当前单 Runner / 单实例运行模型下无并发调用路径，不构成缺陷；多实例前需与 Guard 一并重做。
