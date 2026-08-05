# SmartRouter 多租户部署指南

- **适用范围**:SmartRouter 以多租户 SaaS 模式对外提供服务(平台托管 Provider Key、按租户子域名隔离)。
- **关联文档**:设计文档 [`docs/superpowers/specs/2026-08-02-saas-multi-tenant-design.md`](../superpowers/specs/2026-08-02-saas-multi-tenant-design.md)、P7 端到端设计 [`docs/superpowers/specs/2026-08-05-saas-multi-tenant-p7-design.md`](../superpowers/specs/2026-08-05-saas-multi-tenant-p7-design.md)。
- **重要说明**:P7 阶段**不提供**单租户 → 多租户的迁移脚本,也不提供旧版兼容路径(详见[第 7 节](#7-迁移说明))。本文档面向**全新部署**。

---

## 1. 多租户架构

租户身份由请求的 `Host` 子域名决定,贯穿整个请求生命周期;所有持久化数据按 `tenant_id` 隔离。平台管理员与租户管理员共用 `/admin/*` 路径,由 Host 类型(平台 host / 租户 host)决定路由到哪个 handler。

```
*.base_domain  ──►  TenantResolver 中间件(解析 Host → 租户身份)
                        │
                        ├─ app.<base_domain> / www.<base_domain> / apex(根域)
                        │      ──► 平台 host:PlatformAdminHandler(master key)
                        │             ├─ /admin/*  租户 CRUD、签发租户管理员 key、平台默认配置
                        │             └─ /admin/dashboard  平台视角 Dashboard
                        │
                        ├─ <subdomain>.<base_domain>(租户子域名)
                        │      ──► 租户 host:TenantAdminHandler(租户管理员 key)
                        │             ├─ /admin/*  本租户 API Key/用量/预算/virtual_models 等
                        │             ├─ /admin/dashboard  租户视角 Dashboard
                        │             └─ /v1/*  推理接口(hostGuard("tenant"),租户 API key)
                        │
                        └─ 未知子域名 / 已禁用租户 ──► 404 unknown_tenant / 403 tenant_disabled
```

关键实现:

- **TenantResolver**(`internal/server/tenant_resolver.go`):读取 `r.Host`,剥去配置的 `server.base_domain`,取首段作为子域名。`app`(即 `server.platform_host`)、`www`、apex 均标记为平台 host(`isPlatformHost=true`,不注入租户 ID);租户子域名命中 `tenants` 表且 `status=active` 时注入 `tenantID` 到 context;命中但已禁用返回 403 `tenant_disabled`;未命中返回 404 `unknown_tenant`。解析结果走内存 + 60s TTL 缓存。
- **hostGuard**(`internal/server/host_guard.go`):路由组级守卫。`/v1/*` 挂在 `hostGuard("tenant")` 下,`/admin/*` 按 host 类型分发——平台 host 上的 `/admin` 只暴露平台路由,租户 host 上的 `/admin` 只暴露租户路由,host 类型不匹配一律 404(避免某类路由在错误的 host 上"存在")。
- **/admin 挂载**(`internal/server/http.go`):`ADMIN_ENDPOINTS_ENABLED` 开启且配置了 `base_domain` + tenants service 时,`mountAdminRoutesByHost` 按 host 类型挂载平台/租户路由(路径冲突时按 host 分发的单注册)。**未配置 `base_domain`(开发/单机模式)时回退到旧版单租户表面**(完整平台路由、无 hostGuard)。
- **/v1 推理**(`internal/server/http.go:366-408`):`/v1/*` 只挂在租户 host 下,平台 host 访问 `/v1/*` 返回 404。开发模式(未配置 `base_domain`)下 `isPlatformHost` 恒为 false,hostGuard 放行,路由保持可用。
- **租户配额中间件**(`internal/server/tenant_quota.go`):仅在 `/v1` 推理组上运行;租户级预算(user_path 为 `*`/`/*` 的预算)超限返回 402 `tenant_budget_exceeded`,同时不影响 `/admin/*` 可达(租户管理员仍能登录后台调整预算)。

错误响应(`internal/server/tenant_resolver.go`):

| 场景 | HTTP 状态 | error type |
|---|---|---|
| 未知子域名 | 404 | `unknown_tenant` |
| 租户已禁用 | 403 | `tenant_disabled` |
| 解析内部错误 | 500 | `tenant_resolution_failed` |

---

## 2. 前置条件

1. **共享存储(`storage.type`)**:必须配置 `sqlite`、`postgresql` 或 `mongodb` 之一。多租户隔离、租户解析缓存、用量/预算记录都依赖持久化存储;`tenants` 表在此初始化。多实例横向扩展时务必使用 **PostgreSQL 或 MongoDB**(SQLite 为单文件,适合单实例部署)。
2. **`server.base_domain` 必须设置**:这是开启租户子域名解析的开关。为空时 TenantResolver 为 no-op(开发/单机模式),`/admin` 回退到旧版单租户表面。
3. **Provider 配置**:`providers.*` 保持全局 YAML/env 配置,凭证**不入库、不暴露给租户**(平台托管 Key)。所有租户共享同一 provider 池,通过 `virtual_models` 决定各租户可用的模型。
4. **`server.master_key` 建议配置**:多租户模式下平台管理员依赖 master key 访问平台 host 的 `/admin/*`;master key 为空时平台 host 上的 `/admin/*` 返回 503(`master_key_not_configured`)。

---

## 3. 配置项

### 3.1 多租户核心配置

| YAML 配置项 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `server.base_domain` | `SERVER_BASE_DOMAIN` | `""`(空 = 关闭租户解析) | 子域名解析基域,如 `smart-router.com`。设置后 `<subdomain>.smart-router.com` 才按租户解析。 |
| `server.platform_host` | `SERVER_PLATFORM_HOST` | `"app"` | 平台后台保留子域名(`app.smart-router.com`)。`www` 与 apex(根域)恒映射到平台 host;该值同时被 `tenants.Service` 在创建租户时拒绝占用。 |
| `server.bootstrap_default_tenant` | `BOOTSTRAP_DEFAULT_TENANT` | `true` | 启动时若 `tenants` 表为空,自动创建 `default` 租户,保证空库可启动。 |
| `server.master_key` | `GOMODEL_MASTER_KEY` | 空 | 平台管理员认证 key。在平台 host 上访问 `/admin/*`;在租户 host 上作为超级管理员代客访问(审计日志记录)。 |

> `default` 与 `www` 为固定保留子域名(见 `internal/tenants/types.go`),平台 admin API 创建的租户不能占用;`default` 租户由启动 bootstrap 独占创建。

### 3.2 Admin 相关配置

| YAML 配置项 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `admin.endpoints_enabled` | `ADMIN_ENDPOINTS_ENABLED` | `true` | 是否启用 Admin REST API(`/admin/*`)。 |
| `admin.ui_enabled` | `ADMIN_UI_ENABLED` | `true` | 是否启用 Admin Dashboard UI(`/admin/dashboard`)。依赖 `endpoints_enabled`;若端点关闭而 UI 开启,会告警并强制关闭 UI。 |
| `admin.live_logs_enabled` | `DASHBOARD_LIVE_LOGS_ENABLED` | `true` | Dashboard 实时日志流(可选)。 |

### 3.3 配置示例

```yaml
server:
  base_domain: "smart-router.com"   # 或环境变量 SERVER_BASE_DOMAIN
  platform_host: "app"              # 或环境变量 SERVER_PLATFORM_HOST
  bootstrap_default_tenant: true    # 或环境变量 BOOTSTRAP_DEFAULT_TENANT
  master_key: "your-secret-key"     # 或环境变量 GOMODEL_MASTER_KEY

admin:
  endpoints_enabled: true           # 或环境变量 ADMIN_ENDPOINTS_ENABLED
  ui_enabled: true                  # 或环境变量 ADMIN_UI_ENABLED

storage:
  type: "postgresql"                # sqlite | postgresql | mongodb
  postgresql:
    url: "postgres://user:pass@localhost/gomodel"
```

> 环境变量始终覆盖 `config/config.yaml`(参见 `config.example.yaml` 注释)。

---

## 4. 启动流程

```
空库启动 ──► bootstrap_default_tenant 自动创建 "default" 租户
              └─► 平台管理员用 master key 登录 app.<base_domain>/admin/dashboard
                    └─► 创建租户(子域名 xyz,如 POST /admin/tenants)
                          └─► 为租户签发租户管理员 key(POST /admin/tenants/:id/admin-keys)
                                └─► 租户管理员用该 key 登录 xyz.<base_domain>/admin/dashboard
                                      └─► 配置 virtual_models、预算、failover、guardrails 等
                                            └─► 租户管理员创建租户 API Key
                                                  └─► 开发者用租户 API Key 调用 xyz.<base_domain>/v1/*
```

1. **空库自动 bootstrap `default` 租户**:启动时若 `tenants` 表为空且 `server.bootstrap_default_tenant=true`(默认 true),`internal/app/tenants_bootstrap.go` 通过 `Service.CreateBootstrapTenant` 创建 `default` 租户(ID/子域名均为 `default`,name 为 `Default Tenant`)。`CreateBootstrapTenant` 绕过保留子域名守卫,是 `default` 唯一合法的创建途径;已存在(含已禁用)时 no-op。
2. **平台管理员建租户**:在 `app.<base_domain>` 上用 master key 访问 `/admin/tenants`,创建租户(指定 `subdomain` + `name`,可选 `plan`)。子域名不能是保留值(`default`/`www`/配置的 `platform_host`),不能与其他租户重复。
3. **签发租户管理员 key**:平台管理员调用 `POST /admin/tenants/:id/admin-keys` 为该租户签发 `is_tenant_admin=true` 的 auth key——这是租户管理员凭证唯一合法的签发途径。key 可随时轮换;丢失由平台管理员重置。
4. **租户管理员配置租户**:租户管理员用该 key 登录 `xyz.<base_domain>/admin/dashboard`,管理本租户的 API Key、用量/审计、预算(budgets),以及 `virtual_models`/`failover`/`guardrails`/`workflows`/`pricing_overrides`/`tagging` 的**租户覆盖**(config 类表采用"平台默认 + 租户覆盖"继承:`tenant_id IS NULL` 为平台默认,租户行覆盖默认)。
5. **租户 API Key 调用推理**:租户管理员在租户后台创建租户 API Key(可选绑定 `user_path` 子分组),开发者用其调用 `xyz.<base_domain>/v1/chat/completions` 等推理接口。

两级 Key 模型:

| Key 类型 | 创建者 | 用途 | 绑定字段 |
|---|---|---|---|
| 租户管理员 Key | 平台管理员(创建租户时签发) | 访问 `xyz.<base_domain>/admin/*`,管理本租户 | `tenant_id` + `is_tenant_admin=true` |
| 租户 API Key | 租户管理员 | 调用 `xyz.<base_domain>/v1/*` 推理接口 | `tenant_id` + 可选 `user_path` |

安全约束:租户 admin key 在平台 host 上 401;租户 API key 访问 `/admin/*` 403;auth key 的 `tenant_id` 与 host 不匹配 401(`key_tenant_mismatch`);master key 在租户 host 上有效但审计日志记录。

---

## 5. 启动检查清单

部署完成后按序验证:

| 步骤 | 验证内容 | 命令 / 操作 | 预期 |
|---|---|---|---|
| 1 | 存活探针 | `curl http://localhost:8080/health`(或 `smartrouter --health`) | `200 {"status":"ok"}` |
| 2 | 就绪探针 | `curl http://localhost:8080/health/ready`(或 `smartrouter --ready`) | `200 {"status":"ready","components":{"storage":"ok"}}`;存储不可达时 `503 not_ready`,Redis 缓存不可达时 `200 degraded` |
| 3 | 平台 host Dashboard | 浏览器打开 `https://app.smart-router.com/admin/dashboard`,粘贴 **master key** 登录 | 显示平台视角(租户列表、Tenants 页) |
| 4 | 租户 host Dashboard | 浏览器打开 `https://xyz.smart-router.com/admin/dashboard`,粘贴该租户的**租户管理员 key** 登录 | 显示租户视角(本租户数据,无 Tenants 页) |
| 5 | 平台/租户 host 路由隔离 | 平台 host 访问 `https://app.smart-router.com/v1/chat/completions`;租户 host 访问 `/admin/tenants` | 均返回 `404`(hostGuard 生效) |
| 6 | /v1 推理请求 | `curl https://xyz.smart-router.com/v1/chat/completions -H "Authorization: Bearer <租户 API Key>" -d '{"model":"<虚拟模型>","messages":[{"role":"user","content":"hi"}]}'` | 正常返回推理结果;usage/audit 落在该租户 `tenant_id` 下 |
| 7 | 未知/禁用租户 | 访问未创建的子域名;禁用某租户后访问其 host | 未知子域名 `404 unknown_tenant`;禁用租户 `403 tenant_disabled` |
| 8 | 跨租户隔离(抽查) | 租户 A 登录后台,确认看不到租户 B 的 key/用量/预算 | 各自只见本租户数据 |

> 注意:必须为 `*.smart-router.com` 配置 DNS 泛解析(含 apex/`www`/`app`),并让网关拿到正确的 `Host` 头(反向代理需透传 `Host`,或关闭 `Host` 头重写)。

---

## 6. 已知限制(P6 deferred items,文档性记录,不做修复)

以下为 P5/P4 遗留的已知限制,在 P6 完成时已记录并推迟,当前**仅作文档记录,不修复**:

1. **ForTenant 写透传刷新时延**:六个配置类 Service(`virtualmodels`/`failover`/`guardrails`/`workflows`/`pricingoverrides`/`tagging`)的管理面方法(`XxxForTenant`)直接写 Store,但仅当 `tenantID == "default"` 时才即时刷新共享缓存;其他租户的变更要等下一次周期刷新 tick 才进入推理热路径缓存,**时延最高可达约 1 小时**(取决于配置服务的刷新间隔)。租户管理员修改 virtual_models/预算等配置后,对非 `default` 租户可能不会立即生效。
2. **`ResolveRequestModel` 硬编码 `context.Background()`**:`internal/gateway/request_model_resolution.go` 的 `ResolveRequestModel` 以 `context.Background()` 调用 `ResolveRequestModelWithAuthorizer`,仅影响测试场景的调用方;生产热路径走 `ResolveRequestModelWithAuthorizer`(P7 已为其加 nil-ctx guard),不受影响。
3. **`RefreshAll` 整表换快照语义**:`RefreshAll(ctx, tenantIDs)` 在遍历租户列表时若出现瞬时错误,可能跳过部分租户快照更新,表现为"整表换快照"的部分失败语义;错误仅告警不中断。
4. **Dashboard JS 时区环境相关测试失败**:`node --test internal/admin/dashboard/static/js/modules/*.test.cjs` 存在时区环境相关的用例失败(3 个,在 P5 基线即存在,当时 399/402 通过)。失败数量与运行环境时区相关,不涉及产品代码正确性。
5. **`go vet` 预存警告**:`go vet ./...` 在 `internal/core` 有 3 个预存警告(`responses.go:148`、`types.go:79`、`types.go:124` 的 struct field `ContentSchema` 重复 json tag `"content"`),为 P6 之前就存在的旧代码,非 P6/P7 引入。

---

## 7. 迁移说明

**P7 不提供单租户 → 多租户的迁移脚本,也没有旧版兼容路径。** P1-P6 直接在 master 上完成了一步式重构(租户实体 + 子域名路由 + 两级 Key + Store 隔离),不存在从旧版本平滑升级到多租户的转换工具。

因此:

- **新部署**直接按本文档配置 `server.base_domain` 等项启用多租户,无需任何迁移。
- **已在运行的旧版单租户实例**若需切换为多租户模式,需自行评估数据迁移方案(`tenants` 表初始化、存量业务数据补 `tenant_id` 等),不在本次交付范围内。
- 未配置 `base_domain` 时,系统仍以旧版单租户表面运行(全平台路由、无 hostGuard),可作为开发/本地模式。
