# 跨协议缓存回归基线（批次 2）

基线版本：v3.2.0（commit 3056fad）。本文件记录**当前**测试实际覆盖了哪些缓存路径，
不假设测试套件已经完备。P1 的 Schema/摘要改动以此为对照。

## 1. 缓存键与身份

| 协议 | 路由 | 缓存键 | 内容身份 | 命中判定 |
| --- | --- | --- | --- | --- |
| Docker | `/v2/<name>/manifests/<ref>` | `docker-manifest:<name>:<ref>` | manifest 原始字节 sha256 | `fresh()`：TTL 内直接命中，过期后按 ETag/Last-Modified 重新验证 |
| Docker | `/v2/<name>/blobs/<digest>` | `docker:<digest>` | digest 即 sha256（不可变） | 只认磁盘上按 sha256 命名的 blob |
| Release | `/gh/<o>/<r>/releases/download/<tag>/<file>` | `gh:<o>/<r>/releases/download/<tag>/<file>` | 内容 sha256（无可信摘要） | TTL（Release 24h / Archive 1h），过期重新验证 |
| Raw | `/raw/<o>/<r>/<ref>/<path>` | `raw:<o>/<r>/<ref>/<path>` | 内容 sha256 | 可变 ref（分支）TTL 5m；40 位 commit 视为不可变 |
| HTTP | `/http/<完整 URL>` | `http:<完整 URL>` | 内容 sha256 | TTL + 重新验证；带 Range/HEAD 的请求直通不落盘 |

所有协议共用同一份内容寻址存储：`blobs/<sha 前两位>/<sha256>`，索引在 `cache_entries`。
因此**同一内容多协议共用一份磁盘文件**，但缓存条目（键）按协议分开。

## 2. 回归矩阵（现状）

图例：✅ 有测试覆盖；⚠️ 有测试但只覆盖部分语义；❌ 无测试（本次补）。

| 场景 | Docker | Release | Raw | HTTP |
| --- | --- | --- | --- | --- |
| 正常下载 | ✅ `TestRegistryBlobStreamAndHit`、`TestRegistryManifestMissThenHit` | ✅ `TestGHReleaseThroughMirror` | ✅ `TestMissThenHit` | ✅ `TestHTTPStreamMissThenHit` |
| 缓存命中 | ✅ 同上 | ⚠️ 仅 archive TTL 用例 `TestGHArchiveTTL` | ✅ `TestMissThenHit` | ✅ `TestHTTPStreamMissThenHit` |
| 缓存过期 | ⚠️ manifest 过期靠 `TestRegistryManifestMissThenHit` 间接覆盖 | ✅ `TestGHArchiveTTL` | ✅ `TestRevalidate304`、`TestUpstreamUpdateRefreshesCache` | ✅ `TestHTTPRevalidate304`、`TestHTTPUpstreamChanged` |
| 客户端取消 | ⚠️ `TestClientGoneNotCounted`（计数语义） | ❌ | ⚠️ 同上 | ⚠️ `TestHTTPResumeAfterDisconnect` |
| 上游断连/截断 | ⚠️ `TestOfficialParallelRetriesTruncatedSegment` | ❌ | ✅ `TestTruncatedUpstreamCountedAsFailure` | ✅ `TestStreamBodyReportsFailureOnTruncation` |
| 429 | ✅ `TestFailoverRecords429` | ⚠️ 同调度层用例 | ✅ `TestPassThroughAllSources429KeepsRetryAfter` | ✅ `TestPassThroughRecords429AndFailsOver` |
| 5xx | ✅ `TestMissFollowedByFaultIsNot404` | ⚠️ 同调度层用例 | ✅ `TestMissFollowedByFaultIsNot404` | ⚠️ 同调度层用例 |
| 多源切换 | ✅ `TestSchedulerFailover` | ✅ `TestGHReleaseThroughMirror` | ✅ `TestMirrorMissFailsOverWithoutCooling` | ✅ `TestPassThroughMissFailsOver` |
| Range | ✅ `TestRegistryBlobRangePassthrough`、`TestRegistryBlobRangeSynthesizesDigest` | ❌ 未做 Range 实测（批次 4） | ⚠️ 无独立用例 | ✅ `TestHTTPRangePassThrough`、`TestHTTPRangeFromCache` |
| 内容校验 | ✅ `TestRegistryBlobDigestMismatchNotCached`、`TestRegistryVerifyState` | ⚠️ `TestDigestMismatchOnMirrorNeverCached`（镜像摘要） | ⚠️ `TestDigestMismatchEverywhereNotCached` | ⚠️ `TestLyingSelfDigestRejected` |
| 缓存提交失败 | ❌（本次补 `TestCacheCommitFailureServesWithoutIndex`） | ❌ | ❌ | ❌ |
| 服务重启恢复 | ❌（本次补 `TestCacheSurvivesRestart`） | ❌ | ❌ | ❌ |
| 索引在、文件丢失 | ✅ `TestReconcileDropsOrphanBlob`（janitor 侧） | ❌（本次补 `TestCacheMissingBlobDropsIndexAndRefetches`） | ❌ 同左 | ❌ 同左 |
| `.part` 恢复/清理 | ✅ `TestSweepPartialDropsOrphanPart`、`TestSweepPartialKeepsFreshPair` | ❌ | ❌ | ❌ |

## 3. 已知缺口（P1 前必须知道的影响面）

1. **提交失败后的可见性**：`cache.Commit` 失败只写日志、丢 `.part`，客户端已经收到完整字节。
   没有测试断言「响应仍成功、索引不出现、下次重试能成功」。本次已补。
2. **重启恢复**：索引与 blob 分属 SQLite 与文件系统，重启后命中依赖两者一致；
   没有端到端用例。本次已补。
3. **索引指向缺失文件**：`serveCached` 会删索引并返回 502（客户端需重试一次），
   只有 janitor 侧的孤儿清理测试，没有请求路径用例。本次已补。
4. **Release 协议**：缓存命中、过期、截断、Range 均无独立用例——P1 改动会直接落在
   `ghadapter` 与 `stream.fillStream`，批次 6/7 必须同时补这些用例。
5. **客户端取消后的残留**：只验证了计数，没有验证 `.part` 与 blob 锁的清理。

## 4. 运行方式

```bash
docker run --rm -v <repo>:/src -w /src -e GOPROXY=https://goproxy.cn,direct \
  golang:1.27 go test -race ./internal/gateway ./internal/cache ./internal/janitor ./internal/store
```
