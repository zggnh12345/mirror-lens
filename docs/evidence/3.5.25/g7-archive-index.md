# G7 证据归档索引（3.5.25）

- 最终执行 HEAD：`b27d83ee8eac2c992cb3fea34154aeda5761f7d9`
- 门禁锚定代码 SHA：`0caf9da177e0cf9988ebcd0f10f43c5fc2847802`（此后仅增 docs/evidence 与新 benchmark，代码树中 benchmark 为纯测试文件）
- 基线 SHA：`84b013d23f0c11eaf86de9d5e776789467cb4192`
- 任务书：`5020ec557300a0dbdc4e6a1a2834d2b938da0f90` / blob `02bff3b3f4851be612cf9f883bbf616da3a29b6a`

## 文件清单（事实）
- `baseline-build.log`
- `baseline-evidence.md`
- `baseline-gomod.log`
- `baseline-race.log`
- `baseline-test.log`
- `baseline-version.log`
- `baseline-vet.log`
- `compatibility-inventory.md`
- `decisions.md`
- `design-freeze.md`
- `execution-state.md`
- `execution.lock`
- `file-ownership-matrix.md`
- `findings.md`
- `g4-build.log`
- `g4-changed-go-files.txt`
- `g4-diff-check.log`
- `g4-final-sha.txt`
- `g4-gofmt.log`
- `g4-race.log`
- `g4-test.log`
- `g4-vet.log`
- `g6-comparison.md`
- `g6-methodology.md`
- `g6-raw-3.5.24-addsourcehost.txt`
- `g6-raw-3.5.24-store.txt`
- `g6-raw-3.5.25-addsourcehost.txt`
- `g6-raw-3.5.25-store.txt`
- `host-source-matrix.md`
- `tls-root-cause-investigation.md`
- `transaction-boundary-audit.md`
- `transitional-state-inventory.md`

## 阶段状态（事实 / 目标 / 未执行 / 阻塞）

- G0 基线取证：PASS（事实）
- G1 设计冻结：PASS（事实）
- G2 实施：PASS（事实，四个 Agent 领域 + 集成修正）
- G3 独立复核：PASS（5 Finding 全 CLOSED）
- G4 全量测试：PASS（证据链经两轮复核关闭）
- G5 真实环境验收：**BLOCKED_REAL_ENV**（无端点/无 NAS 权限，不得宣称完成）
- G6 性能对照：**PARTIAL**（本地隔离基准 PASS；真实源站对照 BLOCKED_REAL_ENV；缓存门槛 NOT_RUN）
- G7 文档归档：本文件（事实）
- G8 清理收尾：待执行（目标）

## 未执行 / 阻塞项（不得当作 PASS）

- SparkCR / SUNBALCONY 真实全链路 11 项证据：未执行（阻塞）
- 热/冷缓存 P95 门槛、并发 Pull 成功率：未执行（NOT_RUN，隔离基准未覆盖 gateway 缓存路径）
- 提交前/后进程退出崩溃注入：不可单测，标 NOT_RUN
- 真实证书“正确 SAN 通过”：依赖系统信任库，由 G5 真实环境覆盖（阻塞）
