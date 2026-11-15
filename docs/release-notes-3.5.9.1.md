# Mirror Lens 3.5.9.1 发布说明

**版本：** 3.5.9.1
**基线：** 3.5.9（`a043b80`）
**Schema：** 不变；不运行 `db-migrate`。

## 修复

- 修复首页源列表的多能力 Registry 标签回归：`repo_host` 只表示主归属，不等于唯一能力。
- `docker.io` / `ghcr.io` 源现在按 `docker_cap`、`ghcr_cap` 同时输出 HUB/GHCR；双用途源不再丢失 GHCR。
- `repo_host=''` 仍然不推断 HUB/GHCR；Foreign Host 仍只输出自己的 Registry 标签，`RAW` 仍独立按 `github_cap` 输出。
- 新增 `TestPanelCapabilityTagsGeneral` 通用能力矩阵，覆盖双用途、单能力、空 Host、Foreign Host 和未探测场景，而不是只覆盖单个样本。

## 验证

- `go test ./... -count=1`
- `go test -race ./... -count=1`
- `go vet ./...`
- `git diff --check`
- 首页实际数据验证：双用途源显示 `HUB,GHCR,RAW`；空 Host 不显示 HUB/GHCR。
