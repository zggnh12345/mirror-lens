# Findings — 3.5.25 实施复核

## Finding 来源
- 复核：deleg_bcd6e1aa（CHANGES_REQUIRED），基于集成 HEAD `410f56c`。
- 修复提交：见下。

## F-REV-01（P1）IPv6 裸地址被误判端口
- 状态：GREEN_RETESTED → REVIEWED（deleg_689af327 PASS）
- 证据：api.go 用 strings.Contains(":") + net.SplitHostPort 处理 `[fd00::1]` 报 missing port。
- 修复：抽取 parseEndpointAddress，统一用 endpoint.ParseOrigin（strict_fields.go）；POST/PATCH 共用。
- 测试：TestParseEndpointAddressIPv6（覆盖 `[fd00::1]`、`[fd00::1]:5000`、域名、域名:端口）。

## F-REV-02（P2）缺结构化 API 错误对象
- 状态：GREEN_RETESTED → REVIEWED（deleg_689af327 PASS）
- 修复：TLSError.Error() 输出 JSON（code/stage/message/detail/retryable/tls_applicable/cause_type），cause 经 Unwrap 保留给内部日志；errStr 对 TLSError 不截断；UI 先 JSON.parse 再按 code 分类，正则仅兜底非 TLS 错误。不改 DB schema。
- 测试：TestClassifyVerifyErrorPreservesType 按 JSON 断言 code。

## F-REV-03（P2）后续错误覆盖 TLS 错误
- 状态：RED_REPRODUCED → FIXED → GREEN_RETESTED（待 REVIEWED）
- 二次复核 deleg_689af327 发现 docker.go:178 漏改；已补 `if res.Error == ""` 保护并新增静态守卫测试 TestProbeErrorNeverOverwritesTLSError。
- 修复：docker.go/github.go 所有后续 `res.Error =` 赋值加 `if res.Error == ""` 保护。

## F-REV-04（P3）name 空值被接受
- 状态：GREEN_RETESTED → REVIEWED（deleg_689af327 PASS）
- 修复：addSource 对空 name 返回 400。
- 测试：TestAddSourceNameRequired。

## F-REV-05（P3）config PUT 绕过 fail-fast
- 状态：GREEN_RETESTED → REVIEWED（deleg_689af327 PASS）
- 修复：config.RequireHostProbeFields 导出；readConfigForUpdate 写盘前调用，缺字段拒绝写入。
- 测试：TestRequireHostProbeFieldsExported；受影响的面板配置测试 fixture 已补齐必需字段。
