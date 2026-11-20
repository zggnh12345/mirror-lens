# 3.5.18 阶段 18a 执行证据

## 结论：PASS（18a 范围内）
- 分支：`feat/3.5.18`；起点 `6770711`；代码基线 `6ca6e76`。
- 1.6 权威文档、冻结稿、sha256 清单：未修改。
- 生产：未触碰（本次授权排除生产操作）。

## 范围
18a = 配置模型 + 迁移 + 后端读写 + 运行时解析 + 可观测性审计。
不含 Web UI（18b）、不含候选/调度/排序（18b/18c）、不含安全预算（18e）。

## 冻结裁定落实（设计冻结稿）
- 三键：`cross_registry_enabled` / `cross_registry_mode` / `cross_registry_sort_mode`，存 `settings` 表，**不新增表、不改 Schema**。
- 冲突优先级：新 `cross_registry_enabled` 优先；旧 `off` 仅在新 enabled 不存在时映射 `enabled=false+sequential`；`sort_mode` 独立；三键全缺省以部署基线为底层，缺省基线为空时 = `enabled=true+sequential+fixed`（默认 ON）。
- B OFF（`enabled=false`）不删可信映射与跨站参选配置。
- 单一解析入口 `ResolveBConfig(deployMode)`，无副作用；写入 API 统一走 `SetBEnabled`/`SetBMode`/`SetBSortMode`（同事务写审计）。

## 关键文件
- `internal/store/crossconfig.go`：`BConfig`、`ResolveBConfig`、`MigrateBConfig`、`HasBConfig`、三 setter、审计。
- `internal/api/crossregistry.go`：`GET /api/cross-registry` 返回 `enabled/mode/sort_mode/configured/selected/starred`；`PUT` 接受 `enabled/mode/sort_mode/selected`，`mode` 只接受 `sequential/race`（`off` 由 `enabled=false` 表达）。
- `internal/gateway/replica.go`：`replicaModeLive()` 改为 `ResolveBConfig` 合成模式；三键全缺省时返回空走部署基线分支，保证部署期 `off` 即时生效。新增 `DeployReplicaMode()` 暴露部署基线。
- `main.go`：启动期 `MigrateBConfig`，失败即拒绝启动（避免静默缺省改变 B 启停）。
- `config.example.yaml` / `internal/config/docs.go`：示例与说明改为 3.5.18 三字段语义，默认 `sequential`（默认 ON）。

## 测试（本次重新运行）
- 定向：`TestResolveBConfigMatrix`（11 子用例）、`TestMigrateBConfigIdempotent`、`TestMigrateBConfigConflictWritesAudit`、`TestFreshInstallBDefaultsOn`、`TestBOffKeepsMappingAndSelection` 全 PASS。
- 回归：gateway `TestReplicaModeDefaultsAndLiveSwitch` 等 replica 测试 PASS；api、config 全量 PASS。
- 全量门禁：`gofmt -l .` 空；`go vet ./...` RC=0；`go build ./...` RC=0；`go test ./... -count=1` 34 包 ok（见 `18a-gate.log`）。
- race：`CGO_ENABLED=1 go test -race ./... -count=1` 34 包 ok，`DATA RACE` 计数 0（见 `18a-race.log`）。

## 未覆盖 / 边界
- 生产真实 settings 配置状态：无生产授权，未实测，标 `WAITING_FOR_USER_DECISION`。
- 18a 不实现 Web UI、候选资格、嵌套调度、动态排序、安全预算；分别属 18b/18c/18d/18e。
- `replicaMode()` 运行时接入以兼容方式完成（默认 ON，但无可信映射/参选 Source 时不产生跨站请求）；端到端行为在 18b 联调验证。
