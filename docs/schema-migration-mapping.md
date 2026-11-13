# Canonical Schema 字段映射（3.4.6 / 3.5.4 → 3.5.5）

状态：P0 冻结事实。数据来源是本机真实库副本，不是纸面推断：

- `docs/schema-inventory/schema-inventory-3.4.6.json`（19 张表）
- `docs/schema-inventory/schema-inventory-3.5.4.json`（25 张表，含 `_writable` 测试表）
- 3.5.4 生产快照：91 源、190,670 条 checks、3,169 条 source_events。

## 1. 版本矩阵

| 输入 | 处理 | 输出 |
|---|---|---|
| 全新空库 | `db-migrate init` / 服务首启 `schema.EnsureFresh` | 3.5.5 Canonical |
| 3.4.6 真实副本 | `db-migrate migrate --baseline 3.4.6` | 3.5.5 Canonical |
| 3.5.4 真实副本 | `db-migrate migrate --baseline 3.5.4` | 3.5.5 Canonical |

三份输出的 `schema_hash` 必须相同。本次实测均为
`0842cd1efa4b7330cf53a7f9d98491597481cfcec80d1b5560bf8f9f0ef81170`
（以实际 `verify` 输出为准；该值由 Canonical DDL 排序后 SHA-256 计算）。

不存在“3.5.5 输入库”：3.5.5 是输出，不是升级输入。

## 2. 表级映射

| 表 | 3.4.6 | 3.5.4 | Canonical 3.5.5 | 规则 |
|---|---:|---:|---|---|
| sources | 有 | 有 | 保留 | 唯一需要改写的表，见 §3 |
| checks | 有 | 190,670 | 保留 | 公共列逐列拷贝；`kind` 按旧库已有值保留 |
| source_events | 有 | 3,169 | 保留 | 逐列拷贝，`code` 缺失按 0 |
| source_health | 有 | 255 | 保留 | 逐列拷贝 |
| source_health_archive | 有 | 0 | 保留 | 表保留；无数据不伪造 |
| source_health_scope | 有 | 0 | 保留 | 恢复种子来源，保留表结构 |
| cache_entries / blobs / pending_blobs | 有 | 有 | 保留 | 逐列拷贝 |
| blob_integrity_failures | 有 | 0 | 保留 | 逐列拷贝 |
| traffic_stats / live_speed / proxy_speed | 有 | 有 | 保留 | 逐列拷贝 |
| breaker_state | 有 | 2 | 保留 | 逐列拷贝 |
| audit / sessions / tokens / settings | 有 | 有 | 保留 | 逐列拷贝 |
| source_groups | 有 | 2 | 保留 | 逐列拷贝 |
| registry_host_recovery / registry_host_gen | 有 | 0 | 保留 | 当前健康恢复模型，非遗留 |
| trusted_relays | 有 | 0 | 保留 | 逐列拷贝 |
| meta | 有 | 2 | 保留 | 逐列拷贝；schema 元数据不写这里 |
| schema_migrations | 3 行 | 3 行 | **删除** | 由 `schema_meta` 取代；历史 migration 记录不进新库 |
| `_writable` | 无 | 0 行 | **删除** | 旧版启动自检残留，正式库中不得存在 |
| schema_meta | 无 | 无 | **新增** | schema_version / schema_hash / baseline / created_at |

`sqlite_sequence` 不拷贝行，按目标表最大 ID 由 SQLite 重建。

## 3. sources 的字段决策

`sources` 是唯一需要语义改写的表。其余表按公共列原样搬迁，缺列使用
Canonical 默认值，并且必须在报告中列出 `skipped` 表/列。

### 3.1 repo_host 是唯一归属事实

- `repo_host` 非空 = 该源属于对应 Registry。
- `repo_host = ''` = **没有 Registry 归属**，不是 Docker Hub。
- Docker Hub 必须显式存 `docker.io`。
- GHCR 必须显式存 `ghcr.io`。
- 不引入 `source_host_caps`、一源多 Host。

3.5.4 生产快照的 `repo_host` 分布（91 源）：

| repo_host | 数量 |
|---|---:|
| `docker.io` | 34 |
| `ghcr.io` | 2 |
| `''` | 55 |

空 Host 的 55 条不是同一种状态：

| 派生身份 | 数量 | 含义 |
|---|---:|---|
| `not_applicable` | 9 | 纯 GitHub 源（`github_cap=1` 且无 Registry 归属） |
| `unclassified` | 46 | 历史原因未识别，保留空 Host，不猜 |
| 合计 | 55 | 不得只统计 46 |

### 3.2 registry_identity_state 退役

- 运行时不再持久化 `registry_identity_state`。
- API JSON 仍保留该字段，由 `deriveRegistryIdentity` 派生：
  - `repo_host != ''` → `identified`
  - `repo_host == '' && github_cap == 1` → `not_applicable`
  - 其余 → `unclassified`
- 旧库中的 `registry_identity_state` 值不搬入 Canonical；不可由 Host 与能力
  唯一确定的历史行保持 `unclassified`，不伪造 `identified`。

### 3.3 能力字段

`docker_cap`、`github_cap`、`ghcr_cap` 是服务能力的唯一事实：

- `1` = yes，`0` = no，`-1` = unknown。
- `CapUnknown` 不等于 `not_applicable`；UI 与 API 必须保持语义区分。
- 官方 Host（`docker.io` / `ghcr.io`）不进入 foreign Registry 候选池。
- 六条双用途源保持 `docker.io + docker_cap=1 + ghcr_cap=1 + ghcr_prefix`
  原值，不因为 Canonical 收敛改变语义。

### 3.4 空 Host 不自动补 docker.io

旧版 `backfillRepoHost` 会在启动时把空 Host 批量写成 `docker.io`。新版本
删除该行为：

- 离线工具只复制 `repo_host` 原值。
- 空 Host 的 55 条原样保留。
- 只有用户显式设置 Host，或新增源走 `AddSource` 的显式默认，才写
  `docker.io`。
- `AddSource` 未传 Host 时的默认映射是 `docker.io` + `identified`，这是
  **新写入**规则，不是对历史行的回填。

## 4. 历史迁移的冻结

以下内容保留在源码中作为历史证据，但运行时不再调用：

- `identityMigrationStmts` 五条 SQL 与 checksum 常量：一字不改。
- `knownMigrationChecksums` 中的历史定义：保留。
- `migrateLegacyRegistry`、`backfillRepoHost`、`ensureColumns`：从 `Open`
  调用链移除，冻结为离线工具职责。
- `schema_migrations` 的历史行（3.5.3-phase-b-tables、
  b1-archive-legacy-registry、3.5.4-registry-identity）不写入新库。

## 5. 无法确认时的拒绝规则

离线工具遇到以下情况必须失败，不得填默认值：

- 旧库缺少 `sources` 或 `repo_host`。
- 目标表存在但旧库列无法映射到 Canonical 公共列。
- 目标文件已存在（拒绝覆盖）。
- 旧库 `-wal` 非空且未对副本做 checkpoint。
- 源 `repo_host` 非空但不在允许的 Registry Host 形状内（由
  `registryhost.Check` 拒绝）。
- 迁移后行数、公共列行哈希、`foreign_key_check` 不一致。

## 6. 自建 Registry 的边界

当前 `registryhost.Check` 是硬编码白名单（Docker Hub / GHCR 及其变体），
`Check` 拒绝带端口、IP、未知域名。本次 Canonical 收敛**不改变**该策略：

- 自建 Registry 仍不被接受。
- 不引入可配置白名单或 SSRF 代理面。
- 需要自建 Registry 时单独立项，不在 Schema 迁移中夹带。

## 7. 验收证据

- 3.4.6 → Canonical：`out/3.4.6-canonical.db`，249,856 字节，`verify` 通过。
- 3.5.4 → Canonical：`out/3.5.4-canonical.db`，`verify` 通过。
- 空库 → Canonical：`out/fresh.db`，`verify` 通过。
- 三库 `schema_hash` 一致。
- 新旧二进制互斥：3.5.5 打开旧库报
  `数据库缺少 schema_meta（旧库必须先用离线工具升级）`。
- 回滚只恢复 `mirror-lens.db.pre-3.5.5`（含 `-wal`/`-shm`）与**原 secret.key**，
  不做 DB/key 联合替换。
