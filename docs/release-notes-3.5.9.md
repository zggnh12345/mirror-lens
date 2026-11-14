# Mirror Lens 3.5.9 发布说明

**版本：** 3.5.9
**基线：** 3.5.8（`946b946`）
**Schema：** 不变，仍为 3.5.7 Canonical Schema；不运行 `db-migrate`。

## 范围

同时调整 Registry 的面板展示和 3.5.8 审查中确认的两个行为问题。
面板仍是只读派生视图，不产生第二套配置事实。

### 面板

- 设置页的 `registry.multi_origin_hosts` 从自由文本框改为固定 8 个独立开关：
  `GCR`、`K8S`、`PKG`、`QUAY`、`MCR`、`GITLAB`、`ECR`、`CGR`。
- 开关全部放在同一张卡片内：宽屏一行 4 个，窄屏一行 2 个。
- 空列表仍然是“没有任何 Host 使用多源”；单个开关关闭只让该 Host 回官方源。
- 源列表增加只读 Registry 标签：
  `HUB`、`GHCR`、`GCR`、`K8S`、`PKG`、`QUAY`、`MCR`、`GITLAB`、`ECR`、`CGR`。
- 标签按 `RepoHost` 派生，读取对应的能力列；`PKG` 用 `IsArtifactRegistry`，
  未探测、探测失败或能力未知不显示。
- 标签只在列表展示，不参与 Source 选择、调度、探测目标或缓存键计算。

### F1：裸 Host 缺仓库名

`/v2/gcr.io/manifests/latest` 这类“只有 Host、没有 owner/repo”的请求，
不再静默解释成 Docker Hub 上名为 `gcr.io` 的仓库，而是返回 400 并说明
“显式 Registry Host 后缺少仓库名”。

- 这是明确的语法决策：裸 Host 按显式 Host 处理。
- `library/gcr.io` 这类带命名空间的 Docker Hub 仓库仍然合法。
- Docker Hub 上确实存在一个恰好叫 `gcr.io` 的裸仓库时也会被拒绝；这是
  为避免拼错 Host 被静默回落到默认上游而接受的兼容性代价。

### F3：Blob HEAD/GET 的 401/403 语义

候选源和官方源现在使用同一套鉴权语义：

- 候选源 401/403：记中性事件、不冷却，继续试下一个目标；
- 候选源 429/5xx：记失败并冷却；
- 官方源 401/403：把官方 `WWW-Authenticate` 原样交给客户端；
- 候选源的挑战头不会冒充官方挑战。

Blob HEAD 和 GET 都已按这个顺序处理；Manifest GET 走 `fetchOnce` 时也会
透传官方挑战。另修正了 GET 换源处 `reportSource` 的 `ok` 取反：之前
429/5xx 会被当成成功，401/403 反而会被冷却。

### F2：完整性保证边界

3.5.9 继续保留流式交付，不改变大 Blob 的首字节延迟模型。明确承诺为：

- 摘要或长度不匹配的 Blob 不提交缓存；
- 客户端可能在校验完成前已经收到部分字节，3.5.9 不承诺“客户端绝不收到
  未完整校验的 Blob”。

要实现后者需要先落临时文件、校验后再发响应头，属于单独的性能与产品决策，
不在本版本范围内。

## 兼容性

- 不新增配置键，不新增数据库列。
- 已有的 `multi_origin_hosts` 列表会被原样读取；8 个固定项映射为开关。
- 配置里存在开关未覆盖的 Host 时，设置页会显示提示并保留原值，不静默删除。
- `lscr.io` 仍不在第一批默认范围内。

## 升级

1. 备份数据库和 `secret.key`。
2. 用 3.5.9 镜像启动；Schema 不变，不需要迁移。
3. 打开设置 → 配置参数 → `Registry 加速`，确认 8 个开关与当前配置一致。
4. 打开首页源列表，确认已探测的 Foreign Host 显示短标签；未探测的源没有标签。

## 回滚

- 回滚到 3.5.8 不需要改数据库；配置仍是同一个 `multi_origin_hosts` 列表。
- 若只想关闭多源，保持 8 个开关全关并重启，等价于 `multi_origin_hosts: []`。

## 验证

- `go test ./...`
- `go test -race ./...`
- 面板契约测试覆盖 HUB/GHCR/RAW 与 GCR/PKG 标签、未探测不显示标签、Foreign Host 不重复显示 HUB。
- 设置页契约测试覆盖 8 个固定 Host、4 列/2 列布局和独立开关绑定。
- 路径测试覆盖裸 Host 缺仓库名返回 400、`Plan`/`ParsePath` 两条入口，以及 `docker.io/gcr.io`、`docker.io/example.com/...` 逃生路径。
- 鉴权测试覆盖候选 401/403 的 HEAD/GET 回退、候选 401 + 官方 401 的官方挑战、候选 401 + 官方 5xx 不泄漏挑战、GET 429/5xx 记失败并冷却、Manifest GET 挑战透传。
- 多源测试覆盖 8 个 Host 的 native 与 host_prefix 候选命中、候选 401/5xx/网络失败回官方、候选与官方同时失败不伪装成功、候选 Blob 的 Digest 与缓存命中、失败隔离。
- 真实进程验收覆盖：F1 裸 Host 400；F7 空范围下 8 个 Foreign Host 和 Docker Hub 官方直连，并记录实际官方 Host。F3 的鉴权矩阵、F4 的 8 Host 候选命中、F5 的候选失败回退由单元/受控源测试覆盖；F4/F5 的第三方源为受控测试源，不宣称公共镜像站清单已逐台验收。

## 不包含

- F6 的未知点号命名空间仍返回 400，不为了支持 `example.com/project` 放宽 Host 校验。
- F8 的 `lscr.io` 仍不在第一批默认范围；本版本不增加“永久禁止手工配置”的强制校验。
- 不改变 F2 的流式架构：客户端完整预校验保证仍不在 3.5.9 内。
- 不把候选源 401 的挑战头透传给客户端；只有官方源挑战透传。
- 不新增第三方 Source 的公共镜像站清单；F4、F5 的结论只对实际跑过的源成立。
