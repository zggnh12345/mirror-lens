# 设计冻结（G1）

- 冻结时间基准：任务书 commit 5020ec557300a0dbdc4e6a1a2834d2b938da0f90
- 任务书：只读快照位于 worktree 外，执行依据由 execution-state.md 的 TASKBOOK_COMMIT_SHA/BLOB_SHA 锁定；同一 RUN_ID 固定快照
- BASE_SHA：84b013d23f0c11eaf86de9d5e776789467cb4192（v3.5.24，唯一基线）
- 实施分支：3.5.25-fix（从 BASE_SHA 创建，G1 冻结后所有实现 Agent 从 FREEZE_SHA 建立分支）
- FREEZE_SHA：本目录文件提交后生成的 commit SHA（见 execution-state.md）

## 冻结范围（任务书 5020ec5）

- F2：保留 HTTP/HTTPS 下拉框，地址栏只填 Host/Host:Port，含 scheme 输入 400；新 API endpoint.scheme+endpoint.address；旧字段 400 legacy_field_not_supported，未知字段 400 unknown_field。
- F1：取消归属必选，删除能力强制推断（选 docker.io 强置 CapYes、探测纠不回）；归属与能力解耦。
- F4：删除全部 ML_HOSTPROBE_*；probe.host_probe_repos/interval_sec(21600)/scan_sec(30) 正式配置；已有配置缺字段 fail-fast；空 map=无 Foreign Host。
- F1b：统一三态 Verdict；*.pkg.dev 受限通配（精确优先、registryhost.Match 严格 Artifact Registry 语义、统一解析器供 reconcileRepos 与 jobs 共用、无匹配不兜底）；reconcileRepos 无匹配时 ResetHostCaps 重置旧结论为 Unknown；jobs 跳过并记录。
- F3：仅删失效 See §2.3 warning 文案，不删协议下拉框。
- TLS：仅修错误保真链 verifyChain→tlsInfo→docker.go/github.go→结构化 API 错误码→UI 分类；阶段 E 已修行为只回归。
- 事务：AddSourceHost 四类事务原子化 + base_url 竞态修复（事务内读 Endpoint 再算 policy）；不建第二写路径；不新增 DB Schema/改凭据格式/AEAD AAD/Cache Epoch。
- 严格字段校验仅 POST/PATCH Source 与 Host POST；extra_realms 省略=空数组、null/非数组/超3/非法元素 400；嵌套错误返回 endpoint.unknown_endpoint_field。
- D2：HOLD_CURRENT_404_SEMANTICS，待真实协议取证；不阻塞无关实施。
- 附录 A：多 Endpoint 仅记录未来方向，不属 3.5.25 实施，不改 API/Schema/配置/Go/UI。

## 数据模型边界

单 Endpoint 契约不变：不增加 endpoints[]、ipv4、ipv6、secondary_address 字段或子表；既有 Source ID/凭据/历史业务关系在 3.5.25 中不变。
