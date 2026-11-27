# 文件所有权矩阵（G0/G1）

基线 84b013d；本矩阵锁定 Agent 1–4 的写入边界，同一文件同一时间只有一个写入 Owner。

| Agent | 独占可写文件 | 只读依赖 | 禁止 |
|---|---|---|---|
| Agent 1（API/UI） | `internal/api/api.go`（addSource/patchSource DTO 与 handler）、`internal/api/hosts.go`（Host DTO）、`internal/api/web/index.html`（表单与错误分类） | `internal/endpoint/endpoint.go`、`internal/store/store.go`、`docs/3.5.25-unattended-delivery.md` | 不改 Store 事务、不改探测语义、不保留旧字段 alias |
| Agent 2（Host 配置/探测） | `internal/config/config.go`、`config.example.yaml`、`internal/runner/hostprobe.go`、`internal/registryhost/registryhost.go` | `docs/3.5.8-registry-phase1-design.md`、`internal/store/hostcaps.go` | 不改 DB Schema、不建第二套 Host 能力存储 |
| Agent 3（Store/事务） | `internal/store/hostcaps.go`、`internal/store/host.go`、`internal/store/store.go`、`internal/store/commit.go` | `internal/api/api.go`（接口契约） | 不改 API 契约、不自造第二写路径 |
| Agent 4（TLS 错误保真） | `internal/probe/probe.go`、`internal/probe/docker.go`、`internal/probe/github.go`、`internal/api/web/index.html` 的错误码段 | `internal/endpoint/endpoint.go`、`internal/safehttp` | 不重写阶段 E 已修的 SNI/SAN/IPv6/证书链、不放宽 TLS 验证 |
| Agent 5（QA） | 测试文件、`docs/evidence/3.5.25/` | 全部源码 | 不改生产代码、不删失败测试或调弱断言 |
| Agent 6（Reviewer） | 只提交 findings 与复核报告 | 全部 | 不改代码、不改分支 |
| Agent 7（真实验收） | 验收脚本与证据目录 | 生产环境只读 | 无单独授权不改生产 config/库/服务 |

**共享文件裁定**：`internal/api/web/index.html` 由 Agent 1 拥有表单部分、Agent 4 拥有错误分类部分，二者按 §13.3.1 由 Agent 0 指定写入时段串行修改，禁止并行编辑。`internal/probe/probe.go` 由 Agent 4 独占。冲突时由 Agent 0 按依赖顺序逐支集成。

**任务书**：快照保存在 worktree 外（`taskbook-3.5.25-<RUN_ID>.md`），只读，不属于任何 Agent 可写范围。
