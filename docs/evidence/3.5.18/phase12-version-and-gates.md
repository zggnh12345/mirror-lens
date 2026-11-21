# 3.5.18 版本更新与门禁验证（2026-11-21）

## 版本更新

- `internal/version/version.go:13`：`const Version = "3.5.17"` → `"3.5.18"`（程序版本唯一来源）。
- `internal/store/schema/schema.go:19` 的 `const Version = "3.5.17"` **未改**——那是 Schema 版本，
  3.5.18 明确不升级 Schema（CanonicalSchemaHash 冻结），保持 3.5.17 正确。
- 散落在代码注释里的「3.5.17 XXX」是功能来源标注，非版本号，保持原样。
- `bash scripts/check-version.sh`（无 tag 模式）输出 `3.5.18`，RC=0；
  若打 `v3.5.18` tag 将与 Go 常量一致（但本轮**不打 tag**，tag 未授权）。

## 门禁验证（版本更新后重跑，全部实测）

| 项 | 命令 | 结果 |
|---|---|---|
| 构建 | `go build ./...` | RC=0 |
| 静态检查 | `go vet ./...` | RC=0 |
| 全量测试 | `go test ./... -count=1` | RC=0 |
| 全量竞态 | `go test -race ./... -count=1` | RC=0 |
| 版本校验 | `bash scripts/check-version.sh` | 输出 3.5.18，RC=0 |

## A/B 独立与嵌套专项（显式执行）

| 专项 | 命令 | 结果 |
|---|---|---|
| A 层星源竞速 | `go test ./internal/gateway/ -run TestRace -count=1` | ok 3.867s |
| B 层跨站竞速 | `go test ./internal/gateway/ -run 'TestReplica\|TestCrossFlight' -count=1` | ok 2.202s |
| 嵌套 A+B | `go test -race ./internal/unified/ -count=1` | ok 1.546s |
| P1/P2 定向 race 复查 | `go test -race ./internal/gateway/ -run '<P1/P2 三测试>' -count=5` | ok 5.888s |

## 结构性文档修订（本批一起提交）

- §15.3 性能枚举 REV-1（`PERFORMANCE_NOT_MET`），正式授权修订，
  原 FROZEN 版本+哈希链见 `phase12-freeze-rev1-record.md`，
  冻结稿与校验清单均保留原值与修订值双记录。
- 主报告 HEAD 陈旧标注、18a 降级为「代码完成/独立复核待确认」、
  性能行改 REV-1 枚举并标「待 A10 重测定稿」、终态多维枚举化、
  §6 历史编号歧义澄清、A10 初判更正。
- 18g 台账性能行从 `EXTERNAL_BLOCKED` 改为 `PERFORMANCE_NOT_MET`（已实测 R1/R2）。

## 尚未关闭（后续步骤）

- A10 按修复后代码重跑（并发 8/3/1 最全口径）——执行顺序第6步。
- A10 后定稿主报告性能数据与终态——第7步。
