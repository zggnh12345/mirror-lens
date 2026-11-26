# P2-2 RED 规格：出站地址分类、FakeIP 独立信任域与 Pin 严格边界

- 版本：3.5.24
- 分支：`3.5.24-impl`
- 代码基准：`af10552`（唯一基准；不以 `acef0a5` 的 main 基线替代）
- 文档状态：`SPEC_WRITTEN / RED_NOT_WRITTEN`
- P2-2 状态：`NOT_CLOSED`
- 授权边界：仅实施、修复、测试与独立复核；`PRODUCTION_DEPLOYMENT=NOT_AUTHORIZED`；不得推送、合并 main、发布或部署。
- 本文件性质：把用户 2026-11-26 最终裁定转成可执行规格；**未修改冻结文档、未修改产品代码、未写 RED 测试**。

---

## 0. 一句话结论

本轮不是“给四个 Guard 加一个 FakeIP 开关”，而是三件事同时收紧：

1. `AllowPrivate` 从“跳过全部 IP 分类”收窄为“只放行 RFC1918 + IPv6 ULA”，回环交给独立的 `AllowLoopback`。
2. `198.18.0.0/15` 从“私网放行顺带可用”改为“仅 discover 专用 Guard、仅域名解析结果、仅 Canary healthy 时可拨”；字面量、Pin、gateway、probe、其他默认 Guard 一律拒绝。
3. Pin 的创建与每次实际拨号都走严格 public-only；Redirect 按完整 Origin（Scheme + Host + EffectivePort）比较，任一变化在第二次 Dial 前拒绝。

---

## 1. 规范性输入（本轮裁定的执行口径）

以下为规格的规范来源，冲突时以本轮最新裁定为准：

- `AllowPrivate` 只负责 RFC1918 和 IPv6 ULA 私网地址。
- `AllowLoopback` 独立控制回环地址。
- CGNAT、FakeIP、文档段、基准测试段、链路本地、组播及其他特殊用途地址不属于 `AllowPrivate` 的放行范围。
- `198.18.0.0/15` 不得通过 `AllowPrivate` 放行。
- `AllowFakeIP` 是独立的、受 Canary 约束的 discover 拨号例外，不能复用 `AllowPrivate`。
- Pin 使用严格检查，不受上述放宽选项影响。
- FakeIP 只属于 discover：gateway/probe 保留私网配置能力但不接 FakeIP Resolver；Pin 不接 FakeIP Resolver；其他 SafeHTTP 默认 Guard 不允许 FakeIP。
- URL 直接使用 `198.18` 字面量必须拒绝，即使 discover 的 `AllowFakeIP=true`。
- Pin 严格检查先于数据库 IP 真正拨号，覆盖多 IP、v4/v6、数据库篡改；Redirect 完整比较 Scheme、Host、EffectivePort，任一变化默认拒绝。
- Canary 使用 `resolver_error`、`canary_invalid`、`healthy` 三态；`SERVFAIL` 不得只靠 `DNSError.IsTemporary()` 分类；拿不到 RCODE 时必须保守并记录能力限制。
- Guard 不可变；配置变化通过重建 Guard 生效；不声称取消 in-flight 请求。
- 五组边界必须分别测试，不能由一组 `safehttp_test.go` 代替。
- 实施前先补齐现有配置兼容性测试，明确 RFC1918、ULA、Loopback 的既有行为。

---

## 2. 地址分类模型（单一事实源）

### 2.1 四个互斥类别

| 类别 | 地址范围 | 默认 | 唯一放宽入口 | 说明 |
| --- | --- | --- | --- | --- |
| `PRIVATE` | `10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`fc00::/7` | 拒绝 | `AllowPrivate=true` | 只此四段；不得用 `IsPrivate()` 以外的扩展口径 |
| `LOOPBACK` | `127.0.0.0/8`、`::1/128` | 拒绝 | `AllowLoopback=true` | 与 `AllowPrivate` 完全独立，二者互不蕴含 |
| `FAKEIP` | `198.18.0.0/15` | 拒绝 | discover 专用 Guard 的“域名解析结果 + Canary healthy” | 字面量恒拒；Pin/gateway/probe/默认 Guard 恒拒 |
| `ALWAYS_BLOCK` | 见 2.2 | 拒绝 | 无 | 任何开关都不能放行 |
| `PUBLIC` | 以上之外 | 允许 | 不需要放宽 | 仍受 Dial 时实际 IP 复查 |

### 2.2 `ALWAYS_BLOCK` 清单（按 IANA 特殊用途登记表系统整理）

IPv4：

- `0.0.0.0/8`（本网络）
- `100.64.0.0/10`（CGNAT；**不属于 AllowPrivate**）
- `169.254.0.0/16`（链路本地；**不属于 AllowPrivate**）
- `192.0.0.0/24`（IETF 协议分配；含 PCP/TURN anycast，整体按不可信处理）
- `192.0.2.0/24`（TEST-NET-1）
- `192.88.99.0/24`（6to4 中继任播，已废弃）
- `198.51.100.0/24`（TEST-NET-2）
- `203.0.113.0/24`（TEST-NET-3）
- `224.0.0.0/4`（组播）
- `240.0.0.0/4`（保留，含 `255.255.255.255/32`）

IPv6：

- `64:ff9b::/96`（NAT64，可能映射到私网）
- `64:ff9b:1::/48`（本地 NAT64）
- `100::/64`（Discard-Only）
- `2001::/23`（IETF 协议分配整段：Teredo、benchmarking、ORCHID、文档等）
- `2001:db8::/32`（文档示例；已被 `/23` 覆盖，保留显式规则便于证据）
- `2002::/16`（6to4，可嵌入私网 IPv4）
- `3ffe::/16`（6bone，已回收）
- `3fff::/20`（文档示例）
- `5f00::/16`（SRv6 SID）
- `fe80::/10`（链路本地）
- `fec0::/10`（站点本地，已废弃）
- `ff00::/8`（组播）
- `::/128`（未指定）
- `::1/128`（回环；归 `LOOPBACK`，不是 `ALWAYS_BLOCK`）

> 说明：`192.0.0.9/32`、`192.0.0.10/32`、`192.31.196.0/24`、`192.52.193.0/24`、`192.175.48.0/24`、`2620:4f:8000::/48` 等 IANA 标记为 globally reachable 的特殊用途段，本规格不纳入 `ALWAYS_BLOCK`；其中 `192.0.0.0/24` 因与不可路由子段混在同一 /24，按整段拒绝（可接受的过度阻断，不构成安全缺口）。若后续要求精确放行，必须另立条目并单独测试，不得在本轮顺手放宽。

### 2.3 IPv4-mapped IPv6

所有入口先 `Unmap()`，再分类。`::ffff:10.0.0.1`、`::ffff:127.0.0.1`、`::ffff:198.18.0.4` 必须分别按 IPv4 的 `PRIVATE`、`LOOPBACK`、`FAKEIP` 处理，不能借 IPv6 分支绕过。

---

## 3. `AllowPrivate` / `AllowLoopback` 精确语义

### 3.1 目标算法（伪代码，顺序即语义）

```go
func (g *Guard) CheckIP(ip netip.Addr) error {
    ip = ip.Unmap()
    if !ip.IsValid() { return ErrBlocked }

    // 1) 回环只认 AllowLoopback，AllowPrivate 不得提前放行
    if ip.IsLoopback() {
        if g.o.AllowLoopback { return nil }
        return ErrBlocked
    }

    // 2) 私网只认 AllowPrivate，且只认 RFC1918 + ULA
    if inPrivateNets(ip) {
        if g.o.AllowPrivate { return nil }
        return ErrBlocked
    }

    // 3) 其余特殊用途段一律拒绝；FakeIP 段也在其中
    if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() ||
       ip.IsLinkLocalMulticast() || inAlwaysBlockNets(ip) {
        return ErrBlocked
    }

    return nil
}
```

关键点：

- 删除 `safehttp.go:112-114` 的 `if g.o.AllowPrivate { return nil }` 全放行短路。
- `AllowPrivate` 不得再放行 `127/8`、`::1`、`100.64/10`、`169.254/16`、`198.18/15`、`192.0.2/24`、`2001:2::/48`、`100::/64`、组播、未指定等。
- `AllowLoopback` 只影响回环；不得顺带放行私网或 FakeIP。
- 两个开关都为 false 时，行为与今天默认 Guard 一致（除新增的特殊段拒绝）。
- `Check()` 对 URL 中的 IP 字面量继续直接调用 `CheckIP`；域名不在 `Check()` 解析。

### 3.2 严格路径 `CheckIPStrict`

新增导出方法：

```go
// CheckIPStrict 忽略 AllowPrivate / AllowLoopback / FakeIP，
// 只允许 PUBLIC。Pin 创建、Pin 拨号、过渡期回落都必须用它。
func (g *Guard) CheckIPStrict(ip netip.Addr) error
```

语义：

- 等价于 `CheckIP` 且两个放宽开关强制 false、FakeIP 强制 false。
- 仍保留 `Unmap`、`IsValid`、特殊段清单。
- 不得通过 `New(Options{})` 临时构造来“假装严格”；必须在同一 Guard 内提供严格路径，避免丢失超时、IPFamily、拨号配置。

### 3.3 兼容性定义

“兼容”只指以下既有行为不被破坏：

| 场景 | 既有行为 | 目标行为 |
| --- | --- | --- |
| `AllowPrivate=false` + `10/8`、`172.16/12`、`192.168/16`、`fc00::/7` | 拒绝 | 拒绝 |
| `AllowPrivate=true` + RFC1918 | 放行 | 放行 |
| `AllowPrivate=true` + ULA | 放行 | 放行 |
| `AllowLoopback=true` + `127/8`、`::1` | 放行 | 放行 |
| `AllowLoopback=false` + 回环 | 拒绝 | 拒绝 |
| `AllowPrivate=true` + 回环 | **放行（历史越权）** | **拒绝；需 `AllowLoopback=true`** |
| `AllowPrivate=true` + CGNAT/链路本地/文档段/基准段/组播/未指定 | **放行（历史越权）** | **拒绝** |

最后两行是本次有意改变的行为，不是回归。第一处会命中现有测试：

- `internal/safehttp/family_test.go:74` 用 `AllowPrivate: true` 拨 `localhost`；
- `internal/safehttp/family_test.go:97` 用 `AllowPrivate: true` 跑 RACE 目标。

实施时必须把这两处改成 `AllowLoopback: true`（必要时同时保留 `AllowPrivate`），并在提交说明中标注“测试从历史越权行为迁移到独立开关”，不得把测试改动伪装成产品行为未变。

---

## 4. FakeIP 独立信任域

### 4.1 结构约束

- 不新增用户可见的 `allow_fakeip` 配置项；FakeIP 权限只能由以下三者共同派生：
  1. 配置了专用 `network.fakeip_resolver`（仅 `IP:port` 字面量，禁止域名，拒绝 `127.0.0.11`）；
  2. 配置了显式 Canary 域名（`network.fakeip_canary`）；
  3. 该 Guard 构造时 Canary 验证结果为 `healthy`。
- `Options` 不暴露 `AllowFakeIP` 布尔值；FakeIP 能力是 discover 专用构造函数内部的派生结果，其他调用方无法通过 `Options` 打开。
- gateway、probe、Pin、`safehttp.Default()`、TLS 探测 `Dialer()` 全部不接入专用 Resolver；它们的域名解析仍走系统解析或各自既有路径。
- Guard 不可变：Canary 结果在构造时固化；配置变化重建 Guard；旧 Guard 不被新请求引用；不声称自动取消 in-flight 请求。

### 4.2 只允许“域名解析结果”

Dial 决策必须区分原始目标是字面量还是域名：

| 原始目标 | 解析结果 | 结果 |
| --- | --- | --- |
| 字面量 `198.18.0.4` | 不解析 | 拒绝（即使 FakeIP Guard、Canary healthy） |
| 域名 | `198.18.0.4` | 仅 discover FakeIP Guard + healthy 时允许 |
| 域名 | `10.0.0.1` | 拒绝（FakeIP 不放行私网） |
| 域名 | `192.0.2.10` / `2001:2::10` / `100::10` | 拒绝（FakeIP 不放行特殊段） |
| 域名 | `1.1.1.1` | 按普通公网处理 |

实现要点：

- `Check()` / `CheckIP()` 对 `198.18/15` 始终拒绝；字面量路径不会经过 FakeIP 例外。
- FakeIP 例外只存在于拨号决策函数，例如：
  ```go
  func (g *Guard) checkDialIP(ip netip.Addr, viaDomain bool) error
  ```
  仅当 `viaDomain && g.fakeipEnabled && inFakeIPRange(ip)` 时放行 `198.18/15`；其余一律回落到 `CheckIP` / `CheckIPStrict` 语义。
- `Dialer()` 返回的通用拨号器不得带 FakeIP 例外；TLS 探测、Pin、外部自握手的调用方拿到的都是严格路径。
- FakeIP Guard 的域名拨号必须使用专用 Resolver（auto 模式通过 `net.Dialer.Resolver`，v4/v6 模式通过专用 `LookupNetIP`）；不得回落到系统 DNS。
- 解析阶段过滤只是优化；`Control` / 实际 Dial 前的逐候选检查才是最终边界。

### 4.3 其他 Guard 的处置

| 消费点 | FakeIP | AllowPrivate | 处置 |
| --- | --- | --- | --- |
| discover feed 抓取 | 允许（域名 + healthy） | 强制 false | 专用 Guard；`discover.go:19` 的包级 `Default()` 替换 |
| gateway 回源 | 不允许 | 保留 `cfg.Probe.AllowPrivate` 能力 | `main.go:479` 不传 FakeIP Resolver |
| probe | 不允许 | 保留 `allowPrivate` 能力 | `probe.go:90` 不传 FakeIP Resolver |
| Pin | 不允许 | 不适用（严格 public-only） | 不接 Resolver；创建与拨号均 `CheckIPStrict` |
| 其他 `Default()` | 不允许 | 不允许 | 保持 strict |

`Registry Source` 指向 `198.18.0.0/15` 时，即使 discover 的 Guard 有 FakeIP 能力，gateway 也不得因此成功；逻辑 Registry 身份不构成出站地址授权。

---

## 5. Pin：创建、拨号、Redirect 三层严格

### 5.1 创建阶段

`internal/api/relay_pin.go`：

- `checkPinList`（:123-148）与 `resolveRelayHost`（:152-203）必须使用 `CheckIPStrict` 作为唯一判定，不得继续维护第二套 `bogusPinNets` 清单。
- 现有 `bogusPinNets`（:26-39）缺 `2001::/23`、`2001:2::/48`、`2002::/16`、`3fff::/20`、`5f00::/16`、`64:ff9b:1::/48` 等；这些段同时也不在 safehttp 内置清单中，继续保留会造成创建阶段放行、拨号阶段才拒的“半开”状态。
- 手工填写与 DNS 解析结果都必须逐地址检查；任一非法地址不得进入 pin 集合；全部非法则报错。
- `bogusPinAddr` 若保留，只用于错误原因展示，不得作为安全判定；安全判定只认 `CheckIPStrict`。

### 5.2 拨号阶段

`internal/gateway/pin.go`：

- `pinnedClient`（:56-146）当前在 `dial` 内直接用 `net.Dialer`（:91）拨 `PinIPv4/PinIPv6`，完全不过 Guard；数据库被篡改或手工写入私网 IP 时可直连。必须在每个候选地址真正 Dial 前调用 `CheckIPStrict`，并在 `net.Dialer.Control` 再查一次实际连接地址。
- 保留现有 10s 超时、1ms 竞速、KeepAlive、v4/v6 多候选、连接清理逻辑；不得改成丢失这些配置的默认 Client。
- 多 IP 场景按候选逐个检查：非法候选不拨；合法候选继续；全部非法返回 `ErrBlocked`。不得因一个非法候选而放行其他非法候选，也不得把整个集合交给裸 Dialer。
- 严格检查必须在 `DialContext` 之前执行，不能只依赖 `CheckRedirect` 或创建阶段。

### 5.3 过渡期回落

`pinUsable`（:22-36）在 `""` / `unresolved` 且未过 Grace 时返回 `pinned=false`，`clientFor`（:40-52）随即返回 `g.privClient`。这是 Pin 路径的第二个入口，而且比“继承 AllowPrivate”更宽：

- `g.privClient` 由 `newPrivClient()`（`private.go:264-275`）构造，是**完全裸的 `http.Client`**：没有 SafeHTTP、没有拨号层 IP 检查、没有 `CheckRedirect` 策略；不仅能访问 RFC1918，还能访问回环、链路本地、`198.18/15`、云元数据地址，并跟随任意重定向。
- 即使 gateway 的 `AllowPrivate=false`，这条路径也照样放行私网；`AllowPrivate=true` 只是让它更“名正言顺”。
- 规格要求：Pin 相关路径（含过渡期回落）使用严格系统解析 Client（`AllowPrivate=false`、`AllowLoopback=false`、无 FakeIP、完整 Redirect 策略），不得复用 `g.privClient`。
- 若产品决定保留过渡期私网能力，必须单独裁定并明确记录为例外；在本轮裁定下，Pin 无例外。

### 5.4 非 IP 候选

`pinnedClient` 当前对 `ips` 中的字符串直接 `net.JoinHostPort(ip, port)` 后交给 `net.Dialer`。当前 `store.RelayPinIPs`（`trusted.go:96-109`）会过滤掉非 IP，导致 `ips` 为空并在 pinned 路径 fail-closed；但这只是存储层的一道过滤，不能作为拨号层的安全前提。若未来存储层放宽、缓存被污染，或状态为 `unresolved` 且仍在 Grace 内（走 5.3 的裸 client），主机名就可能进入系统解析。

- 每个候选必须先 `netip.ParseAddr`；解析失败 → 该候选非法，不得交给 Dialer。
- 解析成功后 `Unmap()`，再 `CheckIPStrict`；两步都通过才允许拨号。
- 不得依赖 `store.RelayPinIPs` 的写入/读取校验作为唯一防线；拨号层必须独立复核。

### 5.5 Redirect：完整 Origin

`pinnedClient` 的 `CheckRedirect`（:139-144）当前只做 `safehttp.Default().Check(req.URL)`：

- 同 Host 不同端口：`Check` 只校验 IP 字面量/协议，域名 host 直接通过；随后 `dial` 的 host 比较仍匹配，于是会用钉住 IP 拨新端口——**必须在 CheckRedirect 拒绝**。
- 不同 Host：`Check` 通过，`dial` 阶段才因 host 不匹配失败；虽然未真正连上，但已进入第二次 Dial 调用，不符合“第二次 Dial 前拒绝”。
- HTTPS→HTTP：`Check` 接受 http，当前会放行；**必须拒绝**。

目标：

```go
// 完整 Origin = Scheme + CanonicalHost + EffectivePort
// 任一变化 -> ErrBlocked；不得进入第二次 Dial。
```

- Scheme 比较：`http` vs `https` 任一变化拒绝（含 HTTPS→HTTP、HTTP→HTTPS；Pin 场景不做升级例外）。
- Host 比较：规范化（小写、去尾点）后完全相等；不同 Host 拒绝。
- EffectivePort：显式端口按原值；缺省按 Scheme 推导（http=80，https=443）；同 Host 不同端口拒绝。
- 同 Origin 的重复跳仍受 5 次上限约束。
- 拨号层保留独立严格 IP 检查；Redirect 检查与拨号检查是两道独立边界，不能互相替代。
- 错误统一为 `safehttp.ErrBlocked`，上层映射 403，不产生第二次拨号。

---

## 6. Canary 三态与 SERVFAIL

### 6.1 状态定义

| 状态 | 触发 | FakeIP 权限 | 普通域名解析 | 请求结果 |
| --- | --- | --- | --- | --- |
| `resolver_error` | DNS 传输失败、超时、响应无法解析 | 关闭 | 不回退系统 DNS；使用专用 Resolver 的请求失败 | 502/503 |
| `canary_invalid` | 收到有效 DNS 响应，但 Canary 不符合预期 | 关闭 | 继续使用专用 Resolver | 公网域名正常；FakeIP 域名失败 |
| `healthy` | Canary 验证成功 | 授予限定 FakeIP 权限 | 专用 Resolver | 正常 |

补充：

- Canary 未配置不作为第四运行时态：构造 Guard 时直接不授予 FakeIP（`fakeipEnabled=false`），专用 Resolver 仍可用于普通域名。运行期状态机仍只有三态。
- Canary 必须由用户显式配置；不得用“私网地址”或默认域名代替。
- Canary 查询只取 A 记录（IPv4 FakeIP only）；要求至少一条 A，且全部 A 属于 `198.18.0.0/15`；混入公网、私网、特殊段或空结果均判 `canary_invalid`。
- `resolver_error` 不得回退系统 DNS；`canary_invalid` 不得更换 Resolver。
- `healthy` 只授予“域名解析结果落在 198.18/15”的拨号例外，不授予私网、回环、特殊段、字面量。

### 6.2 SERVFAIL 分类

Go `net.Resolver` 不暴露 DNS RCODE。规格要求：

1. 先按 `*net.DNSError` 分类，不使用 `IsTemporary()` 作为唯一依据。
2. `IsNotFound`（NXDOMAIN / no such host）→ `canary_invalid`。
3. 超时、连接拒绝、网络不可达、响应无法解析 → `resolver_error`。
4. `Err == "server misbehaving"`（SERVFAIL 的常见包装）→ 归 `canary_invalid`，因为至少收到了 DNS 响应；同时记录“RCODE 不可得，无法与畸形响应可靠区分”的能力限制。
5. 其余无法分类的错误 → 保守归 `resolver_error`（关闭 FakeIP、请求失败），并在证据中记录。
6. 无论归类为 `resolver_error` 还是 `canary_invalid`，FakeIP 都必须关闭；安全不变量不依赖分类是否正确。
7. 若后续引入能读取 RCODE 的实现，必须单独提交证据并更新本节；本轮不得引入第三方 DNS 库。

### 6.3 Guard 不可变

- Canary 结果存于 Guard 构造期字段，不提供 setter、不提供运行时热撤销。
- 配置变化：重建 Guard；新请求使用新 Resolver + 新 Canary 状态；旧 Guard 不再被新请求引用。
- 旧 Guard 上已经开始的请求可能继续使用旧状态；不得声称重建自动取消 in-flight 请求。

---

## 7. 五组边界与测试隔离

| 边界 | 混淆表现 | 必须分开的测试 |
| --- | --- | --- |
| B1 `AllowPrivate` ↔ `AllowFakeIP` | 用 `AllowPrivate=true` 放行 `198.18/15` | AP-01（198.18 + private=true 拒）与 FP-05（fakeip=true + 10/8 拒） |
| B2 URL 字面量 ↔ DNS 解析结果 | 字面量 `198.18.0.4` 因 discover 有 FakeIP 而通过 | FP-01/02（字面量拒）与 FP-04（域名结果可放行） |
| B3 创建阶段校验 ↔ 实际拨号校验 | 创建通过就认为拨号安全；DB 篡改后直连 | PIN-01（创建拒私网）与 PIN-10/11（篡改 pin 拨号拒） |
| B4 Resolver 可用性 ↔ Canary 授权状态 | Resolver 通就认为 FakeIP 可用；Canary 失败就换 Resolver | CAN-01（resolver 通、canary 坏：公网域名可用、FakeIP 关）与 CAN-02（resolver 坏：域名失败、不回退） |
| B5 Registry 逻辑身份 ↔ 实际出站地址 | 逻辑 Host 是公网域名，实际源解析到 `198.18` 仍成功 | FP-08（逻辑身份不授权 198.18 出站） |

---

## 8. RED 测试矩阵

> 所有测试先写、先确认失败（RED），再实施。下表“当前预期”基于 `af10552` 的真实代码路径；“当前预期”为 `nil/放行` 的条目才是 RED 证据；为 `error/拒绝` 的条目是回归护栏，不单独证明缺陷存在。

### 8.1 AllowPrivate 收窄（AP）

| ID | 输入 | Options | 当前预期 | 目标 |
| --- | --- | --- | --- | --- |
| AP-01 | `198.18.0.4` | `AllowPrivate=true` | nil（`safehttp.go:112-114`） | ErrBlocked |
| AP-02 | `198.19.255.255` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-03 | `192.0.2.10` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-04 | `2001:2::10` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-05 | `100::10` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-06 | `169.254.169.254` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-07 | `100.64.0.1` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-08 | `fe80::1` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-09 | `224.0.0.1` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-10 | `0.0.0.0` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-11 | `255.255.255.255` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-12 | `2001:db8::1` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-13 | `2001::1`（Teredo/`2001::/23`） | `AllowPrivate=true` | nil | ErrBlocked |
| AP-14 | `2002::1` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-15 | `3fff::1` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-16 | `5f00::1` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-17 | `64:ff9b:1::1` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-18 | `::ffff:198.18.0.4` | `AllowPrivate=true` | nil | ErrBlocked |
| AP-19 | `::ffff:10.0.0.1` | `AllowPrivate=true` | nil | OK（Unmap 后按 RFC1918 放行） |
| AP-20 | `10.0.0.1` | `AllowPrivate=true` | nil | nil（兼容） |
| AP-21 | `172.16.5.5` | `AllowPrivate=true` | nil | nil（兼容） |
| AP-22 | `192.168.1.5` | `AllowPrivate=true` | nil | nil（兼容） |
| AP-23 | `fc00::1` | `AllowPrivate=true` | nil | nil（兼容，ULA） |
| AP-24 | `fd12:3456::1` | `AllowPrivate=true` | nil | nil（兼容，ULA） |
| AP-25 | `10.0.0.1` | `AllowPrivate=false` | ErrBlocked | ErrBlocked（回归） |
| AP-26 | `fc00::1` | `AllowPrivate=false` | ErrBlocked | ErrBlocked（回归） |
| AP-27 | `198.18.0.4` | `AllowPrivate=false` | nil | ErrBlocked |

### 8.2 Loopback 独立（LP）

| ID | 输入 | Options | 当前预期 | 目标 |
| --- | --- | --- | --- | --- |
| LP-01 | `127.0.0.1` | `AllowPrivate=true, AllowLoopback=false` | nil | ErrBlocked |
| LP-02 | `::1` | `AllowPrivate=true, AllowLoopback=false` | nil | ErrBlocked |
| LP-03 | `::ffff:127.0.0.1` | `AllowPrivate=true, AllowLoopback=false` | nil | ErrBlocked |
| LP-04 | `127.0.0.1` | `AllowLoopback=true, AllowPrivate=false` | nil | nil（回归） |
| LP-05 | `::1` | `AllowLoopback=true` | nil | nil（回归） |
| LP-06 | `127.0.0.1` | 两者均 false | ErrBlocked | ErrBlocked（回归） |
| LP-07 | `127.0.0.1` | `AllowLoopback=true, AllowPrivate=true` | nil | nil（不冲突） |

### 8.3 FakeIP 适用范围（FP）

| ID | 场景 | 期望 | 当前预期 |
| --- | --- | --- | --- |
| FP-01 | `Check(URL("http://198.18.0.4/"))`，默认 Guard | ErrBlocked | nil → RED |
| FP-02 | `CheckIP(198.18.0.4)`，discover 专用 Guard、Canary healthy | ErrBlocked（字面量恒拒） | 专用 Guard 不存在 → 编译 RED |
| FP-03 | `checkDialIP(198.18.0.4, viaDomain=false)`，FakeIP Guard | ErrBlocked | 方法不存在 → 编译 RED |
| FP-04 | `checkDialIP(198.18.0.4, viaDomain=true)`，FakeIP Guard、Canary healthy、`AllowPrivate=false` | nil（仅 FakeIP 授权） | 方法不存在 → 编译 RED |
| FP-05 | `checkDialIP(10.0.0.1, viaDomain=true)`，FakeIP Guard、`AllowPrivate=false` | ErrBlocked（FakeIP 不放私网） | 方法不存在 → 编译 RED |
| FP-06 | `checkDialIP(192.0.2.10, viaDomain=true)`，FakeIP Guard | ErrBlocked | 方法不存在 → 编译 RED |
| FP-07 | `checkDialIP(2001:2::10, viaDomain=true)`，FakeIP Guard | ErrBlocked | 方法不存在 → 编译 RED |
| FP-08 | `checkDialIP(198.18.0.4, viaDomain=true)`，非 FakeIP 的 gateway Guard（`AllowPrivate=true`） | ErrBlocked（逻辑身份不授权出站） | 方法不存在 → 编译 RED |
| FP-09 | `checkDialIP(198.17.255.255, viaDomain=true)`，FakeIP Guard | nil（范围外，按公网） | 方法不存在 → 编译 RED |
| FP-10 | `checkDialIP(198.19.255.255, viaDomain=true)`，FakeIP Guard | nil（`/15` 上边界内） | 方法不存在 → 编译 RED |
| FP-11 | `checkDialIP(198.20.0.0, viaDomain=true)`，FakeIP Guard | nil（范围外，按公网） | 方法不存在 → 编译 RED |
| FP-12 | 配置了 `fakeip_resolver` 但未配置 Canary | FakeIP 关闭；普通域名仍走专用 Resolver | 专用 Guard 不存在 → 编译 RED |
| FP-13 | `Options` 不含 `AllowFakeIP` 字段 | 编译期约束 | 现状无该字段（PASS） |

> `198.18.0.0/15` 边界取四点验证：`198.17.255.255`（外，公网）、`198.18.0.0`（内）、`198.19.255.255`（内）、`198.20.0.0`（外，公网）。

### 8.4 Pin（PIN）

| ID | 场景 | 期望 | 当前预期 |
| --- | --- | --- | --- |
| PIN-01 | `checkPinList("10.0.0.1")` | error | error（`Default().CheckIP`，回归） |
| PIN-02 | `checkPinList("2001:2::10")` | error | **nil → RED** |
| PIN-03 | `checkPinList("2001::1")` | error | **nil → RED** |
| PIN-04 | `checkPinList("2002::1")` | error | **nil → RED** |
| PIN-05 | `checkPinList("3fff::1")` | error | **nil → RED** |
| PIN-06 | `checkPinList("5f00::1")` | error | **nil → RED** |
| PIN-07 | `checkPinList("64:ff9b:1::1")` | error | **nil → RED** |
| PIN-08 | `checkPinList("198.18.0.4")` | error | error（`bogusPinNets`，回归） |
| PIN-09 | `checkPinList("1.1.1.1")` | nil | nil（回归） |
| PIN-10 | 数据库篡改 pin=`127.0.0.1`，拨号到本地 listener | 不建立连接；ErrBlocked | **连接成功 → RED** |
| PIN-11 | 数据库篡改 pin=`10.0.0.1,127.0.0.1`，多 IP | 不连接 127；非法候选逐个拒绝 | **连接 127 → RED** |
| PIN-12 | 数据库篡改 pin=`198.18.0.4` | ErrBlocked | 连接失败但非策略错误（无 listener）；需断言 ErrBlocked |
| PIN-13 | 过渡期 `PinState=""`/`unresolved`，目标 `127.0.0.1`（无论 gateway `AllowPrivate` 取值） | ErrBlocked（严格系统解析） | **裸 `privClient` 直接放行 → RED** |
| PIN-14 | Redirect 同 Host 不同端口 | CheckRedirect 返回 ErrBlocked，不进入第二次 Dial | **nil → RED** |
| PIN-15 | Redirect 不同 Host | CheckRedirect 返回 ErrBlocked | **nil → RED** |
| PIN-16 | Redirect HTTPS→HTTP | CheckRedirect 返回 ErrBlocked | **nil → RED** |
| PIN-17 | Redirect 同 Origin | nil（允许） | nil（回归） |
| PIN-18 | Redirect 超过 5 次 | 次数错误，非 ErrBlocked | 现状一致（回归） |

### 8.5 Canary（CAN）

| ID | 场景 | 期望 | 当前预期 |
| --- | --- | --- | --- |
| CAN-01 | 专用 Resolver 正常；Canary 返回 `1.1.1.1` | `canary_invalid`；FakeIP 关；普通公网域名仍可解析 | 状态机不存在 → 编译 RED |
| CAN-02 | 专用 Resolver 超时 | `resolver_error`；不回退系统 DNS；域名请求失败 | 编译 RED |
| CAN-03 | Canary 返回空 A | `canary_invalid` | 编译 RED |
| CAN-04 | Canary 返回 `198.18.0.1` + `1.1.1.1` | `canary_invalid`（混入公网） | 编译 RED |
| CAN-05 | Canary 全部 A 在 `198.18/15` | `healthy`；FakeIP 开 | 编译 RED |
| CAN-06 | Canary NXDOMAIN（`*net.DNSError{IsNotFound:true}`） | `canary_invalid` | 编译 RED |
| CAN-07 | Canary SERVFAIL（`Err="server misbehaving"`） | `canary_invalid`；证据注明 RCODE 不可得 | 编译 RED |
| CAN-08 | 连接拒绝 | `resolver_error` | 编译 RED |
| CAN-09 | `fakeip_resolver` 配了域名/`127.0.0.11` | 配置校验拒绝 | 编译 RED |
| CAN-10 | 配置变化后旧 Guard 仍持旧状态；新 Guard 用新状态 | 旧不撤销、新不引用旧；in-flight 不承诺取消 | 编译 RED |

### 8.6 DNS Rebinding / Redirect / Dial 闭合（DNS）

| ID | 场景 | 期望 | 当前预期 |
| --- | --- | --- | --- |
| DNS-01 | 域名首次公网、二次私网 | Control 对每次候选 IP 拒绝私网 | 现有一半（`ssrf_test.go:84-101`），补 198.18/2001:2/100::/CGNAT |
| DNS-02 | 域名解析多 A，混入 `198.18.0.4` | 非 FakeIP Guard 拒绝该候选；公网候选可拨 | 198.18 当前放行 → RED |
| DNS-03 | 重定向到 `http://198.18.0.4/` | CheckRedirect ErrBlocked | 当前 `CheckIP` 放行 → RED |
| DNS-04 | 重定向到 `http://192.0.2.10/` | ErrBlocked | 放行 → RED |
| DNS-05 | 重定向到 `http://100.64.0.1/` | ErrBlocked | 放行（AllowPrivate=false 也放行，因为 CGNAT 不在 IsPrivate）→ RED |
| DNS-06 | v4/v6 强制档逐候选拨号 | 每个候选都过 `CheckIP`/`CheckIPStrict` | `dialContext:220-229` 已逐 IP 拨，但依赖 Dialer Control；补显式断言 |
| DNS-07 | HTTP 客户端 auto 模式域名拨号 | Control 收到的是解析后 IP，不是域名 | 现状成立（回归） |

### 8.7 五组边界（BND）

| ID | 断言 |
| --- | --- |
| BND-01 | `AllowPrivate=true` 不放行 `198.18/15`；FakeIP 不放行 `10/8` |
| BND-02 | URL 字面量 `198.18.0.4` 恒拒；域名解析到 `198.18.0.4` 仅在 discover healthy 时放行 |
| BND-03 | Pin 创建拒绝私网，不代表拨号安全；篡改 DB 后拨号仍拒 |
| BND-04 | Resolver 可用但 Canary 坏：公网域名可用、FakeIP 关；Resolver 坏：域名失败、不回退 |
| BND-05 | Registry 逻辑 Host 为公网，实际源解析到 `198.18/15` 仍拒 |

---

## 9. 兼容性基线（实施前必须补的测试）

在改 `CheckIP` 之前，先写以下“现状锁定”测试，证明我们知道旧行为是什么：

1. `COMPAT-01`：`AllowPrivate=false` 下 RFC1918、ULA、Loopback 全部拒绝（现有 `ssrf_test.go:16-43` 已覆盖大部分）。
2. `COMPAT-02`：`AllowPrivate=true` 下 RFC1918（10/8、172.16/12、192.168/16）放行。
3. `COMPAT-03`：`AllowPrivate=true` 下 ULA（`fc00::/7`）放行。
4. `COMPAT-04`：`AllowLoopback=true` 下 `127/8`、`::1` 放行。
5. `COMPAT-05`：`AllowPrivate=true` 下 `127.0.0.1` 当前放行（记录为历史越权，迁移后应变拒绝）。
6. `COMPAT-06`：`family_test.go:74`、`:97` 的 `AllowPrivate` 用法迁移到 `AllowLoopback` 后，auto/v4/v6 拨号测试仍通过。

只有 `COMPAT-01..04` 通过，才能开始改 `CheckIP`；`COMPAT-05` 是“确认旧行为”的负向证据，不是要保留的行为。

### 9.1 既有测试迁移清单（必须与产品改动同批提交）

| 文件:行 | 现状 | 迁移方式 |
| --- | --- | --- |
| `internal/safehttp/safehttp_test.go:23-30` | 断言 `198.18.0.4` 放行，理由是“本机 sing-box fakeip 上游” | 改为断言 `198.18.0.4` 拒绝；FakeIP 放行移到 discover 专用测试 |
| `internal/safehttp/family_test.go:74` | `AllowPrivate: true` 拨 `localhost` | 改为 `AllowLoopback: true`（可保留 `AllowPrivate`） |
| `internal/safehttp/family_test.go:97` | `AllowPrivate: true` 跑 RACE 目标 | 改为 `AllowLoopback: true` |
| `internal/gateway/pin_test.go:41-67` | 用 `127.0.0.1` 作为 pin，验证真实拨号 | 生产代码增加可注入 dialer/checker 的测试缝；该用例改用注入 dialer 保留“拨钉住 IP”语义；另加 `127.0.0.1` pin 必须 `ErrBlocked` 的用例 |
| `internal/gateway/pin_test.go:79-106` | 用 `127.0.0.2` / `::1` 验证 v4 失败回落 v6 | 同上，改用注入 dialer 模拟两地址结果 |
| `internal/gateway/pin_test.go:125-155` | 用 `192.0.2.1` / `127.0.0.1` 验证 1ms 竞速 | 同上；不得为了保留该测试而让 production 接受 TEST-NET/回环 pin |

迁移原则：**不为测试在 production 代码保留回环/TEST-NET 例外**；测试缝只存在于测试构造路径，生产 `CheckIPStrict` 不变。

---

## 10. 冻结文档逐项核对

### 10.1 设计冻结稿（`docs/3.5.24-design-freeze.md`）

| 锚点 | 原文要点 | 与本规格关系 | 判定 |
| --- | --- | --- | --- |
| §6 line 201 | “不默认放行私网/回环/链路本地/组播/保留地址” | `AllowPrivate` 默认 false；显式开启只放 RFC1918/ULA | 兼容 |
| §6 line 202 | “safehttp 完整保留不旁路，每跳重定向重查目标 IP” | Pin 增加完整 Origin + 拨号层严格检查，是收紧不是旁路 | 兼容 |
| §1 line 192 | “SourceEndpoint 支持 IP 不等于绕过安全策略” | gateway/probe/pin 不接 FakeIP；字面量恒拒 | 兼容 |
| §3 line 44 | HTTPS→HTTP 拒绝 | Pin Redirect 拒绝全部 Scheme 变化 | 兼容 |
| §2 line 26-33 | 凭据转发用完整 Origin（Scheme+Host+Port） | Pin Redirect 采用同一完整 Origin 口径 | 兼容 |
| §9 | 证据要求与生产授权边界 | 本规格只写文档与测试，不部署 | 兼容 |

**结论：未发现与设计冻结稿的硬冲突，不触发“停止实施”。**

### 10.2 任务书（`docs/3.5.24-unattended-delivery-task.md`）

| 锚点 | 原文要点 | 关系 | 判定 |
| --- | --- | --- | --- |
| §0.3 line 49 | “不默认放行私网、回环、链路本地、组播或保留地址” | 与冻结稿同口径，默认拒绝 | 兼容 |
| §2.4 line 255 | “私网、回环…继续由 safehttp 拒绝。该版本不通过关闭 safehttp 或新增旁路来支持内网 IP” | 该句位于“公网 IP 准入”功能段；`AllowPrivate` 是既有局域网探测/回源配置，不是本轮新增旁路。但字面“继续拒绝”未区分默认配置与显式开关 | **措辞张力，非冻结冲突** |
| §3.2 line 557 | “确保私网、回环、链路本地、组播和保留 IP 仍被拒绝” | 阶段 B 的验收项，语境是新增公网 IP 准入 | 同上 |
| §4.2 line 654 | “私网、回环、链路本地、组播、未指定和保留 IP 继续拒绝” | IP 安全与准入验收，默认配置口径 | 同上 |
| §12 line 1172 | checklist “私网、回环…仍拒绝” | 同上 | 同上 |

处理规则（写入验收口径，不改冻结文档）：

- §4.2 / §12 的“拒绝”以**默认配置**（`probe.allow_private=false`）为验收条件；这是 3.5.24 公网 IP 准入的 PASS 条件。
- `AllowPrivate=true` 只作为“既有局域网源”兼容性用例单独记录，不得用于证明 3.5.24 公网 IP 准入 PASS，也不得在验收报告中写成“私网仍拒绝”的通过证据。
- 若要求把任务书上述四处解释为“任何配置下都绝对拒绝”，则与本轮“保持 RFC1918/ULA 兼容”直接矛盾，必须改判并删除 `AllowPrivate` 的 RFC1918/ULA 语义；在未改判前，本规格按冻结稿“不默认放行”执行。

**结论：任务书存在 4 处绝对化措辞与既有 `AllowPrivate` 兼容性的解释差异，已记录；不与设计冻结稿冲突，不触发停止。实施证据中必须写明验收配置值，不能笼统写“私网仍拒绝”。**

---

## 11. 与早前 P2-2 口径的差异（取代记录）

| 早前口径 | 本轮最新裁定 | 执行 |
| --- | --- | --- |
| “`fc00::/18` 及其他 IPv6 ULA 继续拒绝”（若按字面适用于 `AllowPrivate`） | “`AllowPrivate` 只负责 RFC1918 和 IPv6 ULA” | 按最新：`AllowPrivate=true` 放行 ULA；默认、`CheckIPStrict`、FakeIP、Pin 下 ULA 仍拒绝 |
| “已配置 Resolver + 未配置 Canary → 配置级信任、允许 FakeIP” | “`healthy` 验证成功，才授予限定 FakeIP 权限” | 按最新：未配置 Canary 即不授予 FakeIP；运行期仍三态 |
| “Pin 只修 pinned 路径” | “Pin 创建和实际拨号都必须 public-only” | 按最新：含过渡期回落，`clientFor` 不再复用 `privClient` |
| “`198.18` 仅 discover 域名拨号例外” | 同左，且明确 gateway/probe/Pin/默认 Guard 不接 | 一致 |

上述差异不修改冻结文档；若后续发现冻结文档与最新裁定再次冲突，按用户指令停止并报告锚点。

---

## 12. 非目标（本轮不做）

- 不实现 P1-2 双身份模型、Registry 自定义节点入口或 UI。
- 不实现运行期热撤销、Canary 定时重验、配置热更新。
- 不引入第三方 DNS 库；不改 DB Schema、不改凭据格式、不改 AEAD AAD、不改 Cache Epoch。
- 不推送、不合并 main、不发布镜像、不部署生产。
- 不顺手放宽任何 IANA 特殊段；不把 `192.0.0.0/24` 中的 globally reachable 子段单独放行。
- 不修改 `docs/3.5.24-design-freeze.md` 与任务书；本文件只记录解释与差异。

---

## 13. 证据与门禁

实施与 RED/GREEN 必须产出（当前已产出的第一批）：

- `docs/evidence/3.5.24/p2-2-compat.log`：COMPAT-01..04 的现状证据（当前 PASS）。
- `docs/evidence/3.5.24/p2-2-red-safehttp.log`：AP/LP/FP 字面量与边界用例的失败输出（当前 5 个测试 FAIL）。
- `docs/evidence/3.5.24/p2-2-red-api.log`：Pin 创建阶段保留段用例的失败输出（当前 FAIL）。
- `docs/evidence/3.5.24/p2-2-red-gateway.log`：Pin 回环 pin、同 Host 异端口、跨 Host 重定向用例的失败输出（当前 3 个 FAIL）。
- 后续仍需补齐：`gate-p2-2-green.log`、`gate-p2-2-canary.log`、`gate-p2-2-wiring.log`、全仓 `gofmt`/`go build`/`go test ./...`/`go test -race ./...` 原始日志。
- 所有提交留在 `3.5.24-impl`；不推送。

门禁判定：

- 任一 RED 未先失败 → 证据无效。
- 任一 AP/LP/FP/PIN/CAN/DNS/BND 条目失败 → P2-2 保持 `NOT_CLOSED`。
- `RELEASE_READY` 仅在 P1-2、P2-5、P2-6 全部关闭、全量门禁通过、独立复核 PASS 后才可能成立；本轮不得标记。

---

## 14. 状态

- P2-2：`NOT_CLOSED`
- 本规格：`SPEC_WRITTEN`
- RED 测试：`PARTIAL_WRITTEN`（AP/LP/FP 字面量与边界、PIN 创建/拨号/同 Host 异端口/跨 Host；FP 域名例外、CAN、DNS 注入矩阵未写）
- 代码实施：`NOT_STARTED`
- P1-2：`NOT_STARTED`（P2-2 关闭前不得开工）
- FULL_TESTS / RACE / 独立复核：`NOT_RUN`
- `RELEASE_READY`：不成立
