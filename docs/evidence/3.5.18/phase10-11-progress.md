# 阶段10/11 实测报告（2026-11-20）

## 授权与边界

用户授权：按既定顺序执行阶段10/11，构建 3.5.18 候选 + 自建环境；终态按实测更新（覆盖 §12.5 裁定）。
约束：不打 tag、不推送、不建 Release/GHCR、不碰生产。本轮全部遵守。

## 测试方案（第0步，已冻结）

- 文档：`test-plan-phase10-11.md`（sha256 见同名 .sha256）。
- 环境：8 核 i3-12300T / 31G / Docker 28.5.2 overlay2 / Go 1.27.4，实测采集。
- 阈值：A10 P95 TTFB 相对改善 ≥50%、正式 10 轮，继承 1.6 §12.3.0，未改。

## 已完成（真实、可复核证据）

### 构建与镜像
- 候选 `mirror-lens:3.5.18-local`（ID 8339c8913086），`-version` 报 commit=da8eba5 → 证明是 3.5.18 代码。`version.go` 仍显示 3.5.17 是刻意保留至正式 tag 流程（本轮不打 tag）。
- 基线 `mirror-lens:3.5.17`（ID 42f783f3b0c2）。

### 真实 Registry 数据集
- 两个 `registry:2` 容器（ml318-reg-a/b），命名卷，走真实 Distribution API + 磁盘 IO。
- 各推送 5 个多层 manifest（t1–t5，2/3/4 层）+ latest，digest 见 `dataset.txt`、`push-latest.log`。manifest 带正确 `Docker-Content-Digest`、`Content-Type`。

### 资格链（真实 probe 建立）
- Source/映射/策略由夹具 `fixture.sql` 提供；但 `source_host_caps` 与 `registry_host_recovery=ok` 是**真实 host probe 探测**产生（日志："Host registry.gitlab.com 验证通过（native）"），非手工伪造。
- 探测过程中真实遇到并解决了：缺 source_realm_policy → 补策略；realm 格式非法 → 合法 https origin；probe 用 latest tag 而数据集缺 latest → 补推 latest。最终 probe 通过。

### 阶段10 场景（真实请求，证据 phase10-scenarios.log / gw-log-t2.log）
- **B sequential 跨站回退**：裸路径 `/v2/library/alpine/manifests/t4`（冷缓存）→ 200、`X-ML-Source: fixture-reg-a`、`Docker-Content-Digest=sha256:425cc93f…d04`，与推送时 t4 digest 完全一致；`cross_run_total{mode=sequential,outcome=ok}=1`。
- **多 tag 冷回源**：t1/t2/t3/t5 全部 200、命中 fixture-reg-a、digest 逐一与 dataset.txt 一致。首字节/完整延迟：t1 6534ms（含源切换建立）、t2 1091ms、t3 1660ms、t5 1661ms。
- **缓存命中**：t4 第2次 0.8ms、无 X-ML-Source（不经回源）；第1次 16.7ms 带 source。行为正确。

### 关键架构发现（实测纠正先前假设）
- B 跨站仅对 `OfficialOnly=false` 生效。Foreign host（`/v2/quay.io/...`）的 `officialOnly=foreign=true` → B 跳过、走官方直连（真实日志 `WWW-Authenticate: realm="https://quay.io/v2/auth"`）。裸路径 `/v2/library/alpine/...`（foreign=false，`rangeHostOf=docker.io`）→ B 生效，故副本映射权威设为 docker.io。
- SSRF 防护拒绝内网地址做信任中转（172.19.0.x），但 24h 内沿用系统解析，proxy 请求实际可达自建源。
- discover 45s 后发现 70 个公网候选源（容器内无公网、探测失败），为噪声，不影响 fixture 候选命中。

## 阶段11 性能对照：部分完成，未做满正式验收

已取得：同环境、真实 registry、真实请求延迟样本（见上 t2/t3/t5 的 P50 级数据，单样本非统计结论）。

未完成正式配对的原因（诚实披露，非能力问题）：
1. A10 阈值针对"竞速组 P95 相对基线改善 ≥50%"，需同 host 快慢两个源触发竞速。当前两个自建源速度相同（同为本地 registry:2），无速度差，竞速不会产生 P95 改善，测出的"无改善"是环境所致，不能作为性能结论。
2. 要造真实速度差需给一个源注入网络延迟（tc netem），并保持 3.5.17/3.5.18 两版本同环境各跑 10 轮——这是完整子项目，尚未执行。
3. 环境辅助容器曾因 sleep 超时退出，导致中途一次配置未生效（已识别并重建为持久容器）。

按 §12.5 纪律：基线环境不完整/样本不足时不得宣称性能提升，不得用模拟数据替代。故阶段11 当前标记 **PARTIAL / 未做满正式 10 轮配对**，不判 PASS/FAIL。终态暂维持 `PERFORMANCE_EXTERNAL_BLOCKED`（待正式配对后按实测覆盖）。

## 环境资源（收尾时清理）

- 网络 ml318-acc；卷 ml318-reg-a/b、ml318-data；容器 ml318-reg-a/b、ml318-gw、ml318-fixture、ml318-push2。
- 候选与基线镜像保留（证据价值）。
