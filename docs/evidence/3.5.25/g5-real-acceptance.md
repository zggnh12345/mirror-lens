# G5 真实环境验收报告（3.5.25，2026-11-27）

## 授权与边界
- 用户明确授权"授权"后执行：只读导出生产库 sources 表 name/base_url（不碰凭据表，副本查完即删）；新建隔离验收容器（不改生产容器/配置/数据库）。
- 生产仍为 3.5.24，未触碰。两个验收容器（ml3525-verify / ml3525-verify-fakeip）验收后已删除。

## 两个 Source 的真实 Endpoint（脱敏，来自生产库 sources 表，只读导出）
- SparkCR：name=docker.sparkcr.cn, site_name=SparkCR, base_url=https://docker.sparkcr.cn（+443），生产状态 usable
- SUNBALCONY：name=dockerproxy.cool, site_name=SUNBALCONY, base_url=https://dockerproxy.cool（+443），生产状态 dead（"连续 3 次探测失败"）

## 验收环境
- 镜像：从 3.5.25-fix HEAD a30d289 现场 docker build（golang:1.27-alpine 编译 + alpine:3.22 运行，ca-certificates 20261020-r0），commit 自证=a30d2893a7fa31a8651d91a387a3174882b4abb1。
- 网络：traefik（与生产同类）。唯一对照变量 = DNS。
  - 验收容器 A：--dns 10.1.1.4 --dns 223.5.5.5（真实 IP，绕开 fakeip 前门）
  - 对照容器 B：默认 DNS（127.0.0.11 → 10.1.1.3 前门，fakeip）
- 两容器均 setup 一次性测试账号后通过 API 添加两个 Source 并触发 tier2/tier3 探测。

## §9.2 全链路取证（Host 侧 openssl + 容器内 wget + API JSON）
- DNS A/AAAA：SparkCR 前门/SmartDNS/公共均返回真实 IP（122.228.243.x + 240e:f7::）；SUNBALCONY 前门返回 fakeip（198.18.0.43 / fc00::2d），SmartDNS/公共返回真实 IP（43.169.13.123 / 240d:c010:77:4::c0）。
- 证书链/Subject/Issuer/SAN/有效期（openssl 实测）：
  - SparkCR：CN=*.sparkcr.cn, Issuer=Let's Encrypt YR1, SAN=*.sparkcr.cn/sparkcr.cn, 2026-11-16~2027-02-14（cert_days=79），系统根验证 OK。
  - SUNBALCONY：CN=dockerproxy.cool, Issuer=TrustAsia DV TLS RSA CA 2025, SAN=dockerproxy.cool, 2026-09-15~2026-12-13（cert_days=16），系统根验证 OK。
- 容器内真实 IP DNS 下 HTTPS：SparkCR /v2/→401（正常 Registry 挑战）、SUNBALCONY /v2/→200、raw.githubusercontent→200。
- API JSON：g5-two-sources-full.json（验收容器 A）、g5-fakeip-control-full.json（对照容器 B）、g5-api-3.5.25-sources.json。
- 三轮重复：g5-three-rounds.json（A 容器三轮，结果稳定）。

## 结论：根因定位（A/B 对照，唯一变量 DNS）

| Source | 容器 A（真实 IP DNS） | 容器 B（fakeip DNS） | 生产 3.5.24（fakeip） |
|---|---|---|---|
| SparkCR | usable, tls_ok=true, docker_cap=1 | usable, tls_ok=true | usable |
| SUNBALCONY | **usable**, tls_ok=true, docker_digest_ok=1, github_cap=1 | degraded/pending, tls_ok=false, code=CONNECT_FAILED/TIMEOUT, stage=tls_dial | dead（连续 3 次失败） |

1. **SUNBALCONY 从全绿变"连接异常"的根因**：其域名经 sing-box fakeip 前门（10.1.1.3）解析为 198.18.0.43/fc00::2d（保留基准段），3.5.24/3.5.25 的 safehttp SSRF 防护在拨号层（Dialer.Control）识别该段为特殊用途并拒绝，导致 TCP/TLS dial 失败；换真实 IP DNS 后立即恢复 usable。这不是源站故障、不是证书问题、不是代码 bug。
2. **SparkCR 从未因 fakeip 失败**：国内域名解析为真实公网 IP，fakeip 前门不干预，故各环境均 usable。其 degraded 来自 tier3 的深度探测：docker blob 重定向到第三方 CDN（cache-esa.cr.tongritech.com）取 blob 失败、github raw 返回 404——属源站自身能力/内容问题，且与 TLS 无关（tls_ok=true, cert_days=79）。
3. **不靠降级变绿**：全程未关闭 TLS 验证、未放宽 Guard/Pin；放行的是 DNS 给出的合法公网 IP。

## §9.6 通过条件逐项判定
- 真实证书/连接健康时状态恢复正确：PASS（SUNBALCONY 换真实 IP DNS 后 usable，三轮稳定）。
- 源站自身问题时 UI 准确报告唯一根因：PASS（SparkCR 精确报 "blob 下载失败: <CDN URL>" 与 "raw 文件返回 404"，tls_ok=true；不再混淆为证书错误）。
- 证书错误不与连接错误混淆：PASS（对照容器 B 错误为结构化 JSON code=CONNECT_FAILED/TIMEOUT/stage=tls_dial/cause_type=*net.OpError，非证书类）。
- 不通过跳过验证/放宽安全策略变绿：PASS。

## 结论状态
- SparkCR：PASS（usable，degraded 为源站自身 CDN/404 问题，非本版本缺陷，根因已明）。
- SUNBALCONY：PASS（真实根因=fakeip DNS，非源站/证书/代码问题；换真实 IP DNS 后恢复，三轮稳定）。
- G5 从 BLOCKED_REAL_ENV 推进为真实根因定位完成 + 通过条件验证 PASS。

## 残余
- fakeip 前门是整个 NAS 的网络布局，是否改为真实 IP DNS / 是否给该容器单独配 DNS，属生产网络配置决策，需另行授权；本报告只取证，不改生产。
- cert_days=16（SUNBALCONY 证书 2026-12-13 到期）临近到期，属源站运维事项，记录备考。
