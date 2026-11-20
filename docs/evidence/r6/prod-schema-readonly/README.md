# 生产 Schema 只读核验与迁移演练（3.5.16 → 3.5.17）

## 范围

只读核验生产库当前 Schema 指纹，并在**副本**上演练 3.5.14 → 3.5.17 迁移。
未修改生产数据库、未停服、未切换容器。

## 方法

1. 生产数据目录以 `:ro` 挂载进临时容器；
2. 用 SQLite backup API（`sqlite3 .backup`）生成一致性副本，写入独立 docker volume；
3. 在副本上读取 `schema_meta`、执行 `db-migrate migrate` 与 `verify`；
4. 结束后删除含生产数据的卷与临时镜像。

## 结果

生产库（`mirror-lens:3.5.16`）：

```
schema_version = 3.5.14
schema_hash    = 7e5d71a711609eeb3d5e0cc883f1b5163268ff84e904a79370a2af93e0c1e28c
baseline       = 3.5.11
```

与冻结的 3.5.14 基线指纹一致。

迁移演练（副本）：

- `migrate --baseline 3.5.14` 退出码 0
- 输出 Schema：`3.5.17`，hash `bfc7f2f9e1c1e489c294ac8ee4b7f7d463017e789f14e703ff3e4e6e400bb4ae`
- `verify` 退出码 0
- 旧库缺失的 F08 七张表按预期保留为空表（`trusted_mapping_generation` 回填 6 行）

## 未执行

- 生产库实际迁移（需停服、备份、切换镜像，单独授权）
- 迁移后生产流量验证

## 注意（实测踩到）

用 `verify` 只读打开副本后，SQLite 仍会在同目录生成 `-shm`；`migrate` 会以
“旧库存在非空 -shm” 拒绝。演练时应使用未被打开过的干净副本，或先对副本执行
`db-migrate checkpoint`，再迁移。`commands.txt` 记录的是可复现的干净副本流程。

## 文件

- `commands.txt`：完整命令与退出码
- `meta.txt`：生产副本 `schema_meta`
- `verify-src.log`：3.5.17 运行时拒绝 3.5.14 副本（预期）
- `migrate.log`：迁移全过程输出
- `verify-out.log`：迁移结果校验

## 敏感性

副本含生产数据，已在核验后删除；本目录只保留 Schema 元数据与工具输出，不含业务数据。
