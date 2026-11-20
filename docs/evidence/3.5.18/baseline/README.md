# Mirror Lens 3.5.18 阶段 0 基线取证报告

取证日期：2026-11-20 (CST)
实施分支：`feat/3.5.18`
起点提交：`677071117582f653cad2b0d5a08d467940edcc6e`（冻结提交）
代码基线提交：`6ca6e76a72eca13ad2f3311c59263900679130de`
冻结文档：`docs/3.5.18-design-freeze-errata-draft.md`（FROZEN，sha256 见 `docs/3.5.18-design-freeze.sha256`）

## 基线事实（实测）

| 项 | 值 | 来源 |
|---|---|---|
| 当前分支 | feat/3.5.18 | `git rev-parse --abbrev-ref HEAD` |
| HEAD | 677071117582f653cad2b0d5a08d467940edcc6e | `git rev-parse HEAD` |
| 起点→HEAD 差异 | 仅冻结文档 2 文件，运行时源码一致 | `git diff --stat 6ca6e76..6770711` |
| 1.6 权威文档 Blob | `74a0df7e832f7b0684617d08b2922404b34b4145` | `git rev-parse HEAD:docs/registry-roadmap-v1.6.md` |
| 程序版本 | 3.5.17 | `internal/version/version.go` |
| Schema 版本 | 3.5.17 | `internal/store/schema/schema.go` |
| Go 工具链 | go1.27.4 linux/amd64 | 临时容器 golang:1.27-alpine |
| 构建方式 | `--volumes-from hermes` 挂载 repo，GOCACHE/GOPATH 置 /tmp | 本容器无 go，见下 |

## 关键代码事实核对（与设计文档逐条对照）

| 设计文档论断 | 代码实测 | 结论 |
|---|---|---|
| `replica.go:166-169` 空参选集合直接 `return nil,0,nil`，阻断顺序候选发现 | 属实：`selected := g.st.EffectiveCrossSelected(); if len(selected)==0 { return nil,0,nil }` | 已证实 3.5.17 实现偏差，18b 须修正 |
| B 模式三值 sequential/race/off（`cross_registry_mode`） | 属实：`internal/store/crossselect.go` crossModeKey；`CrossRegistryMode()` 返回 sequential/race/off/"" | 18a 须拆为 enabled + mode + sort_mode |
| 部署期 `registry.replica_mode` 默认 off | 属实：`config.example.yaml:111 replica_mode: "off"`；`config.go:133 ReplicaMode` | 18a 迁移矩阵须处理 |
| 运行时缺省 `""` 解析为 sequential | 属实：`replica.go:82-99 replicaMode()` 把 ""→sequential | 已证实 |
| 跨站参选默认空、恒为星标子集 | 属实：`crossselect.go` CrossSelectedSources/EffectiveCrossSelected | 基线行为正确 |
| C=2、M=4、A=2、R=16 冻结值 | 属实：`replica.go:49-51 crossReplicaM=4 / crossReplicaC=2`；1.6 §11.2 R=16 | 18e 不得扩大 |

## 全量门禁基线

完整输出见 `docs/evidence/3.5.18/baseline/baseline-go.log`（race 单独见 `race.log`）。

| 检查 | 结果 | 证据 |
|---|---|---|
| gofmt -l . | 空（无未格式化文件） | GOFMT 段输出为空 |
| go vet ./... | PASS（RC=0） | VET_RC=0 |
| go build ./... | PASS（RC=0） | BUILD_RC=0 |
| go test ./... -count=1 | PASS（RC=0，34 包 ok） | TEST_RC=0 |
| go test -race ./... -count=1 | PASS（RC=0，34 包 ok，零 DATA RACE） | race.log / RACE_RC=0 |

说明：首次 race 因 `go: -race requires cgo` 失败，属 golang:1.27-alpine 默认 CGO_ENABLED=0 的工具链/镜像环境限制，不是测试失败，不触发终止条件；已在临时容器装 gcc + CGO_ENABLED=1 用等价方式重跑，34 包全绿、零 DATA RACE。

## 授权边界与开放项状态

- 本次授权排除：远程 Push、Tag、Release、GHCR、任何生产操作。
- 任务书 §18a 要求"核实生产 settings 和配置真实状态"，但**本次无生产授权**。处理方式：迁移矩阵用代码默认值 + 测试配置取证；生产实测项标 `WAITING_FOR_USER_DECISION`，不编造生产状态。此项不构成终止条件（任务书 §0.2 已预置该状态）。
- 四个开放项：①新旧字段冲突优先级（18a 冻结报告定）②生产真实状态（WAITING）③B20 旧测试覆盖（18a 取证）④性能外部环境（阶段 11 前确认，当前无多 Registry 对照环境，预判 BLOCKED）。

## 阶段 0 结论

基线成立：代码基线 3.5.17 与冻结文档一致，1.6 未改，关键代码事实与设计文档逐条吻合。未发现"1.6 权威规则与实际代码未裁定冲突"——B 默认 ON 是设计文档 §2.2/§2.3 已明确裁定的行为变更，不是未裁定项。可进入 18a。
