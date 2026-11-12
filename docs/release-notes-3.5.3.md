# 3.5.3 发布说明

> 版本号 `3.5.3`。基线 `v3.5.2`（`3159213`），本版只做三件事：Phase B 正确收口、
> P2-3 Reclaim 去重、迁移可观测性补齐。不含缓存身份模型改造（C0 已裁定不做）。

## 升级前必读

1. **首次启动会自动执行 Phase B**：把 `source_health` 中 `family='registry'` 的历史行
   归档到 `source_health_archive`，然后从运行表删除。这个过程在 `BEGIN IMMEDIATE`
   事务内完成，失败整体回滚，下次启动重试。
2. **迁移失败会阻止启动**。两种失败会直接退出，不会带病运行：
   - 事务内自校验不通过（归档数/删除数/剩余行不一致）；
   - 迁移标记已存在，但运行表里又出现了 registry 行 —— 说明有人把数据写回，
     需要人工判断，本版**拒绝再次删除**；
   - 运行行与归档行同主键但内容不一致（例如 Unarchive 跳过了冲突）—— 报冲突，
     两份数据都保留，由运维决定保留哪一份。
3. `source_health_archive` **永久保留**，不随 `sources` 删除而清理，用于事后核对。
4. 生产 3.5.1 的 `family='registry'` 行数为 0，正常升级时这一步是空迁移
   （日志 `candidates=0 archived=0 removed=0 left=0`）。

## 单租户信任边界（C0 裁定）

3.5.3 使用**单租户共享缓存**模型：所有持有网关 Access Token 的用户处于同一个缓存信任域，
可以读取该部署中的全部共享缓存内容。本版本不提供用户级缓存隔离，不解析、不转发客户端的
Registry 凭据。**不要把这套部署当作多租户隔离环境，给互不信任的主体共用。**

## 变更

- **Phase B 归档改用 `BEGIN IMMEDIATE`**：专用 `*sql.Conn`，避免 DEFERRED 事务在
  读→写升级时直接 `SQLITE_BUSY`。归档、删除、写标记同一事务。
- **迁移步骤日志**：一次性步骤登记到 `schema_migrations`，业务写入与标记同事务，
  失败整体回滚、可重试；启动日志输出 `batch/candidates/archived/removed/left/already_done`。
- **标记只写一次**：`already_done` 的启动不再刷新 `applied_at`。
- **P2-3 Reclaim 去重**：`reclaimStarted` 在锁内原子判定。同一 Lease 重复或并发
  `Reclaim` 只执行一次取消，指标与 `ReclaimSource` 计数不再重复。
- **冲突数据不再被误删**：`dup` 判定从「同主键已归档」收紧为「`fails/ok_at/last_at`
  完全一致」。同主键但内容不同时事务内报冲突、拒绝启动，运行行与归档行都原样保留；
  这堵住了「Unarchive 跳过冲突 → 撤销标记 → 下次启动把现行行删掉」的路径。
- **Unarchive 语义（方案 A）**：恢复数据与撤销迁移标记在同一事务。回滚后可安全重跑，
  归档表靠 `dup` 判定不重复写入（`archived=0 removed=N` 是正确结果）。

## 回滚

- `UnarchiveLegacyRegistry` 只恢复 **Phase B** 的 registry 数据，**不是**版本回滚工具。
- 3.5.2 已经把 `source_health_scope` 重建为四列主键，回退到 3.5.1 的表结构未经验证。
  **回退到 3.5.1 的正确做法：恢复升级前的完整数据库备份，再回退容器镜像。**
  不要依赖 Unarchive。
- 恢复后若再次启动 3.5.3+，迁移会重新执行并把数据再次归档——这是设计行为。

## 升级步骤

1. 停容器，`VACUUM INTO` 或文件级快照备份整库，并在副本上验证可打开、行数一致。
2. 换镜像启动，确认日志出现 `迁移步骤 b1-archive-legacy-registry 完成` 且 `left=0`。
3. `GET /readyz` 必须 200 且 `db.ok=true`、`cache.ok=true`；`/healthz` 只证明进程活着。
4. 面板抽查源列表与探测状态；确认 `source_health` 无 registry 行、归档表行数符合预期。
