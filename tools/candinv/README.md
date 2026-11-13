# S1 只读盘点工具（candinv）

用途：在不修改 `internal/`、不写数据库的前提下，调用实际 `store.SourceCandidates`
输出候选集基线，并导出 89 条空 Host 的分类/状态明细。

约束：

- 数据库只读打开：`mode=ro&_pragma=query_only(1)`。
- 只调用 `SourceCandidates` 与 SELECT；不迁移、不 UPDATE/INSERT/DELETE、不 VACUUM、不启动调度或 Probe。
- 退出前校验数据库 sha256 前后一致，不一致直接失败。
- 默认构建不包含：文件带 `//go:build s1inv`，`go build ./...` 不会编译。

## 文件

- `s1_baseline_test.go`：盘点逻辑，声明为 `package store`，通过 overlay 注入。
- `overlay.json`：把 `/src/internal/store/zz_s1_baseline_test.go` 映射到本文件。

## 用法

1. 生成一致快照（源库只读，避免直接跑在生产库上）：

```sh
docker run --rm -i \
  -v /path/to/prod/data:/src:ro \
  -v /path/to/scratch:/out \
  python:3.12-alpine python - <<'PY'
import sqlite3
s = sqlite3.connect('file:/src/mirror-lens.db?mode=ro', uri=True)
d = sqlite3.connect('/out/snap.db')
s.backup(d); d.execute('pragma journal_mode=delete'); d.close(); s.close()
PY
sha256sum /path/to/scratch/snap.db
```

2. 运行盘点（路径按容器内挂载点调整）：

```sh
docker run --rm \
  -v $PWD:/src \
  -v /path/to/scratch:/out \
  -v ml8-gomod:/go/pkg/mod \
  -w /src \
  -e S1_DB=/out/snap.db \
  -e S1_OUT=/out/s1-baseline.json \
  -e GOTOOLCHAIN=local \
  golang:1.27-alpine \
  go test -tags s1inv -overlay /src/tools/candinv/overlay.json \
  -run TestS1ReadOnlyBaseline -v ./internal/store
```

## 输出

`S1_OUT` 指定的 JSON，字段固定、ID 升序，便于迁移前后 diff：

- 快照信息：`db_path`、`db_size`、`db_sha256_before/after`
- 候选池：`docker_candidates`、`ghcr_candidates` 及计数
- 汇总：`status_count`、`enabled_count`、`group_status`（分类/状态）
- 明细：`rows`（91 条，含 `repo_host`、四个能力字段、`planned_identity`）

`planned_identity` 是**迁移计划分类**，不是库中已有字段；3.5.4 之前库中没有
`registry_identity_state` 列。
