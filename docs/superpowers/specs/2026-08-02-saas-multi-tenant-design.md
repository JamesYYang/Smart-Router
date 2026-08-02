# SmartRouter SaaS 多租户改造设计

- **日期**:2026-08-02
- **状态**:Draft(待用户审阅)
- **方案**:方案 A — 租户实体 + 子域名路由 + API Key 管理员(精简 MVP)
- **改造策略**:一步重构,不向后兼容;现有单租户部署迁移为 `default` 租户

---

## 1. 背景与目标

SmartRouter 当前的所有模块(authkeys、usage、audit、budget、virtual_models、failover、guardrails、workflows、pricing_overrides、tagging)都是单租户全局命名空间:`/admin/*` 无 RBAC、无 Host/子域名感知、所有持久化表无 `tenant_id` 列。本设计将其改造为多租户 SaaS 平台,每个租户拥有独立子域名(如 `xyz.smart-router.com`),平台托管 Provider Key、按租户隔离数据与配额。

### 核心决策(已确认)

| 决策点 | 选择 | 理由 |
|---|---|---|
| Provider Key 模式 | 平台托管 | 凭证不出配置文件,租户不接触上游 Key |
| 数据隔离模型 | 共享 DB + `tenant_id` 列 | 资源利用率高、运维简单,适合租户量大的 SaaS |
| 改造策略 | 一步重构,不向后兼容 | 单一代码路径,无分支维护成本 |

### 不在范围内(Out of Scope)

- 用户登录系统 / 邮箱密码 / SSO(方案 B 的范畴)
- 租户自助注册
- 租户级 rate limit / 配额中间件
- 计费、发票、支付集成
- 跨租户聚合分析仪表盘
- per-tenant 独立 provider 配置(BYOK)

---

## 2. 架构总览

租户身份由子域名决定,贯穿整个请求生命周期。所有持久化数据按 `tenant_id` 隔离。平台管理员与租户管理员共用 `/admin/*` 路径,由 Host 决定路由到哪个 handler。

```
*.smart-router.com  ──►  TenantResolver 中间件
                            │
                            ├─ app. / www. / apex  ──► PlatformAdminHandler  (master key)
                            ├─ xyz. (tenant)       ──► TenantAdminHandler     (tenant admin key)
                            │                        └─ /v1/*  ──► 现有推理链路 (tenant API key)
                            └─ 未知/已禁用          ──► 404 / 403
```

### 请求中间件链(新增项加粗)

在 `internal/server/http.go:220-326` 现有中间件链中插入 `TenantResolver`:

```
RequestLogger → Recover → BodyLimit → RequestID → WriteDeadline
  → 【TenantResolver】 → RequestSnapshotCapture → TaggingCapture
  → PassthroughSemanticEnrichment → AuditLog → ExtraMiddleware
  → Auth → RequestRewrite → WorkflowResolution
```

`TenantResolver` 必须在 `RequestSnapshotCapture` 之前,使后续所有阶段都能从 `context.Context` 读到 `tenantID`。

### 配置类数据的继承模型

`virtual_models`、`failover_rules`、`guardrails`、`workflows`、`pricing_overrides`、`tagging_rules` 采用"平台默认 + 租户覆盖"继承:表中 `tenant_id IS NULL` 表示平台默认,租户行覆盖平台默认。这与 `internal/app/app.go` 现有的"YAML 种子 + DB 覆盖"模式一致,无需新概念。

---

## 3. 租户身份与子域名解析

### 3.1 新增表 `tenants`

```sql
CREATE TABLE tenants (
  id          TEXT PRIMARY KEY,            -- uuid
  subdomain   TEXT UNIQUE NOT NULL,        -- 小写, [a-z0-9-]{3,63}
  name        TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'active',  -- active|disabled
  plan        TEXT,                        -- free|pro|enterprise (MVP 仅信息字段)
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
```

### 3.2 新增中间件 `TenantResolver`(`internal/server/tenant_resolver.go`)

- 读取 `r.Host`,剥去配置的 `server.base_domain`(如 `smart-router.com`),取首段作为 subdomain。
- subdomain 命中 `server.platform_host`(默认 `app`),或为 `www`、apex → 标记 `isPlatformHost=true`,不注入 tenantID。
- subdomain 在 `tenants` 表命中且 `status=active` → 注入 `core.TenantID` 到 context。
- 命中但 `status=disabled` → 返回 403 `{"error":{"type":"tenant_disabled"}}`。
- 未命中 → 返回 404 `{"error":{"type":"unknown_tenant"}}`(或 421 Misdirected Request)。
- 租户查询结果走 `internal/authkeys` 同款缓存模式(内存 + 60s TTL,见 `internal/authkeys/service.go:289`),避免每请求查库。

### 3.3 新增 context key(`internal/core/context.go`)

现有 `authKeyID`、`effectiveUserPath`(`internal/core/context.go:17-21`)旁新增:

- `tenantID` context key + `TenantIDFromContext(ctx)` / `WithTenantID(ctx, id)` helper
- `isPlatformHost` context key + 对应 helper

### 3.4 配置新增(`config/config.example.yaml`)

```yaml
server:
  base_domain: smart-router.com    # 子域名解析基域
  platform_host: app               # 平台后台子域名 (app.smart-router.com)
```

`www` 与 apex 也视为 platform host,兼容直接访问根域的场景。

---

## 4. 数据模型变更

### 4.1 `auth_keys` 表(`internal/authkeys/store_sqlite.go:25`)

加两列:

```sql
ALTER TABLE auth_keys ADD COLUMN tenant_id TEXT REFERENCES tenants(id);
ALTER TABLE auth_keys ADD COLUMN is_tenant_admin INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_auth_keys_tenant ON auth_keys(tenant_id);
```

`AuthKey` 结构体(`internal/authkeys/types.go:12`)对应加 `TenantID string` 与 `IsTenantAdmin bool`。现有 `UserPath` 保留,含义不变(租户内的子分组,如团队/项目)。

### 4.2 其它表统一加 `tenant_id`

| 表 | 隔离模型 | 继承 |
|---|---|---|
| `usage_entries`、`audit_log`、`budgets` | `tenant_id NOT NULL` | 无,纯租户私有 |
| `conversations`、`response_cache` | `tenant_id NOT NULL` | 无 |
| `virtual_models`、`failover_rules`、`guardrails`、`workflows`、`pricing_overrides`、`tagging_rules` | `tenant_id TEXT NULL` | **平台默认 + 租户覆盖**,`NULL`=平台默认 |

`pricing_overrides` 当前按 `selector` 全局生效(`internal/pricingoverrides/types.go:42`),改为 `(tenant_id, selector)` 复合键,租户可签自定义价。

### 4.3 两级 Key 模型

| Key 类型 | 创建者 | 用途 | 绑定字段 |
|---|---|---|---|
| 租户管理员 Key | 平台管理员(创建租户时签发) | 访问 `xyz.smart-router.com/admin/*`,管理本租户 API Key、查看用量、配置 budget | `tenant_id` + `is_tenant_admin=true` |
| 租户 API Key | 租户管理员(在租户后台自行创建/轮换) | 调用 `xyz.smart-router.com/v1/chat/completions` 等推理接口 | `tenant_id` + 可选 `UserPath` |

租户管理员 Key 可随时轮换;丢失由平台管理员重置。

### 4.4 迁移脚本(一步重构,不向后兼容)

```sql
-- 1. 建 tenants 表,插入 default 租户
INSERT INTO tenants(id, subdomain, name, status, created_at, updated_at)
  VALUES ('default', 'default', 'Default Tenant', 'active', ?, ?);

-- 2. 每张业务表 ALTER ADD COLUMN tenant_id TEXT DEFAULT 'default';
-- 3. 回填后改 NOT NULL(usage/audit/budget/conversations/response_cache);
--    config 类表保留 NULL 表示平台默认
-- 4. auth_keys: 现有 key 全部 tenant_id='default', is_tenant_admin=1
```

现有部署视为单租户 `default`,行为等价于当前单租户模式。迁移由 `internal/storage` 在 schema 初始化阶段执行(扩展现有 migration 机制)。

---

## 5. Store 层接口变更

当前 Store 接口签名无 tenant 参数(如 `authkeys.Store.List(ctx)`,见 `internal/authkeys/store.go:38`、`pricingoverrides/store.go:31`)。**改为显式 `tenantID` 参数**,不用 context-scoping 隐式传递。

### 理由

跨租户泄露是安全边界,显式参数能利用编译器强制每个调用点思考租户归属;context-scoping 容易在新增调用点时遗漏注入。

### 示例

```go
// internal/authkeys/store.go
type Store interface {
    List(ctx context.Context, tenantID string) ([]AuthKey, error)
    Get(ctx context.Context, tenantID, keyID string) (*AuthKey, error)
    Create(ctx context.Context, tenantID string, key AuthKey) error
    // ...
}
```

config 类表(virtual_models 等)的 Store 增加继承解析方法:

```go
// 返回 (tenant 专属覆盖 + 平台默认 fallback) 合并后的视图
ListEffective(ctx context.Context, tenantID string) ([]VirtualModel, error)
```

实现层 SQL:`WHERE tenant_id = ? OR tenant_id IS NULL`,应用层按"租户覆盖优先"合并。

---

## 6. HTTP 路由与认证

### 6.1 路由分组(`internal/server/http.go:329-409` 改造)

`/admin/*` 路径按 Host 分流到不同 handler:

```go
// 平台后台:host ∈ {app, www, apex}
platformGroup := e.Group("/admin", hostGuard("platform"), platformAuth(masterKey))
platformGroup.Use(adminHandlers.Platform.Register)

// 租户后台:host = 租户子域名
tenantGroup := e.Group("/admin", hostGuard("tenant"), tenantAdminAuth())
tenantGroup.Use(adminHandlers.Tenant.Register)

// 推理 API:host = 租户子域名
v1 := e.Group("/v1", hostGuard("tenant"), auth)  // 现有 auth,加 tenant 校验
```

`hostGuard(kind)` 是新中间件:校验当前 Host 类型与预期一致,否则 404,避免租户后台路由在平台 host 上暴露。

### 6.2 认证中间件改造(`internal/server/auth.go`)

现有 `AuthMiddlewareWithAuthenticator`(`internal/server/auth.go:32`)逻辑更新:

- **master key 命中** → 平台管理员身份。在 platform host 上以平台管理员身份访问 `/admin/*`;在租户 host 上以超级管理员身份代客排障(审计日志记录"平台管理员访问租户")。
- **managed key 命中** → 校验 `key.tenant_id == ctx.tenantID`(否则 401 `key_tenant_mismatch`);
  - 若 `key.is_tenant_admin=true` → 允许访问 `/admin/*`(仅租户 host;在 platform host 上 401 `key_not_allowed_on_platform_host`)。
  - 否则 → 仅允许 `/v1/*`,访问 `/admin/*` 返回 403 `insufficient_role`。

### 6.3 关键安全约束

- master key 在租户 host 上有效(超级管理员),但审计日志记录。
- 租户 admin key 在平台 host 上 **401 拒绝**。
- 租户 API key 访问 `/admin/*` **403 拒绝**。
- master key 为空时,`/admin/*` 在 platform host 上返回 503(配置错误),而非当前的"全开放"(`internal/server/http.go:207-209` 的 skipPaths 行为废止)。

---

## 7. Admin 拆分:平台后台 vs 租户后台

把现有 `internal/admin/handler.go:32-54` 的单 `Handler` 拆成两个。

### 7.1 `PlatformAdminHandler`(挂 `app.smart-router.com/admin/*`)

| 职责 | 端点示例 |
|---|---|
| 租户 CRUD | `POST/GET/PATCH/DELETE /admin/tenants` |
| 签发/重置租户管理员 key | `POST /admin/tenants/:id/admin-keys` |
| 全局 provider 状态(只读) | `GET /admin/providers` |
| 平台默认配置 | `/admin/virtual-models?scope=platform` 等 |
| 全局定价默认 | `/admin/pricing-overrides?scope=platform` |
| 跨租户聚合指标(可选) | `GET /admin/metrics/tenants` |

### 7.2 `TenantAdminHandler`(挂 `xyz.smart-router.com/admin/*`)

| 职责 | 端点示例 |
|---|---|
| 本租户 auth key 管理 | `POST/GET/DELETE /admin/auth-keys`(自动 scope 当前 tenant) |
| 本租户用量/审计 | `/admin/usage`、`/admin/audit` |
| 本租户预算 | `/admin/budgets` |
| 本租户 virtual_models/failover/guardrails/workflows 覆盖 | `/admin/virtual-models`(scope=tenant) |
| 本租户 pricing 覆盖 | `/admin/pricing-overrides`(scope=tenant) |

两个 handler 共享底层 service,但 service 方法都带 `tenantID` 参数;`TenantAdminHandler` 从 context 取 tenantID,`PlatformAdminHandler` 显式传目标 tenantID 或 `nil`(平台默认)。

### 7.3 Dashboard UI

`internal/admin/dashboard` 现有 SPA 改为 role-aware:

- 平台 host 渲染平台视角(租户列表、全局配置)。
- 租户 host 渲染租户视角(本租户数据)。
- "登录"页 = 粘贴 admin key(MVP),session 存 localStorage;后续可演进为方案 B 的邮箱密码登录。

---

## 8. Provider 与平台托管 Key

- `providers.*` 配置保持全局 YAML/env(`internal/providers/config.go:44`),**不入库**——平台托管 Key 的凭证不出配置文件,安全边界清晰。
- 所有租户共享同一 provider 池,通过 `virtual_models` 决定可用模型。
- 租户不能选自己的 provider,但平台可通过 `virtual_models` 的 tenant 覆盖,限制某租户只暴露部分模型。
- 计费/限流:MVP 不做租户级 rate limit;用量按 `tenant_id` 记录,后续可加配额中间件。

---

## 9. 内存态 store 的 DB 化(前置依赖)

`conversationstore`(`internal/conversationstore/store_memory.go:27`)与 `responsestore` 当前仅内存实现。多租户下有两个问题:

1. **横向扩展**:多实例下内存状态不共享。
2. **隔离**:内存实现没 `tenant_id` 列,无法 SQL 过滤。

### 要求

MVP 必须给这两个 store 补 SQL 实现,复用 `internal/storage` 的 SQLite/Postgres/Mongo 抽象(模式参考 `internal/authkeys/store_sqlite.go`),加 `tenant_id` 列。Redis 后端可选。

如果 MVP 部署确定单实例,可暂留内存实现,但必须把 `tenant_id` 加到 key 上(map 结构改为 `map[tenantID]map[sessionID]...`),否则跨租户会泄露。**推荐直接做 SQL 版**,避免后续返工。

---

## 10. 错误处理

| 场景 | HTTP 状态 | 错误体 |
|---|---|---|
| 未知子域名 | 404 | `{"error":{"type":"unknown_tenant"}}` |
| 租户已禁用 | 403 | `{"error":{"type":"tenant_disabled"}}` |
| auth key 的 tenant_id 与 host 不匹配 | 401 | `{"error":{"type":"key_tenant_mismatch"}}` |
| 租户 admin key 访问平台 host | 401 | `{"error":{"type":"key_not_allowed_on_platform_host"}}` |
| 租户 API key 访问 `/admin/*` | 403 | `{"error":{"type":"insufficient_role"}}` |
| master key 未配置但访问 `/admin/*` | 503 | `{"error":{"type":"master_key_not_configured"}}` |
| 配置类继承解析无任何匹配 | 走现有"未配置"路径 | 不变 |

错误响应复用 `internal/core` 现有错误格式,新增几个 error type 常量。

---

## 11. 测试策略

1. **Store 层隔离测试**:每个 store 的 `_test.go` 加用例——插入 tenant A 与 tenant B 数据,断言 `List(ctx, "A")` 不返回 B 的行。这是跨租户泄露的核心防线。
2. **TenantResolver 单元测试**:Host 解析、平台 host 识别、未知/禁用租户、缓存命中。
3. **Auth 中间件测试**:四种 key × 两种 host 的矩阵(2×2)+ master key 全通。
4. **端到端集成测试**:起两个租户(`xyz`、`abc`),各自签发 API key,发推理请求,断言 usage/audit 只落在自己 tenant_id 下。
5. **现有测试改造**:全部注入 `default` tenant 的 fixture,保证现有用例改动最小。

遵循项目既有约定:每个源文件配同包同基名的 `_test.go`,使用 `testify`(`require`/`assert`)。

---

## 12. 装配层与启动流程

### 12.1 `internal/app/app.go` 装配变更

- `New` 中创建 `tenants.Service`(新),注入 Storage。
- `PlatformAdminHandler` 与 `TenantAdminHandler` 在 `app.go:500-565` 装配,通过 `serverCfg.AdminHandlers` 传入(扩展现有 `serverCfg.AdminHandler` 单字段为 map 或 struct)。
- 各 service(`virtualmodels`、`failover`、`guardrails`、`workflows`、`pricingoverrides`、`budget`、`usage`、`auditlog`、`authkeys`、`tagging`)构造函数加 `tenants` 依赖或仅靠 `tenantID` 参数。
- `New` 失败时的 unwind 逻辑(`app.New` 现有行为)需覆盖新增的 tenants service。

### 12.2 `run/run.go` 启动变更

- 启动时若 `tenants` 表为空且配置了 `server.bootstrap_default_tenant=true`(默认 true),自动插入 `default` 租户,保证空库可启动。
- 现有 `--health` / `--ready` 探针不变。

### 12.3 ext.Registry 兼容性

`ext.Registry`(`ext/registry.go:13-97`)的 `UseMiddleware` 挂载点位于 audit 后、auth 前——即 `TenantResolver` 之后,扩展 middleware 可读到 `tenantID`。扩展无需感知多租户即可继续工作,但文档需说明:扩展若访问 store,必须传 context 中的 `tenantID`。

---

## 13. 关键文件清单(改动锚点)

| 文件 | 改动 |
|---|---|
| `internal/server/http.go:107-429` | 中间件链插入 TenantResolver;路由分组按 Host 拆分 |
| `internal/server/auth.go:32-111` | 认证中间件加 tenant 校验、admin role 校验 |
| `internal/server/tenant_resolver.go`(新) | 子域名解析中间件 |
| `internal/core/context.go:17-24` | 新增 tenantID、isPlatformHost context key |
| `internal/authkeys/types.go:12`、`store.go:38`、`store_sqlite.go:25`、`service.go:289` | AuthKey 加字段、Store 加 tenantID 参数、缓存按 tenant 分桶 |
| `internal/storage/storage.go:63-135` | 新增 tenants 表 migration、各表加 tenant_id 列 |
| `internal/admin/handler.go:32-54`、`routes.go:18-82` | 拆为 PlatformAdminHandler + TenantAdminHandler |
| `internal/admin/dashboard/dashboard.go:24-93` | role-aware UI |
| `internal/app/app.go:195-565` | 装配 tenants.Service、拆分 admin handlers |
| `internal/providers/config.go:44` | 无改动(保持全局) |
| 各业务 store(usage/audit/budget/virtualmodels/failover/guardrails/workflows/pricingoverrides/tagging) | 接口加 tenantID 参数、SQL 加 WHERE tenant_id |
| `internal/conversationstore/store_memory.go:27`、`internal/responsestore`(新 SQL 实现) | 补 SQL/Mongo store + tenant_id |
| `config/config.go:15-37`、`config/config.example.yaml` | 新增 `server.base_domain`、`server.platform_host`、`server.bootstrap_default_tenant` |
| `run/run.go:109-196` | 启动时 bootstrap default 租户 |

---

## 14. 风险与权衡

| 风险 | 影响 | 缓解 |
|---|---|---|
| Store 接口签名全改,改动面大 | 大量文件需同步修改 | 一次性改完,用编译器兜底;先改接口再逐个实现 |
| 跨租户泄露靠应用层 WHERE 过滤,SQL 漏写 = 数据泄露 | 安全事故 | Store 层隔离测试全覆盖;考虑在 SQL 层加 generated column 或 view 强制带 tenant_id |
| `pricing_overrides` 等继承语义复杂 | 解析 bug | 单独的"继承合并"单元测试,覆盖"仅平台默认"、"仅租户覆盖"、"两者并存"三种场景 |
| 内存 store DB 化是前置依赖,不做则多实例无法横向扩展 | 单实例瓶颈 | MVP 先做 SQL 版,避免技术债 |
| master key 在租户 host 上有效 | 误用风险 | 审计日志强制记录;后续可加 `master_key_restricted_hosts` 配置 |
| 现有部署迁移 | 数据丢失 | 一步重构虽不向后兼容,但提供 `default` 租户迁移脚本,行为等价单租户 |

---

## 15. 后续演进路径(非本设计范围)

1. **方案 B 演进**:新增 `users` 表 + JWT/session,租户管理员邮箱密码登录;auth key 仅用于 API 调用。
2. **租户级配额与限流**:在 auth 后加 quota middleware,按 `tenant_id` + `plan` 限速。
3. **计费集成**:usage 表加 `cost_cents` 字段,定期汇总出账单。
4. **跨租户聚合仪表盘**:平台级 analytics,基于 `tenant_id` 维度聚合。
5. **BYOK 支持**:provider 配置入库,按 tenant 隔离,与平台托管 Key 共存(混合模式)。

---

## 附:术语表

- **租户(Tenant)**:SaaS 客户组织,对应一个子域名与一个 `tenant_id`。
- **平台管理员(Platform Admin)**:SaaS 运营方,用 master key 访问 `app.smart-router.com`。
- **租户管理员(Tenant Admin)**:租户内部管理员,用 `is_tenant_admin=true` 的 auth key 访问 `xyz.smart-router.com/admin/*`。
- **租户 API Key(Tenant API Key)**:租户发给开发者的 key,仅用于 `/v1/*` 推理接口。
- **平台默认(Platform Default)**:config 类表中 `tenant_id IS NULL` 的行,所有租户继承的默认值。
- **租户覆盖(Tenant Override)**:config 类表中 `tenant_id = <某租户>` 的行,覆盖平台默认。
