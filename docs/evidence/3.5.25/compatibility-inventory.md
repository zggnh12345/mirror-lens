# 兼容参数普查（G0，基线 84b013d）

## POST /api/sources（internal/api/api.go:599-704）

| 旧字段 | 行号 | 当前处理 |
|---|---|---|
| `url` | :601 | 无 connect_host 时作为 rawURL 回退（:654-656，注释 :625“旧 API 调用兼容”） |
| `host` | :606 | 归属别名，与 repo_host 二选一（:618-623，host 优先）；不参与连接地址 |
| `repo_host` | :611 | 归属别名兜底（:618-623） |
| `scheme` | :609 | 空则默认 https（:635-638）；与 port 一起构造 SourceEndpoint（:648） |
| `port` | :610 | 校验范围（:639-647）；参与 endpoint.New（:648） |
| `connect_host` | :612 | 主连接地址来源（:624-653）；缺失时从 url 解析（:629-633） |

后续链路：normalizeURL(:657,:723-748)、probe.CheckURL(:662-666)、归属推断/未知地址 400(:672-689)、registryhost.Check(:690-697)、AddSource(:698-704)。旧 url 仍在测试与验收文档使用（add_source_test.go:15,19,39；g3-real-acceptance.md:22-23）。

## PATCH /api/sources/{id}（internal/api/api.go:900-978）

`base_url`(:910)、`repo_host`(:911)、`scheme`(:912)、`port`(:913) 与 metadata 字段并存；任一地址/归属字段出现即取当前 base_url(:921-927)，endpoint.Parse/New 重建(:929-955)，normalizeURL(:956-960)，SetBaseURL(:961-965)、TouchProbe0(:966-969)、RunSource(:970 异步)、SetRepoHost(:972-977)。

## POST /api/sources/{id}/hosts（internal/api/hosts.go:41-67）

DTO 仅 host/primary_realm/extra_realms，**无旧地址字段**；AddSourceHost 在 :54。

## UI 旧字段入口（internal/api/web/index.html）

表单 addScheme(:257-258)/addPort(:264-265)/addRepoHost(:266-267)、editScheme(:291)/editPort(:298)/editRepoHost(:300)；POST 提交 connect_host/scheme/port/repo_host(:1489-1497)；PATCH 提交 base_url+repo_host(:1515-1523)；编辑回填从 base_url 解析(:1219-1228)。

## 旧响应投影/兼容派生

`internal/store/store.go:26-28`（Source.BaseURL json base_url）、:61-72（RepoHost/RegistryIdentity/Hosts 派生）；`internal/store/hostcaps.go:170-171`（“仅供现有 UI/JSON 兼容”）；`internal/api/trusted_api.go:74`；`internal/api/web/index.html:801,804,818,852,931,1220,1329,1425`。非 source/host API 的兼容包装：`internal/adapter/registryadapter/registry.go:131`（splitHost）、:280（Route 包装），不在本批删除范围。

## 3.5.25 冻结删除口径（任务书 5020ec5）

POST 拒绝 url/host/顶层 scheme/顶层 port/repo_host/connect_host，返回 400 `legacy_field_not_supported`；PATCH 拒绝 base_url/repo_host/scheme/port 同类错误；其他未知字段 400 `unknown_field`；严格字段校验仅覆盖 POST/PATCH Source 与 Host POST（任务书 :921-929,:977）。新契约 `endpoint.scheme`+`endpoint.address` 落在 api.go:599-613、:900-914 与 hosts.go:41-45；底层地址模型 internal/endpoint/endpoint.go:48-103 已存在，不重写。
