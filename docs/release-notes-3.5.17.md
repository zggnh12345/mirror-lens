# Mirror Lens 3.5.17 发布说明

> [!WARNING]
> **升级警告 — 跨 Schema 升级必须手动完成（3.5.16 → 3.5.17）**
>
> 1. 3.5.17 不提供启动时自动 Schema 迁移。
> 2. 不支持只拉取新镜像并重启的跨 Schema 自动升级。
> 3. 不得使用 Watchtower 等自动更新工具直接更新。
> 4. 必须先停服、备份数据库、执行离线迁移、验证 Schema，再启动新版本。
> 5. 详细步骤见 docs/upgrade-3.5.17.md。
>
> 直接使用旧数据库启动 3.5.17 会因为 Schema 版本校验失败而**拒绝启动**（fail-closed）。这不是可跳过的警告。

**版本：** 3.5.17
**程序版本：** `internal/version/version.go` 为唯一权威来源；`-version` / `--version` 与启动日志均由其派生。
**Schema：** 3.5.17（由 3.5.14 经离线工具迁移得到；应用运行时不迁移）
**发布状态：** 本地提交完成；**未推送分支、未打 `v3.5.17` Tag、未创建 Release、未推送 GHCR**。

## 版本范围

3.5.17 交付两项独立能力，以及统一 RequestScope / Claim / R·P·G 预算控制：

- **A：同一 Registry 内星标 Source Manifest 竞速**（继承 3.5.16）。
- **B：跨 Registry 单层可信 Manifest 调度**（默认关闭，`replica_mode` 为空时行为与 3.5.16 一致）。

明确**不在**本版本范围（正式延期至 3.5.18）：

- B08：A+B 嵌套竞速（B 路径每个副本只发一路，不调用站内 A 竞速）。
- B13：A+B 组合预算（无组合调用路径，无从验收）。

不得以本版本名义宣称 A+B 组合完成。权威范围与门禁见 `docs/registry-roadmap-v1.6.md` 第 11.9、11.10 节。

## 本版本变化

- 统一执行框架：RequestScope、Claim 生命周期、R=16 / P=8192 / G 预算。
- 跨 Registry 可信 Manifest 调度与顺序回退（需显式配置可信副本映射；默认不启用）。
- F08 管理配置与 G0 流式内容一致性。
- 发布工程增强：启动版本日志、`-version` / `--version`、CI Tag/版本一致性校验、OCI image labels、Docker 构建期版本守卫。

## 升级说明（重要）

从 3.5.16（Schema 3.5.14）升级到 3.5.17 **必须离线迁移数据库**，不能只更新镜像：

1. 停止 3.5.16 容器。
2. 备份数据库（含 WAL / SHM 一并处理）。
3. 使用 `db-migrate migrate --baseline 3.5.14` 在离线副本上执行迁移。
4. 使用 `db-migrate verify` 验证目标 Schema 为 3.5.17。
5. 再启动 3.5.17 镜像。

**完整命令、回滚步骤与前置条件见 `docs/upgrade-3.5.17.md`。**

不得使用 Watchtower 等自动更新工具直接跨 Schema 更新；这类工具只换镜像、不迁移数据库，会导致服务启动失败。

## 已知限制与披露

| 项目 | 状态 | 说明 |
| --- | --- | --- |
| B15 外部性能验收 | `EXTERNAL_BLOCKED` | 未取得真实公共 Registry 的同环境配对 P50/P95/P99 数据，**不宣称性能改善**；A+B 配对性能随 B08/B13 延期至 3.5.18 |
| 生产数据库迁移 | `NOT_EXECUTED` | 已完成生产 Schema 只读核验与 3.5.14→3.5.17 副本迁移演练；生产库未迁移、未停服、未切换 |
| B08 / B13 | `DEFERRED_TO_3.5.18` | 本版本不交付 A+B 嵌套与组合预算 |
| 发布授权 | 未执行 | 未推送、未打 Tag、未创建 Release、未推送 GHCR |

## 验证状态

- `gofmt` / `go vet ./...` / `go build ./...` 通过。
- `go test ./... -count=1`：34 包通过。
- `go test -race ./... -count=1`：34 包通过，无 data race。
- 冻结清单 `docs/3.5.17-final-freeze.sha256` 全项匹配。
- Docker 构建：`VERSION=3.5.17` 成功，`VERSION=3.5.18` 由版本守卫真实失败；镜像 OCI label 与容器内 `-version` 一致。
- 生产 Schema 只读核验与副本迁移演练证据：`docs/evidence/r6/prod-schema-readonly/`。

上述为内部验收证据；B15 与生产迁移见「已知限制与披露」，不因内部验收通过而解除。
