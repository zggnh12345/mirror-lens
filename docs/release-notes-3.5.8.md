# Mirror Lens 3.5.8 发布说明

**版本：** 3.5.8  
**基线：** 3.5.7（`e5f5bdd`）  
**Schema：** 不变，仍为 3.5.7 Canonical Schema；不运行 `db-migrate`。

## 范围

第一批公开 Registry 多源调度：

- `gcr.io`
- `registry.k8s.io`
- `<location>-docker.pkg.dev`
- `quay.io`
- `mcr.microsoft.com`
- `registry.gitlab.com`
- `public.ecr.aws`
- `cgr.dev`

`lscr.io` 不启用；其 Realm 已修正，但仍待独立验收。

## 行为变化

- `registry.multi_origin_hosts` 是唯一范围来源；空列表表示没有任何 Host 使用多源。
- 删除 `registry.multi_origin` 总开关；不再读取 `ML_REGISTRY_MULTI_ORIGIN`。
- 列表内 Host 的候选池为 `registry:<host>`，候选优先，失败回官方源。
- 每个 Host 独立调度、独立健康状态；没有 Source 的 Host 行为与 3.5.7 一致。
- `disabled_hosts` 关闭访问，覆盖 Manifest、Blob、HEAD、referrers 和缓存命中。
- 未知 Host 前缀不再静默按 Docker Hub 仓库处理。
- 公开匿名 Realm 按实际 Challenge 放行；MCR 无 Bearer 时不强行构造 Token。

## 升级

1. 停旧容器，完整备份数据库和 `secret.key`。
2. 用 3.5.8 镜像启动；数据库 Schema 仍为 3.5.7，不需要迁移。
3. 确认启动日志显示 `多源范围=8 禁用Host=0`（默认配置）。
4. 请求一个已配置 Source 的 Host，确认 `X-Ml-Source` 和缓存行为。

## 回滚

- 多源全部关闭：`multi_origin_hosts: []`，重启。
- 单个 Host 不使用多源：从列表移除；官方直连仍可用。
- 单个 Host 完全关闭：加入 `disabled_hosts`。
- 回滚旧版本：恢复 3.5.7 数据库副本和配对 `secret.key`，再启动 3.5.7；不要用旧二进制直接打开新库。

## 验证

- `go test ./...`、`go vet ./...`、全量 `-race` 通过。
- 8 个公开 Host 的 Manifest/Index 与 config blob digest 校验通过。
- 真实进程、真实配置文件、SQLite Source 的命中与缓存复用通过。
- 详细记录见 `docs/3.5.8-registry-phase1-design.md` 第 5、10 节。

## 不包含

- `lscr.io` 多源调度。
- 私有/受限仓库凭据闭环。
- 同 Host 多 Source 正式验收。
- Manifest 星标竞速、Blob 竞速。
- Host 级 UI 管理页。
