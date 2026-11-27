# 3.5.24 基线证据（G0）

- 取证时间: 2026-11-27T03:56:19.473987+00:00
- 基线 commit: 84b013d23f0c11eaf86de9d5e776789467cb4192
- tag: v3.5.24 → 84b013d（远程/生产/基线）
- 任务书 commit: 5020ec557300a0dbdc4e6a1a2834d2b938da0f90
- 任务书 blob: 02bff3b3f4851be612cf9f883bbf616da3a29b6a
- Go: go1.27.3 linux/amd64（golang:1.27.3 容器，因宿主无 Go）
- 代码来源: git archive 84b013d（无 .git），docker cp 进入容器 /work

## Git 取证命令

```bash
git fetch --all --tags --prune
git status --short --branch
git branch --show-current
git rev-parse HEAD
git show -s --format=fuller HEAD
git log --oneline --decorate -30
git show-ref --tags
git diff --check
```

## 基线测试命令与结果

| 命令 | 日志 | 结果 |
|---|---|---|
| go version | baseline-version.log | go1.27.3 |
| go env GOMOD | baseline-gomod.log | /work/go.mod |
| go build ./... | baseline-build.log | PASS（rc=0） |
| go vet ./... | baseline-vet.log | PASS（rc=0） |
| go test ./... | baseline-test.log | FAIL（见 BASELINE_FAILURE） |
| go test -race ./... | baseline-race.log | PASS（rc=0，零 DATA RACE；internal/unified 在 race 下 PASS） |

## BASELINE_FAILURE（历史失败，不属于 3.5.25 新增回归）

- 包: internal/unified
- 测试: TestT52ReserveRCommitRace
- 现象: budget_test.go:52 成功预留=17，want 16（并发计数波动）
- 处理: 按任务书冻结口径记为 BASELINE_FAILURE，不修改基线代码、不调弱断言、不混入 3.5.25 新增失败；G4 必须据此区分新增回归与历史问题。注意：同一测试在 `go test -race ./...` 下 PASS（baseline-race.log），说明是并发计数偶发波动，非确定性失败。
