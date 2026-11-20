# Mirror Lens 3.5.17 升级指南

> [!WARNING]
> **3.5.17 不会自动迁移数据库。**
> 不支持「拉取新镜像 + 重启」的跨 Schema 自动升级；不要使用 Watchtower 等自动更新工具直接更新。
> 必须先停服、备份、离线迁移、验证 Schema，再启动新版本。否则应用会因 Schema 版本校验失败而**拒绝启动**。

本文说明如何从 3.5.16（Schema 3.5.14）升级到 3.5.17（Schema 3.5.17）。
迁移工具 `db-migrate` 是离线、异库迁移工具：读取旧库、写出一个新库，**从不原地改写生产库**，遇到已存在的目标文件会拒绝覆盖。

## 适用范围

- 源库：Schema `3.5.14`（3.5.15 / 3.5.16 均使用此基线）。
- 目标库：Schema `3.5.17`，预期结构指纹 `bfc7f2f9e1c1e489c294ac8ee4b7f7d463017e789f14e703ff3e4e6e400bb4ae`。
- 其他 baseline（3.4.6 / 3.5.4 / 3.5.5 / 3.5.7 / 3.5.11）的迁移不在本文范围；`db-migrate` 的 `--baseline` 会按已冻结指纹校验源库。

## 前置条件

1. 已确认当前运行版本为 3.5.16、Schema 为 3.5.14。
2. 有足够磁盘空间容纳一份完整数据库副本与备份。
3. 已准备好 `db-migrate` 工具（从 3.5.17 源码或发布产物获取）。
4. 可停机；迁移期间服务不可用。
5. 已知生产容器使用的不可变镜像 ID（`docker inspect --format '{{.Image}}' <容器>`），不要只用可变 tag 判断身份。

## 升级步骤

以下步骤以数据目录 `/data`（容器内挂载点）为例，请替换为实际路径。

### 第 1 步：停止服务

停止 3.5.16 容器，并确认没有进程仍持有数据库及其 `-wal` / `-shm` 文件。迁移必须在数据库不被写入时进行。

### 第 2 步：备份

对当前数据库做一致性备份（推荐 SQLite backup API，避免直接拷贝处于 WAL 模式的库）：

```sh
sqlite3 /data/mirror-lens.db ".backup /data/mirror-lens.db.backup"
sha256sum /data/mirror-lens.db.backup
```

同时备份 `secret.key`（凭据加密密钥）。**不要替换或重新生成 `secret.key`**，否则历史密文凭据将无法解密。

### 第 3 步：确认源 Schema

```sh
db-migrate inspect --db /data/mirror-lens.db
```

确认输出为 `schema_version=3.5.14` 且指纹匹配 `7e5d71a711609eeb3d5e0cc883f1b5163268ff84e904a79370a2af93e0c1e28c`。

### 第 4 步：离线迁移到新库

```sh
db-migrate migrate \
  --in  /data/mirror-lens.db \
  --out /data/mirror-lens.3.5.17.db \
  --baseline 3.5.14
```

- 目标文件不得预先存在；工具会拒绝覆盖。
- 若源库存在未合并的 `-wal`，先对副本执行 `db-migrate checkpoint --db <副本>`，不要在正式库上直接操作。
- 不要在迁移目标旁遗留 `-shm` / `-wal`；迁移工具会以「snapshot is being modified while cloning」拒绝。

### 第 5 步：验证目标 Schema

```sh
db-migrate verify --db /data/mirror-lens.3.5.17.db
```

预期退出码 0，输出 `schema_version=3.5.17`、`hash=bfc7f2f9…`。验证失败则不要切换，保留备份并排查。

### 第 6 步：切换数据库并启动

用迁移验证通过的新库替换正式库，保留原备份；确认 `secret.key` 未变，然后启动 3.5.17 镜像。

启动后检查：

- 启动版本日志显示 `3.5.17`。
- `mirror-lens -version` 输出与镜像标签一致。
- 关键路径回归：Manifest 拉取、缓存命中、私有认证。

> 3.5.17 默认不启用跨 Registry 调度：`replica_mode` 为空时行为与 3.5.16 一致；`unified_framework` 缺省为开启（新 RequestScope/Claim 执行链）。如需完全回退到旧执行链，设置 `registry.unified_framework: false` 并重启，无需回滚数据库。

## 回滚

迁移后的库旧二进制读不了（Schema 版本检查会拒绝）。**回滚不是换回镜像标签**，而是：

1. 停止 3.5.17 容器。
2. 恢复第 2 步的数据库备份与 `secret.key`。
3. 启动原 3.5.16 镜像。

## 自动化说明

仓库中的 `scripts/upgrade-db.sh` 将「停服校验 → 备份 → checkpoint → 迁移 → verify → 切换 → manifest」流程脚本化，但它：

- 不会替你停止或启动容器；
- 要求提供不可变镜像 ID 做身份校验；
- 检测到有进程持有数据库时 fail-closed。

它是半自动的受控升级脚本，不是启动时自动迁移，也不为零停机升级提供支持。

## 核验依据

生产 Schema 只读核验与 3.5.14→3.5.17 副本迁移演练的真实证据见 `docs/evidence/r6/prod-schema-readonly/`。
演练结果：源库 `3.5.14 / 7e5d71a7…`，目标 `3.5.17 / bfc7f2f9…`，迁移与 verify 均退出码 0。
**该演练只在数据副本上进行，生产库未迁移、未停服、未切换。**
