# Resin 原生「每 IP 账号配额」设计文档

- 日期：2026-08-15
- 状态：待评审
- 仓库：`D:\data\code\Resin`（MIT，自维护 fork）
- 上游需求来源：Grok Build Free 风控机制分析（账号池保智力：每个代理 IP 每 2 小时最多走 3 个账号）

---

## 1. 背景

### 1.1 需求来源

Grok Build Free 的风控机制中，**使用风控（BFS flag 2）**针对「同一 IP 短时间内多个账号请求」：

- 同一 IP 约 10 分钟内 ≥5 个账号请求 → 账号被标记 flag 2，必降智；
- flag 2 目前不可逆；
- 社区通行做法：**每个代理 IP 每 2 小时只走 3 个账号**，额度用完冷却换 IP。

### 1.2 现状能力核查（基于源码）

| 组件 | 能力 | 缺口 |
|---|---|---|
| Resin | 粘性租约（账号→出口 IP）、IPLoadStats、平台正则过滤热加载 | **无「每 IP 账号数上限」原语**；无节点启停 API |
| grok2api | `{account}` 占位符（账号=Resin 账号）、账号隔离、出口节点管理 | 无 per-IP 配额限制 |

结论：两者均无法直接实现「每 IP 2 小时 3 个账号」，需在上层或 Resin 内部补能力。

### 1.3 方案选型

- **方案 A：外部控制器脚本**（轮询 ip-load/leases + 动态改 platform 正则过滤做硬冷却）——可行但属旁路控制，逻辑游离在路由核心之外，自愈性差；
- **方案 B（选定）：魔改 Resin 加原生配额**——在路由热路径直接支持 `max_accounts_per_ip` + 滚动窗口，长期最优、零外部依赖；
- **方案 C：放弃 Resin 回到 Clash/Mihomo 自管路由**——最确定但放弃现有基建。

选定方案 B。Resin 为 MIT 开源，源码本地已有（`D:\data\code\Resin`）。

### 1.4 规模参数

- 账号池：150+ 账号
- 出口 IP：10+ 个
- 配额：每 IP 每 2h ≤ 3 个账号
- **同时在线上限 = IP 数 × 3 ≈ 30 个账号**；150 个账号作为蓄水池轮转（配额耗尽后换号）。

---

## 2. 语义定义

- **规则**：任一 IP，在**滚动 2 小时窗口**内，最多被绑定过 N 个不同账号（N 可配，默认 3）。第 N+1 个账号尝试绑定该 IP → 换其它 IP。
- **不驱逐**：已绑定账号保持原 IP 不动（粘性优先，避免打断已建立会话/CF 状态）。配额仅在**新租约创建**时检查。
- **计数口径**：按「账号首次绑定该 IP 的时间」写入窗口；账号离开后计数保留至窗口滑出（与「3 个额度用完即冷却」语义一致）。
- **同账号续租**（`tryLeaseHit` / `tryLeaseSameIPRotation`）：不增加计数，不检查配额。
- **非粘性请求**（无账号的随机路由）：不计数、不限制。
- **重启行为**：窗口为内存态，重启清零，2 小时内自愈，可接受。

---

## 3. 配置字段（platform 级）

```
max_accounts_per_ip: 3        # 0 = 不限制（默认，向后兼容）
ip_account_window: "2h"       # 滚动窗口，0 = 默认 2h
```

校验：`max_accounts_per_ip > 0` 时 `ip_account_window` 必须 > 0。

---

## 4. 改动点（文件级）

| 层 | 文件 | 改动 |
|---|---|---|
| 领域模型 | `internal/model/models.go` | `Platform` 增加 `MaxAccountsPerIP int`、`IPAccountWindowNs int64` |
| 平台域 | `internal/platform/` | 字段透传 + 校验（max>0 时 window 必须 >0）|
| 持久化 | `internal/state/schema.go` + `migrate.go` | `platforms` 表新增两列，默认 0（无损迁移）|
| 控制面 API | `internal/api/handler_platform.go` + `internal/service` | create/patch/list 支持新字段 + 校验 |
| **路由状态** | `internal/routing/lease.go`（新增 `ip_window.go`）| 新增 `IPAccountWindow`：`map[ip]map[account]firstSeenNs`；`Touch(ip, acc, now)` / `CountRecent(ip, now, win)` / `IsEligible(...)`；带锁（10 IP 规模开销可忽略）|
| **分配热路径** | `internal/routing/random.go` | `randomRoute()` 每次 pick 后检查 `IsEligible`，不满足则重试（≤8 次）；全部不满足 → 走兜底 |
| 租约创建挂钩 | `internal/routing/router.go` `createOrAbortStickyLease` | 创建成功后 `state.IPWindow.Touch(newLease.EgressIP, account, now)` |

---

## 5. 数据流（第 4 个账号到来时）

```
账号 D 请求 → routeSticky → 无租约 → createLease → randomRoute
  → pick 到 IP-1 → IPWindow.CountRecent(IP-1, 2h) == 3 → 不合法，重试
  → pick 到 IP-2（计数 1）→ 合法 → 创建租约 → Touch(IP-2, D)
```

已有账号 A/B/C 的租约不受影响；账号离开后 `DELETE lease` 释放 IP，但窗口内该 IP 对新账号仍显示为满。

---

## 6. 兜底策略（已确认：fail-open）

**全部 IP 均达配额上限时**：

- **fail-open（采用）**：放行负载最低的 IP，同时计数并输出结构化日志（事件 `ip_quota_exceeded_fallback`），并暴露指标 `ip_quota_blocked_total`（可观测到"超额"程度）。
- fail-closed（备选，未采用）：返回 `NO_AVAILABLE_NODES` 503，严格不超配，但 IP 池小时直接打挂。

选择依据：IP 池仅 10+ 个，fail-closed 会造成间歇性全池 503 断流；fail-open 配合监控日志可平滑应对小池子，实际超额幅度可控（第 4 个号的窗口滑动即可自愈）。

---

## 7. 明确不做（YAGNI）

- 窗口不持久化（重启清零，2h 内自愈）
- 不做 WebUI 表单（API 先行，UI 后补）
- 不做「指定账号到指定 IP」——Resin 架构不可行
- 不改动非粘性路由行为

---

## 8. 测试计划

| 测试 | 内容 |
|---|---|
| `ip_window_test.go` | 窗口滑动、剪枝、计数正确性；`Touch` 幂等（同账号重复触碰不重复计数）|
| router 级 | 第 4 账号换 IP；同账号续租/同 IP 轮换不受影响；窗口滑出后配额恢复 |
| 兜底 | 全满时 fail-open 放行并计数；fail-closed 路径返回 503（若启用）|
| API contract | 新字段校验（max>0 必须带 window）、默认 0 兼容、旧 payload 无损 |
| 迁移 | 旧库升级无损，默认配额禁用（行为不变）|

---

## 9. 验收标准

1. 现有行为零变化（新字段默认 0 时，路由、租约、API 全部与升级前一致）。
2. 配置 `max_accounts_per_ip: 3` + `ip_account_window: 2h` 后，任意 IP 在 2h 窗口内绑定账号数不超过 3（fail-open 兜底路径除外，且有日志/指标可查）。
3. 已绑定账号的粘性不被破坏。
