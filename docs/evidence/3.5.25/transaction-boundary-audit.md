# 事务边界审计（G0，基线 84b013d）

## AddSourceHost（internal/store/hostcaps.go:392-487）

base_url 在事务**外**读取：:397-403 `SELECT base_url`，随后 :404-405 `sourcePlainOrigin`/`normalizePolicyRealms` 计算 Realm policy；`BEGIN IMMEDIATE` 在 :409-420（beginImmediate :154-164），status 检查 :421-430。事务内写：source_host_caps(:431-435)、source_realm_policy(:437-445)、重置 caps(:448-452)、删 source_health_scope(:453-456)、更新 registry_host_recovery(:457-461)、清 source_health(:462-470)、插入 recovery(:471-478)、事件(:479-481)，COMMIT :482-485。**单事务成立，但 base_url 读取在事务外**。

## SetBaseURL（internal/store/host.go:192-205）

经 withSourceLive(:210-232，db.Begin :211、Commit :231) 单事务：UPDATE base_url(:194)、policy_gen+1(:197)、DELETE source_realm_policy(:201)、recordPolicyChangeTx(:204)。对照 SetRepoHost(:78-85) 在同事务内读 base_url(:83-85)。

## 新增 Source（internal/store/store.go:458-610）

`s.db.Begin`(:518) → INSERT sources(:523-538)、recovery(:542-553)、realm policy(:563-573)、caps(:579-606) → tx.Commit(:607)。API 调用点 api.go:698-704。

## 编辑 Source（internal/api/api.go:919-978）

API 请求本身**不是单事务**：GetSource(:922) 后 SetBaseURL(:962)、TouchProbe0(:966)、SetRepoHost(:973) 各开事务，RunSource(:970) 为异步 goroutine。SetBaseURL/SetRepoHost 内部分别单事务。

## 探测写回（internal/store/commit.go:65-121）

唯一写回入口 CommitHostProbe：BEGIN IMMEDIATE(:102) → commitHostProbeTx(:112) → COMMIT(:116)；事务内校验源/归档/caps/策略代次(:123-165)与 gen/摘要(:167-180)；Runner 调用点 hostprobe.go:490。第二条写路径 SetHostCaps 已标 Deprecated、仅供历史测试造数据(host.go:234-238)；NoteProbeScoped 对 registry family 直接拒绝(sources.go:111-120)。

## 竞态点事实与冻结要求

AddSourceHost 的 Realm policy 依赖事务外 base_url(hostcaps.go:398→:410)，与 PATCH 的 SetBaseURL(api.go:962 / host.go:194)可并发交错——旧 Endpoint 的 policy 可能被写入新状态。任务书 :881-889 记为待修复竞态；:534-546 冻结：BEGIN IMMEDIATE 后在事务内读当前 Source Endpoint 再计算 policy，一次提交；不另建第二写路径。
