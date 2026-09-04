# Resin 订阅级前置代理（Subscription-Level Upstream Proxy）设计文档

- 日期：2026-09-03
- 状态：已确认，待实施
- 仓库：`D:\data\code\Resin`（MIT,自维护 fork）
- 前置关联：PR #80「一个环境变量,全局单层链」（`muqing-kg/Resin#80`，`codex/feat-upstream-proxy` 分支）。本文档复用其核心代码（`internal/outbound/upstream_proxy.go` 的 URI 解析/构建/注册/DNS transport），在此基础上扩展为订阅级 + 节点引用。

---

## 1. 背景

### 1.1 需求来源

- 存在一种常见场景：Resin 本机**直连不到部分国外订阅节点服务器**（网络策略/路由不可达），需要把"本机 → 节点服务器"这一段经一个可达的**前置中转代理**垫过去；
- 此前 PR #80 提出的全局 env 层方案（`RESIN_UPSTREAM_PROXY`）能解决"全部节点垫一层"，但它是**进程级写死**的：更换前置需重启 Resin；也缺少按订阅区分的灵活性。
- 平台级（per-platform）前置经分析成本高：节点 outbound 是**全局共享单实例**（`NodeEntry.Outbound`，`internal/node/entry.go`），detour 是构建期烧进 dialer 的属性，运行期无法按请求切换；per-platform 需要按（平台×节点）克隆 outbound + 探测/延迟按平台维度改造，属结构性重构（完整版约 5–9 人天，缩水版约 2–4 人天且背着架构包袱）。

### 1.2 方案选型

- **方案 A：全局 env 层（PR #80）**——简单（约 0.5–1 人天），但写死、需重启、无按订阅区分；
- **方案 B（选定）：订阅级前置**——前置配置落在**订阅实体**上；借助"node hash = RawOptions 的哈希"这一既有性质（`internal/node/hash.go`），在解析期把 `detour` 注入 RawOptions → 每个订阅天然获得独立节点身份（独立 outbound/探测/健康状态），**绕开**平台级"共享单实例 outbound"的结构难题；更换前置走订阅刷新路径→**热更换、无需重启**；预计 **~4–5 人天**（含节点引用支持）。
- **方案 C：平台级（per-platform）**——语义最准（按平台路由），但代价最高、含结构性重构风险，本期不做。

选自方案 B。PR #80 的 `upstream_proxy.go` 核心代码（URI 解析、outbound 构建注册、域名 DNS transport）直接复用，在此基础上做参数化扩展（per-sub tag、动态生命周期、节点引用）。

---

## 2. 语义定义

- **配置字段**：订阅级 `upstream_proxy`（字符串，可空）。空 = 该订阅节点保持现状（直连节点）；非空 = 该订阅全部节点经此前置中转去"够着"节点服务器。
- **输入格式**（两种）：
  - **外部代理 URI**：`socks5/http/https://[user:pass@]host:port`，或裸 `host:port`（默认 http）；
  - **节点引用**：`ref:<node-hash>`——引用 Resin 全局节点池里另一个节点的 outbound 作为前置（需该节点已存在且 outbound 已构建）。
- **链路**：`本地 Resin →（经前置中转拨号）→ 节点服务器 →（节点服务器自身出网）→ 目标服务`。目标看到的出口 IP = **节点服务器 IP**（不是前置中转的 IP；`detour` 只作用于"拨节点服务器"这一段，sing-box 源码可证：`protocol/vmess/outbound.go:158` 用 `h.dialer.DialContext(..., h.serverAddr)` 拨的是节点自己的服务器地址）。
- **三层优先级（低→高）**：全局 env 层（若 PR #80 将来落地）→ 订阅级 `upstream_proxy` → 节点自带 `detour`（订阅内容里 `dialer-proxy`）。**节点自带最高；订阅级不覆盖已有 `detour`**。
- **每订阅节点身份**：解析期注入后，`detour` 成为 RawOptions 的一部分 → node hash 变化 → 同一物理节点在不同订阅（或同一订阅不同前置下）得到不同 hash → 独立 outbound 实例、独立探测/延迟/健康状态。这**绕开**了"NodeEntry 单 outbound 实例"的共享冲突（这正是平台级方案的结构性难点）。
- **对"每 IP 账号配额"的影响**：无。配额键值（`GetEgressIP`、`IPAccountWindow`、`IPLoadStats`）全部按**节点出口 IP** 计数，而 detour 不改出口语义（出口仍是节点服务器 IP）。多订阅共享同一物理服务器时，egress IP 相同，配额按 IP 去重，不会重复占额。（例外：前置中转自身做强制出口改写时才失真——那是上游行为，非本设计职责。）
- **热更换**：改 `upstream_proxy` → bump `configVersion` → 走常规订阅刷新（定时/手动）→ 重新下载/重解析/重建节点。无需重启 Resin；代价为**整订阅节点一轮 churn**（见§6）。

---

## 3. 配置格式

字段：`model.Subscription.upstream_proxy`（JSON: `upstream_proxy`，state.db `subscriptions` 表列 `upstream_proxy TEXT NOT NULL DEFAULT ''`）。

支持两种输入格式（二选一）：

### 3.1 外部代理 URI

格式与 PR #80 语法一致（`parseUpstreamProxyURL`，`internal/outbound/upstream_proxy.go`）：

```
socks5://[user:pass@]host:port     # SOCKS5 代理
socks5h://[user:pass@]host:port    # SOCKS5 + 远端域名解析
http://[user:pass@]host:port       # HTTP 代理
https://[user:pass@]host:port      # HTTPS 代理
[user:pass@]host:port              # 裸 host:port，默认按 http 处理（端口默认：http=80, https=443, socks=1080）
```

### 3.2 节点引用

引用 Resin 全局节点池里另一个节点的 outbound 作为前置：

```
ref:<node-hash-hex>                # 32 位十六进制 node hash
```

示例：`ref:a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4`。该节点必须已存在于池中且 outbound 已构建；若节点不存在或 outbound 未就绪，订阅刷新时报错（可从 `last_error` 查看）。

校验：非法格式 / 不支持的协议 / 引用节点不存在 → API 层拒绝或刷新时记录错误，不落库（URI）或落库后刷新失败报错（ref）。

---

## 4. 架构与数据流

###  ️4.1 解析期注入（核心）

```
订阅内容 → ParseGeneralSubscription → ApplyUpstreamDetour(nodes, upstream, subID)
   upstream 为空 → 原样返回
   upstream 为 URI → detourTag = "__resin_sub_upstream_<subID>"
   upstream 为 ref:<hash> → detourTag = 注册 tag（见 4.2/4.3）
   每个 ParsedNode：
     已有 detour ？→ 跳过（节点级优先）
     否则 → 写 detour = detourTag → 重 marshal → 作为该订阅新 RawOptions
→（hash 随 RawOptions 变化 → 每订阅节点身份）
→ AddNodeFromSub / DiffHashes 应用池
```

###  ️4.2 订阅级前置 outbound 生命周期（URI 模式）

- `SingboxBuilder` 增加导出方法：
  - `RegisterDetourOutbound(tag string, raw json.RawMessage) error`——把前置 raw 构建成 sing-box outbound 并注册进 builder 内部那台 outbound manager（`sbOutbound.NewManager`，`internal/outbound/builder.go:76`）；节点 outbound 的 `DetourDialer` 在**首次拨号时**才按 tag 查 manager，故**必须先注册、后构建节点 outbound**。
  - `UnregisterDetourOutbound(tag)`——移除并关闭。
- 底层复用 PR #80 的 `createUpstreamProxy()`（`upstream_proxy.go`），参数化 tag（PR #80 写死 `__resin_upstream_proxy`，我们传 per-sub tag）。
- **域名前置解析**：前置 outbound 的 server 为域名时，**注册独立本地 DNS transport**（`C.DNSTypeLocal`，系统解析器），tag 为 `__resin_sub_dns_<subID>`——复用 PR #80 的 `setUpstreamProxyDomainResolver()`。不用默认 DoH 链，避免循环（前置域名经 DoH 解析 → DoH 经节点 outbound → 节点又 detour 到前置 → 鸡生蛋）。
- **刷新/换前置顺序**：先 `RegisterDetourOutbound` 新上游（仅 URI 模式）→ 再 `DiffHashes` 增删池（新 hash 加入；旧 hash 移除并关闭旧节点 outbound）→ **最后** `UnregisterDetourOutbound` 旧上游（保证旧节点 outbound 全部关闭后才注销，避免悬空 detour 引用）。ref 模式不调用 `RegisterDetourOutbound`——前置节点 outbound 由 `EnsureNodeOutbound` 注册进 sbOutbound manager。
- **删订阅**：先移除节点（`RemoveSubscriptionID`→池清理、节点 outbound 关闭）→ 再 `UnregisterDetourOutbound` 其前置（URI 模式）；ref 模式无需额外注销（节点 outbound 的注销由其自身订阅管理）。
- **Bootstrap**：载入 subscriptions → 对每个带 **URI** upstream 的订阅 `RegisterDetourOutbound` → 再恢复节点 / `WarmupAll`（先注册后构建）。ref 模式不调用 `RegisterDetourOutbound`——前置节点的 outbound 在 `WarmupAll` 阶段由 `EnsureNodeOutbound` 自动注册进 sbOutbound manager。

###  ️4.3 节点引用模式（`ref:<hash>`）

当 `upstream_proxy` 为节点引用时，前置目标是 Resin 池里另一个节点的 outbound——不需要构建新的外部代理 outbound，而是把**已有节点 outbound 注册进 sbOutbound manager**，使其可被 detour 解析。

- **注册**：在 `OutboundManager.EnsureNodeOutbound` 构建节点 outbound 后，**同步注册进 sbOutbound manager**（tag = 节点 hash hex）。`RemoveNodeOutbound` 时同步注销。
- **引用订阅刷新时**：解析 `ref:<hash>` → 查找池里该节点 → 确认 outbound 已构建 → 用该节点 hash 作为 detourTag 注入。若节点不存在或 outbound 未就绪，刷新报错。
- **构建顺序**：被引用的节点（"前置节点"）必须先于引用它的订阅的节点完成 outbound 构建。Bootstrap 时：先 `WarmupAll` 所有节点 outbound（含前置节点）→ 再做订阅刷新（此时前置节点 outbound 已就绪）。订阅刷新时：前置节点 outbound 由 `OnNodeAdded` 回调触发 `EnsureNodeOutbound`，在引用它的节点构建前完成（因为前置节点来自其他订阅，已在池中且 outbound 已构建）。
- **循环检测**：`ref:<hash>` 引用的节点自身若有 `upstream_proxy`（又指向另一个 ref），需检测环路。实现：从当前订阅出发沿 **upstream 引用链** BFS——`ref:<hash>` → 查该节点所属**全部订阅**（`NodeEntry.SubscriptionIDs()`，节点可属多订阅）→ 逐个检查其 `upstream_proxy` → 若为 `ref:` 继续追踪 → 回到已访问订阅或深度超 10 则拒绝并报错。仅 `ref:` 格式触发检测（URI 外部代理不构成 Resin 内部环路）。

---

##  ️5. 改动点（文件级）

|层 |文件 |改动 |复用/新增 |
|---|---|---|---|
| 领域模型 | `internal/model/models.go` | `Subscription` 加 `UpstreamProxy string \`json:"upstream_proxy"\`` |新增 |
| 运行时订阅 | `internal/subscription/subscription.go` |字段（mu 保护）+getter/setter；`SetUpstreamProxy` bump `configVersion` |新增 |
| 持久化 | `internal/state/schema.go` + `migrate.go` + subscription repo |`subscriptions` 加列 `upstream_proxy TEXT NOT NULL DEFAULT ''`；CRUD 读写 |新增 |
| 解析注入 | `internal/subscription/parser.go`（或新 helper） |`ApplyUpstreamDetour(nodes, upstream, subID)` |新增 |
| **URI 解析/构建** | `internal/outbound/upstream_proxy.go` |**复用** PR #80 的 `parseUpstreamProxyURL`、`createUpstreamProxy`（tag 参数化）、`setUpstreamProxyDomainResolver`、`injectUpstreamProxyDetour` |复用+扩展 |
| outbound 生命周期 | `internal/outbound/builder.go` |`RegisterDetourOutbound` / `UnregisterDetourOutbound`；保留 PR #80 的 env 层注入 |扩展 PR #80 |
| **节点注册** | `internal/outbound/manager.go` |`EnsureNodeOutbound` 构建后同步注册进 sbOutbound manager（tag=hash hex）；`RemoveNodeOutbound` 时同步注销 |新增 |
| 循环检测 | `internal/subscription/upstream_validate.go`（新） |`ref:<hash>` 沿 detour 图 BFS 检测环路 |新增 |
| 接线 | 订阅刷新/增删路径 + `cmd/resin` 装配 |按 §4.2/§4.3 顺序接线 |扩展 |
| API | `internal/api/handler_subscription.go` |create/update/list 增字段 + 格式校验（URI / ref） |新增 |
| WebUI | `webui/src/features/subscriptions/{types.ts, api.ts, SubscriptionPage.tsx}`, `webui/src/i18n/translations.ts` |新建+编辑+详情三处加“前置代理（可选）”输入框（手填 hash / URI），照 `user_agent` 范式；i18n 文案；留空=直连 |新增 |
| 文档 |README + 本设计文档 + plan 文档 |新增"订阅级前置代理"节；含"明确不做"声明 |新增 |

---

## 6. 边界与代价

- **换前置 = 整订阅节点重建 + 一轮刷新**：旧 hash 退池、新 hash 进池、租约按正常失效/重建重排、探测重来。已确认接受（一次性 churn）。
- **生效触发**：改前置后于**下一次定时/手动刷新**生效（需能下载到内容）；若源不可用，配置先落库、生效顺延。已确认。
- **免下载立即重建**：本期不做（后续增量可基于缓存 content 重建）。
- **同配置多订阅**：每订阅独立 outbound 实例（tag 按 `__resin_sub_upstream_<subID>`），开销可忽略，删除/隔离干净。已确认。
- **凭证明文落库**：URI 若带 `user:pass` 明文存 state.db（与现状 URL/Content 明文存储风格一致）；若部署于共享/不可信环境，后续改独立 secret 存储。已确认。
- **删除即断链**：订阅删除/禁用 → 其前置 outbound 一并注销（移除节点之后）。已确认。
- **节点引用的额外风险**：
  - 被引用节点故障（熔断/健康检查失败）→ 依赖它的订阅全部受影响。后续可观测化（监控前置节点状态→受影响订阅列表）；本期记录日志。
  - 被引用节点的订阅被删除/禁用 → 其 outbound 关闭 → 依赖它的订阅的节点 detour 解析失败（下次拨时报错）。本期不自动级联清理（YAGNI），后续按需加。
  - **DetourDialer stale cache**：被引用节点因自身订阅刷新而重建 outbound 时，sbOutbound manager 里的实例被替换，但引用它的节点的 `DetourDialer` 在首次拨号时已缓存旧引用（`common/dialer/detour.go:44` `initOnce`）。后续拨号会短暂使用已关闭的旧 outbound 并失败，直到该节点自身也被重建（整订阅 churn 的一部分）——这是 sing-box 的预期行为，一次性瞬态，非持续故障。

---

## 7. 明确不做（本期）

- 不做全局 env 层（PR #80 本期不合入；将来若落地，为**最低优先级兜底**，本设计的注入逻辑（"已有 detour 即跳过"）天然兼容三层优先级）；
- 不做 per-node / per-platform 前置选择（平台级需求届时另行评估，成本与架构改动已知：完整版约 5–9 人天）；
- 不做多前置负载均衡；
- 不做"换前置免下载立即重建"（后续增量）；
- 不做节点引用的级联清理（被引用节点删除时自动清理引用方——YAGNI）；
- 不做 WebUI 节点下拉选择器（本期手填 hash/URI，后续增量）；
- 不改动 egress/配额/负载/区域的统计口径（保持现状）。

---

## 8. 测试计划

|测试 |内容 |
|---|---|
| parser 单测 |upstream 为空 → 原样；已有节点级 detour → 不覆盖；URI 注入后 hash 变化；ref 注入后 hash 变化；每订阅 tag 独立 |
| URI 解析单测 |`parseUpstreamProxyURL`：socks5/http/https/裸 host:port（默认 http）/带认证/非法格式/端口默认值 |
| builder 单测 |`RegisterDetourOutbound` 后节点 outbound 首拨 detour 解析成功；`UnregisterDetourOutbound` 后 detour 解析失败（预期）；未注册 → 报错且不 panic；域名上游 → 本地 DNS transport 已注册 |
| manager 单测 |`EnsureNodeOutbound` 后节点 outbound 已注册进 sbOutbound manager（tag=hash hex，可被 detour 解析）；`RemoveNodeOutbound` 后注销 |
| 循环检测单测 |A→B→A 检测到环并拒绝；A→B→C→无环通过；深度超限拒绝 |
| 生命周期 e2e |改前置（URI→URI）：先注册新→池增删→后注销旧（顺序断言）；改前置（URI→ref）：注销旧 URI outbound→注册 ref；删订阅：先移除节点→后注销前置 outbound；Bootstrap：先注册→后 WarmupAll |
| 契约 API |字段读写；非法 URI 拒绝；非法 ref 格式拒绝；默认空兼容 |
| 迁移 |旧库加列默认空 → 现有行为零变化（硬验收） |
| 配额回归 |同服务器多订阅不同前置时，`max_accounts_per_ip`/egress 统计按 IP 去重不重复占额 |
| WebUI |新建/编辑/详情三处字段展示与提交一致性 |

---

## 9. 验收标准

1. **无 upstream_proxy 的订阅**：全部行为与现状 100% 一致（路由、租约、配额、探测均不变）。
2. **URI 前置**：配置 `socks5/http/https://…` 后，该订阅全部节点的"拨节点服务器"一段均经此前置中转；目标看到的仍是各节点服务器出口 IP。
3. **节点引用前置**：配置 `ref:<hash>` 后，该订阅全部节点经被引用节点的 outbound 拨出；被引用节点不存在/outbound 未就绪时刷新报错。
4. **节点身份独立**：同一物理节点在不同订阅（或不同前置下）为不同 hash、独立 outbound/探测/健康状态。
5. **每 IP 配额/负载/区域口径不变**（前置不改出口语义）。
6. **热更换**：修改 upstream_proxy 后经一轮订阅刷新生效，无需重启 Resin（接受整订阅一次 churn）。
7. **循环检测**：`ref:<hash>` 引用形成环路时，拒绝配置并报明确错误。
8. **优先级**：节点自带 detour > 订阅级 >（将来）全局 env 层；均有文档与测试覆盖。
9. **无 upstream 行为零变化**（迁移硬验收）。

---

## 10. 决策记录（2026-09-03 已确认）

|# |决策点 |结论 |
|---|---|---|
|1 |本期范围 |订阅级前置（URI + 节点引用）；env 全局层后期 |
|2 |WebUI |本期做；新建+编辑+详情三处，手填 hash/URI，照 user_agent 范式 |
|3 |文档 |随代码同步补（README 一节 + 本文档 + plan 文档） |
|4 |换前置代价 |接受"整订阅节点重建 + 一轮刷新" |
|5 |生效触发 |走常规订阅刷新（定时/手动） |
|6 |同配置多订阅 |每订阅独立 outbound 实例 |
|7 |删除/禁用订阅 |移除节点后注销其前置 outbound（删除即断链） |
|8 |凭证明文落库 |与现有 URL/Content 一致（明文）；安全增强另议 |
|9 |裸 host:port |**默认按 http 处理**（与 PR #80 一致；端口默认 http=80） |
|10 |域名前置 DNS |注册独立本地 DNS transport（`C.DNSTypeLocal`，系统解析器），避免 DoH 循环（复用 PR #80 `setUpstreamProxyDomainResolver`） |
|11 |节点引用格式 |`ref:<hash>`，手填 hash hex，后续增量加 UI 下拉 |
|12 |节点 outbound 注册 |`EnsureNodeOutbound` 后同步注册进 sbOutbound manager（tag=hash hex），使可被 detour 解析 |
|13 |循环检测 |沿 upstream 引用链 BFS 检测环路（遍历节点所属**全部订阅**），深度上限 10；仅 ref 模式触发 |
|14 |代码复用 |直接复用 PR #80 的 `upstream_proxy.go` 核心函数，参数化 tag + 动态生命周期扩展 |
|15 |协作方式 |全部一次实现，完成后整包 review |
|16 |DetourDialer 缓存 |接受节点重建后短暂 stale（一次性 churn 范围内，非持续故障） |
|17 |多订阅归属节点循环检测 |遍历节点所属全部订阅的 upstream（非仅第一个） |