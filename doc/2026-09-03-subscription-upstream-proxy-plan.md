# 订阅级前置代理 — 实施计划

- 设计文档：`doc/2026-09-03-resin-subscription-upstream-proxy-design.md`
- 总预估：~4–5 人天
- 依赖：PR #80 `internal/outbound/upstream_proxy.go`（`muqing-kg/Resin` `codex/feat-upstream-proxy` 分支）

---

## Task 0：移植 PR #80 核心代码（`upstream_proxy.go`）

**描述**：从 PR #80 复制 `internal/outbound/upstream_proxy.go` 到本地仓库，做以下适配：
- `createUpstreamProxy()` 的 tag 参数化（PR #80 写死 `__resin_upstream_proxy`；改为 `tag string` 入参，env 层调用仍传 `__resin_upstream_proxy`）
- `upstreamProxyLocalDNSTransportTag` 同样参数化（per-sub 传 `__resin_sub_dns_<subID>`）
- 保留 PR #80 的 `parseUpstreamProxyURL`、`setUpstreamProxyDomainResolver`、`injectUpstreamProxyDetour` 原样不动

**文件**：`internal/outbound/upstream_proxy.go`（新增，从 PR #80 移植）

**测试**：
- `parseUpstreamProxyURL`：socks5/http/https/裸 host:port（默认 http）/socks5h/带认证/非法格式/端口默认值
- `createUpstreamProxy`：tag 参数化后注册成功；域名上游 → 本地 DNS transport 已注册

**验收**：URI 解析结果与 PR #80 行为一致；tag 参数化后 env 层调用和 per-sub 调用均正常

---

## Task 1：模型 + 持久化 + 订阅运行时字段

**描述**：
1. `internal/model/models.go`：`Subscription` 加 `UpstreamProxy string \`json:"upstream_proxy"\``
2. `internal/subscription/subscription.go`：加 `upstreamProxy` 字段（mu 保护）+ `UpstreamProxy()` getter + `SetUpstreamProxy(v string)` setter（值变化时 bump `configVersion`）
3. `internal/state/schema.go` + `migrate.go`：`subscriptions` 表加列 `upstream_proxy TEXT NOT NULL DEFAULT ''`
4. subscription repo（`repo_state.go` 或相关）：CRUD 读写 `upstream_proxy`

**文件**：`internal/model/models.go`, `internal/subscription/subscription.go`, `internal/state/schema.go`, `internal/state/migrate.go`, subscription repo

**测试**：
- setter/getter 线程安全；configVersion bump 断言
- 旧库迁移：加列默认空 → 现有 subscription 行零影响
- repo CRUD roundtrip

**验收**：字段持久化/恢复正确；configVersion bump 可触发刷新

---

## Task 2：解析期注入（`ApplyUpstreamDetour`）

**描述**：在 `internal/subscription/parser.go`（或新 helper `internal/subscription/upstream_inject.go`）加：
```go
func ApplyUpstreamDetour(nodes []ParsedNode, upstream string, subID string) []ParsedNode
```
逻辑：
- upstream 为空 → 原样
- upstream 以 `ref:` 前缀 → 解析 hash，detourTag = hash hex（§4.3 模式）
- 否则（URI）→ detourTag = `"__resin_sub_upstream_" + subID`
- 对每个 ParsedNode：unmarshal raw → 若已有 `"detour"` 键 → 跳过；否则写 `detour = detourTag` → re-marshal

**文件**：`internal/subscription/parser.go` 或 `internal/subscription/upstream_inject.go`

**测试**：
- 空 upstream → 原样，hash 不变
- URI upstream → 注入 detour，hash 变化，tag 格式正确
- ref upstream → detour = hash hex，hash 变化
- 已有节点级 detour → 不覆盖，hash 不变
- 两个订阅不同 upstream → 不同 hash（同一物理节点身份独立）

**验收**：注入逻辑正确，节点级优先

---

## Task 3：SingboxBuilder 动态 Register / Unregister

**描述**：`internal/outbound/builder.go` 加导出方法：
- `RegisterDetourOutbound(tag string, raw json.RawMessage) error`：调用 `createUpstreamProxy(ctx, outboundMgr, dnsTransportMgr, logger, raw)`（参数化 tag），返回 error。若已注册同 tag → 先注销再注册（幂等）。
- `UnregisterDetourOutbound(tag string)`：从 outboundMgr 移除并关闭 outbound；若为域名上游，同步注销其 DNS transport。**注意**：sing-box `adapter.OutboundManager` 接口可能无 per-outbound `Remove` 方法——实现前先确认；若无则改为只 `Close()` outbound 实例（不从 manager 移除），同 tag 再次 Register 时幂等覆盖。
- `Close()` 扩展：关闭时清理所有 per-sub 注册的上游 outbound（维护一个注册 tag 集合）。

底层复用 Task 0 的 `createUpstreamProxy`（参数化 tag）。

**文件**：`internal/outbound/builder.go`

**测试**：
- Register 后节点 outbound detour 解析成功
- Unregister 后 detour 解析失败（预期）
- 同 tag 重复 Register → 幂等（旧注销+新注册）
- Close() 清理所有注册

**验收**：动态注册/注销生命周期正确，线程安全

---

## Task 4：节点 outbound 注册进 sbOutbound manager（节点引用基础）

**描述**：改动 `internal/outbound/manager.go`：
- `EnsureNodeOutbound`：构建 outbound 成功后，同步调用 `builder.ctx` 里的 sbOutbound manager `Create(ctx, logger, hash.Hex(), outboundType, outboundOptions)` 注册。这里需要 builder 暴露一个 `RegisterNodeOutbound(hash, outbound)` 方法（或暴露 sbOutbound manager 的注册能力），避免 manager 直接依赖 builder 内部。
  - 方案：SingboxBuilder 加 `RegisterBuiltOutbound(tag string, outbound adapter.Outbound)` 方法——直接注册已有 outbound 实例进 sbOutbound manager（绕开 Create 的重建逻辑）。需确认 sbOutbound manager 支持"注册已有实例"（否则改为通过 builder 的 ctx 用 `outboundMgr.Create` 重建一次——浪费但简单；优选确认 API）。
- `RemoveNodeOutbound`：同步注销 sbOutbound manager 里的对应 tag。

**文件**：`internal/outbound/builder.go`, `internal/outbound/manager.go`

**测试**：
- `EnsureNodeOutbound` 后，通过 sbOutbound manager 可以 `Outbound(hash.Hex())` 取到该节点 outbound
- `RemoveNodeOutbound` 后，`Outbound(hash.Hex())` 返回 not found
- 多节点并发构建/注销不 panic

**验收**：节点 outbound 可被 detour 解析到（`ref:<hash>` 场景的基础）

---

## Task 5：订阅刷新 / 删除 / Bootstrap 接线

**描述**：在订阅刷新/增删路径（`internal/topology` 或 control-plane 订阅服务层）和 `cmd/resin/main.go` 中接入上游生命周期。

接线顺序（见设计文档 §4.2/§4.3）：
- **刷新/换前置**：
  1. 计算新旧 upstream 差异
  2. 新 upstream 非空 → `RegisterDetourOutbound`（URI 模式）或确认 ref 节点 outbound 已就绪（ref 模式）
  3. `ApplyUpstreamDetour` → `DiffHashes` → 池增删（旧 hash 移除，旧节点 outbound 关闭）
  4. 旧 upstream 非空且与新不同 → `UnregisterDetourOutbound` 旧 tag
- **删订阅**：
  1. 移除节点（`RemoveSubscriptionID` → 池清理 → 节点 outbound 关闭 → `RemoveNodeOutbound`）
  2. `UnregisterDetourOutbound` 其前置 tag（URI 模式）；ref 模式无需额外注销（节点 outbound 的注销由其自身订阅管理）
- **Bootstrap**（`cmd/resin/main.go` `bootstrapTopology` / `bootstrapNodes`）：
  1. 载入 subscriptions
  2. 对每个带 **URI** upstream 的订阅 → `RegisterDetourOutbound`
  3. 载入节点 / `WarmupAll`（节点 outbound 构建 + 注册进 sbOutbound manager）
  4. 恢复 subscription bindings
  - **ref 模式不调用 `RegisterDetourOutbound`**——前置节点的 outbound 在 `WarmupAll` 阶段由 `EnsureNodeOutbound` 自动注册进 sbOutbound manager。

**文件**：`internal/topology/`（订阅刷新路径）、`cmd/resin/main.go`

**测试**：
- 刷新（URI→URI）：先注册新→池增删→后注销旧（顺序断言）
- 刷新（URI→ref）：注销旧 URI outbound→确认 ref 节点就绪→注入
- 删订阅：先移除节点→后注销前置 outbound
- Bootstrap：先注册→后 WarmupAll（顺序正确）
- ref 引用节点 outbound 未就绪 → 刷新报 last_error

**验收**：生命周期顺序正确，无悬空 detour 引用

---

## Task 6：循环检测

**描述**：新文件 `internal/subscription/upstream_validate.go`，函数：
```go
func ValidateUpstreamRefNoCycle(subID string, upstream string, subManager *topology.SubscriptionManager, pool outbound.PoolAccessor) error
```
- 仅对 `ref:<hash>` 格式执行
- 从当前订阅出发，沿 **upstream 引用链** BFS：`ref:<hash>` → 查该节点所属**全部订阅**（`NodeEntry.SubscriptionIDs()`，节点可属多订阅）→ 逐个检查其 `upstream_proxy` → 若为 `ref:` 继续追踪 → 回到已访问订阅或深度超 10 → 返回错误
- URI 类型的 upstream 不参与环检测（外部代理不会形成 Resin 内部环路）

**文件**：`internal/subscription/upstream_validate.go`（新增）

**测试**：
- A→B→A 检测到环，拒绝
- A→B→C→URI 无环，通过
- 节点 X 属于 B 和 C 两个订阅，B 的 upstream=ref:Y→Y 属于 A（upstream=ref:X）→ 环，拒绝
- 深度 11 超限，拒绝
- 无 ref 的纯 URI 配置不触发检测

**验收**：环路配置被拒绝并报明确错误

---

## Task 7：API（handler_subscription.go）

**描述**：`internal/api/handler_subscription.go` 的 create/update/list 增 `upstream_proxy` 字段：
- create/update 接受 `upstream_proxy` 字符串（可空）
- 校验：非空时检查格式（URI 用 `parseUpstreamProxyURL` 校验；ref 用 hex 格式 + 长度 32 校验）
- list 返回 `upstream_proxy` 字段
- 契约测试补齐

**文件**：`internal/api/handler_subscription.go`

**测试**：
- create 带合法 URI → 成功
- create 带合法 ref → 成功
- create 带非法格式 → 400 拒绝
- update 改 upstream → 成功，configVersion 变化
- list 包含 upstream_proxy 字段
- 默认空兼容

**验收**：CRUD + 校验正确

---

## Task 8：WebUI

**描述**：`webui/src/features/subscriptions/` 三处加"前置代理（可选）"输入框：
- `types.ts`：`Subscription` 类型加 `upstream_proxy: string`
- `api.ts`：create/update payload 包含 `upstream_proxy`
- `SubscriptionPage.tsx`：
  - `subscriptionCreateSchema` 加 `upstream_proxy: z.string()`
  - 新建弹窗：`create-sub-upstream-proxy` 输入框（照 `user_agent` 同区）
  - 编辑表单：`edit-sub-upstream-proxy` 输入框（照 `user_agent` 同区）
  - 详情面板：展示 `upstream_proxy`（可选，照 `user_agent` 展示）
  - defaultValues 两处（新建/编辑）加 `upstream_proxy: ""`
  - payload 映射两处加 `upstream_proxy`
  - helptext：`留空=直连节点；支持 socks5/http/https://… URI 或 ref:<节点hash>（32位十六进制）`
- i18n：`translations.ts` 加对应中英文

**文件**：`webui/src/features/subscriptions/{types.ts, api.ts, SubscriptionPage.tsx}`, `webui/src/i18n/translations.ts`

**测试**：手动验证新建/编辑/详情展示一致性

**验收**：UI 字段可填/可保存/可展示；留空=直连

---

## Task 9：文档

**描述**：
- `README.zh-CN.md` + `README.md`：新增"订阅级前置代理"一节（配置说明、格式、三层优先级、明确不做）
- 设计文档已落（`doc/2026-09-03-resin-subscription-upstream-proxy-design.md`）

**文件**：`README.zh-CN.md`, `README.md`

**验收**：文档覆盖配置格式、使用示例、边界说明

---

## 依赖关系

```
Task 0 (PR #80 移植)
  ↓
Task 1 (模型/持久化)        ← 无其他依赖，可与 Task 0 并行
  ↓
Task 2 (解析注入)           ← 依赖 Task 1（需要 upstream 字段）
  ↓
Task 3 (Builder Register)   ← 依赖 Task 0（createUpstreamProxy）
  ↓
Task 4 (节点注册 manager)   ← 依赖 Task 3（builder 注册能力）
  ↓
Task 5 (接线)               ← 依赖 Task 2 + Task 3（ref 还依赖 Task 4）
  ↓
Task 6 (循环检测)           ← 仅依赖 Task 1（subscription manager + pool stub 可测），不依赖 Task 5 完整接线
  ↓
Task 7 (API)                ← 依赖 Task 1（model 字段）
  ↓
Task 8 (WebUI)              ← 依赖 Task 7（API 可用）
  ↓
Task 9 (文档)               ← 最后
```

建议实施顺序：**0 → 1 → 2 → 3 → 4 → 5 → 7 → 6 → 8 → 9**

（API 在接线后做，保证端到端可测；循环检测可与 Task 2/3 并行写 helper，在 Task 5 集成时验收）

---

## PR #80 代码复用清单

|PR #80 函数/文件 |复用方式 |改动点 |
|---|---|---|
| `parseUpstreamProxyURL()` |原样复用 |无 |
| `createUpstreamProxy()` |复用，tag 参数化 |tag 从硬编码改入参 |
| `setUpstreamProxyDomainResolver()` |原样复用 |无 |
| `injectUpstreamProxyDetour()` |逻辑参考（parse-time 注入用 map 级别而非 typed options）|实现不同但逻辑等价 |
| `upstreamProxyLocalDNSTransportTag` |参数化 |per-sub 传 `__resin_sub_dns_<subID>` |
| `builder.go UpstreamProxy config` |保留为 env 层（三层优先级最底层） |无 |
| `env.go UpstreamProxy` |保留为 env 层 |无 |
| `main.go wiring` |保留为 env 层 |无 |

---

## 风险与缓解

|风险 |影响 |缓解 |
|---|---|---|
| sbOutbound manager 动态注册/注销线程安全 |节点引用模式 |先写并发压测（Task 4 测试）；sing-box manager 内部有锁，但需验证 |
| `RegisterBuiltOutbound` API 是否存在于 sbOutbound manager |Task 4 实现路径 |若无此 API → 改为通过 `outboundMgr.Create` 重建（从 RawOptions），代价多一次构建但代码更简单 |
| 被引用节点的订阅删除导致 detour 悬空 |节点引用模式 |记录 last_error；后续加级联清理（本期 YAGNI） |
| sbOutbound manager 无 per-outbound Remove API |Task 3 注销路径 |确认接口；若无则只 Close 实例不从 manager 移除，同 tag Register 幂等覆盖 |
| 被引用节点重建后 DetourDialer 缓存旧 outbound 引用 |引用方拨号短暂 stale |属整订阅 churn 的预期瞬态（sing-box `DetourDialer.initOnce`）；节点也重建后自动恢复；非持续故障 |
| 大订阅刷新时 per-sub outbound 注册/注销开销 |所有模式 |单个 outbound 注册开销 < 1ms（内存操作）；实际无感知 |