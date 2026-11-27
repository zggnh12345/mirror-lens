# TLS 根因调查（G0，基线 84b013d）

## 错误传递链现状

- `internal/probe/probe.go:125-139` verifyChain(cs, host)：cs.PeerCertificates[1:] 作 Intermediates、系统根验证 leaf，x509.VerifyOptions{DNSName: host}(:134-137)，返回原始 error(:138)。
- `internal/probe/probe.go:147-196` tlsInfo：tlsEndpoint(base) 解析(:148,:216-220)，非 HTTPS 报错(:152-154)，host/port 用 ep.Host()/ep.Port()(:155)，拨号目标 net.JoinHostPort + 解析 IP(:163-170)，tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion TLS1.2}(:171-175)，握手错误返回(:178-181)，取 leaf 与 CertDays(:183-188)，末尾 `verr := verifyChain(cs, host); return verr == nil, days, nil`(:194-195)——**verifyChain 原始 error 被压成 bool，err 恒为 nil**。
- 调用点丢弃错误：`internal/probe/docker.go:94-101`（isPlainHTTP 分支 TLSOK=true :94-97；否则 `ok, days, _ := tlsInfo(...)` :99 写 res.TLSOK/res.CertDays :100）；`internal/probe/github.go:32-39` 同（:37 丢 error、:38 写结果）。

## API/UI 现状

API 错误为 HTTP 状态码 + `{"error": 字符串}`（api.go:615,:643,:650,:659,:664），**无结构化错误码**；UI `internal/api/web/index.html:738-740` errReason、:742-756 errHuman 正则分类，其中 :745 TLS handshake timeout、:746 x509|certificate|证书、:752 connection reset、:754 timeout、:755 refused、:756 兜底；技术详情 errchip :823；REG_TRANSPORT 映射 :555。

## 历史根因与当前结论

`docs/3.5.24-design-freeze.md:204-225` 记录 hostOf 返回完整 Authority、tlsInfo 二次拼 443 污染 ServerName/verifyChain DNSName；**84b013d 已修复**：ep.Host()/ep.Port()(probe.go:155,:163)与 tlsEndpoint(:216-220)。任务书 :595-607 将该项列为 commit 5518739 已修复、仅回归；:609-619 将剩余缺口定为错误保真链；:623-652 冻结错误码 TLS_CERT_UNTRUSTED/TLS_CERT_EXPIRED/TLS_HOSTNAME_MISMATCH/TLS_HANDSHAKE_FAILED/DNS_FAILED/CONNECT_FAILED/TIMEOUT/NETWORK_BLOCKED/HTTP_STATUS_ERROR/CONTENT_INVALID/TLS_NOT_APPLICABLE 及对象字段。

InsecureSkipVerify 仅用于专用诊断路径“先取证、后 verifyChain 手动验证”，业务 Transport 仍严格验证；不得据此把 Source 标健康（任务书 :698-699）。
