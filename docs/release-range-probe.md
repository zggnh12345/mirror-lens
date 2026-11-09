# Release Range 能力实测（P2-1）

结论：GitHub Release 资产与 raw 文件都支持字节范围请求，Release 可以进入分片并行，
但准入必须同时满足「有可信摘要」与「预检拿到 206」。少一条就退回单流。

## 实测（2026-11-09，容器内 curl，带 Range 头）

| 目标 | 结果 |
| --- | --- |
| `github.com/cli/cli/releases/download/v2.62.0/gh_2.62.0_linux_amd64.tar.gz` | 206，`Content-Range` 正确 |
| `github.com/ollama/ollama/releases/download/v0.5.7/ollama-linux-amd64.tgz`（从 1MiB 起） | 206 |
| `github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64` | 206 |
| `raw.githubusercontent.com/torvalds/linux/master/README` | 206，`Accept-Ranges: bytes` |

Release 下载会 302 到 `objects.githubusercontent.com`，跳转后仍保留 Range 语义；
预检用的是最终响应，所以判断依据是实际 206，而不是首跳头。

## 准入条件（代码位置）

`internal/gateway/parallel.go: tryParallel`

1. 并行总开关打开、worker ≥ 2、分片大小 > 0；
2. `plan.ExpectSHA != ""` —— 没有可信摘要就没有拼装资格；
3. `plan.Stream && plan.Cacheable`、磁盘够、客户端没自带 `Range`（续传不掺和）；
4. 没有未完成的 `.part`；
5. 至少两台源在 `parPreflight` 里返回 206 且总长一致。

第 5 条就是 Range 能力的运行时验证：不支持 Range 的源会被预检剔除，
不会出现「一半源给整文件、一半给分片」的混流。

## Release 摘要来源

- 配置了 GitHub Token：`Release Asset API digest` → 其次 `SHA256SUMS`（P1-2）；
- 未配置 Token：完全不查 API，Release 没有可信摘要，因此**不进并行**，走单流。
  这是刻意的：没有摘要时拼装无法验证，速度让位于正确性。
