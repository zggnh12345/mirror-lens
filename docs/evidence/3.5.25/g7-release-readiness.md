# G7 Release Readiness（3.5.25，隔离实施分支）

## 结论
- 代码实施与本地门禁：**READY**。
- 真实环境验收：**NOT READY**（G5 BLOCKED_REAL_ENV）。
- 不得据此发布生产；发布前须补 G5 真实端点与性能对照。

## 已完成
- G0–G4 全 PASS；5 项独立复核 Finding 全 CLOSED。
- 新增/修正测试覆盖 API 契约、配置 fail-fast、Host 解析两路径一致、事务回滚、TLS 错误保真四码与行为级优先级。
- 全量门禁在最终代码 SHA 上：build/vet/test(-count=1)/race(-count=1)/gofmt/diff-check 全绿。

## 阻塞与缺口
- G5：SparkCR/SUNBALCONY 真实 SourceEndpoint、NAS/验收权限缺失。
- G6：完整 §17.1 真实源站对照未做；本地基准只覆盖 DB/事务，不覆盖 gateway 缓存门槛。

## 发布前置条件
1. 提供两个 Source 的脱敏端点与验收实例。
2. 独立执行 §9.6 全链路 11 项 × 3 轮并归档。
3. 在真实源站重跑 §17.1 性能对照并满足双门槛。
