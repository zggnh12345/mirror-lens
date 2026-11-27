# 冻结决策归档（G1，据任务书 5020ec5）

| 编号 | 决策 | 状态 | 依据 |
|---|---|---|---|
| D1 | F1 替代旧设计冻结稿“归属必选” | 已冻结 | 任务书 §2.3 为 G0 兼容普查章节；旧决策不属于本任务书 |
| D2 | Manifest 404 实际语义 | `HOLD_CURRENT_404_SEMANTICS`（唯一待取证） | §11.9/§12；保持 404/410 不判 VerdictNo；不阻塞无关实施；必须改语义则单独 BLOCKED |
| D3 | 生产 config 是否允许修改 | 已冻结：不允许 | 本任务不授权生产 config 变更，只在隔离环境验证 |
| D4 | 其他 Host 登记策略 | 已冻结 | 默认未登记不自动探测；API 关联、全局准入、探测仓库三条件分离（§7.3） |
| D5 | 删除 ML_HOSTPROBE_* 与 F4 单一配置源 | 已冻结 | 彻底删除环境变量；规范 Host 字段为唯一权威；已有配置缺字段启动报错人工补齐；显式空列表=无 Foreign Host |

## 其他冻结契约（非 D1–D5 但同等约束）

- D-API：POST/PATCH endpoint.scheme+endpoint.address；旧字段拒绝；strict 仅三端点。
- D-HOST：两步登记，POST /api/sources 成功后独立 POST /api/sources/{id}/hosts；primary_realm API 层非空校验；extra_realms 省略/[]/null 语义冻结。
- D-CONFIG：host_probe_repos 9 行映射、21600/30、Default() 与 config.example.yaml 一致、缺字段 fail-fast、*.pkg.dev 唯一受限通配键、配置键 Normalize 后落 Map、空键/冲突拒绝。
- D-RESOLVE：方案 B；精确优先；统一解析器；两场景不同动作；不兜底。
- D-STORE：四类事务单提交；AddSourceHost 事务内读 Endpoint；探测写回唯一入口 CommitHostProbe。
- D-F2：下拉框保留、地址栏无 scheme、含 scheme 400；D-F3 只改文案。
- D-MANIFEST：404/410 维持 D2 待取证。
- 附录 A：多 Endpoint 未来方向，不实施。
