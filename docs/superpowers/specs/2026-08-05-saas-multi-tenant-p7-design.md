# P7 端到端集成设计

- **日期**: 2026-08-05
- **状态**: Draft(待用户审阅)
- **前置**: P1-P6 全部完成(P4 实现完成但 ROADMAP 状态未更新)
- **策略**: 在 master 上直接执行,不 push,subagent-driven-development

---

## 1. 背景与目标

P1-P6 已完成多租户改造的骨架:租户实体与子域名解析(P1)、两级 Key 认证(P2)、Store 隔离(P3)、Admin 拆分与 Host Guard(P4)、/v1 路由与配额中间件(P5)、内存 Store DB 化(P6)。P7 是收尾阶段,目标是把已实现的功能"端到端"串起来并交付可运维的产品形态:

1. **修复唯一可操作的遗留 bug**:`ResolveRequestModelWithAuthorizer` 缺 nil-ctx guard。
2. **Dashboard role-aware UI**:注入角色信息,平台 host 新增租户管理页。
3. **跨租户端到端集成测试**:验证租户隔离与 admin 路由分发的正确性。
4. **部署文档**:多租户模式的配置与启动说明。
5. **ROADMAP 状态修正**:P4 标记完成。

### 已确认范围决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| Dashboard 改造深度 | 最小改动 | 不改页面内容与 JS 逻辑;仅注入 role + sidebar 区分 + 新增 Tenants 页 |
| E2E 测试深度 | 集成测试 | httptest + 真实 SQLite store,复用 `tenant_visibility_integration_test.go` 模式 |
| 部署文档 | 基础文档 | 环境变量、base_domain 配置、架构图、启动检查清单 |
| 迁移脚本 | 不写 | P1-P6 直接在 master 改造,无旧版兼容路径;改为启动检查清单 |
| P6 deferred items | 只修 nil-ctx guard | 其他项(ForTenant 时延、context.Background、timezone 测试、go vet)为已知限制,在文档中记录 |

---

## 2. 架构与改动点

### 2.1 nil-ctx guard(`internal/gateway/request_model_resolution.go`)

`ResolveRequestModelWithAuthorizer`(line 65)接受 `ctx context.Context` 但未检查 nil,若传入 nil 会向下传播到 `ResolveExecutionSelector` 导致 panic。与其他 gateway 文件(如 `inference_prepare.go` 的 `ResolveWorkflowForChat` 等)的既有防御模式保持一致:

```go
func ResolveRequestModelWithAuthorizer(ctx context.Context, ...) (*core.RequestModelResolution, error) {
    if ctx == nil {
        ctx = context.Background()
    }
    // ...
}
```

**测试**: 在 `request_model_resolution_test.go`(若存在)或对应测试文件中加 nil-ctx 用例;若无现成测试文件则新建。

### 2.2 Dashboard role-aware UI

#### 现状

- `internal/admin/dashboard/dashboard.go` 的 `templateData` 只有 `BasePath` 和 `Version`,无角色信息。
- Dashboard 路由(`/admin/dashboard`)挂在全局,无 host guard,两种 host 渲染完全相同的页面。
- 后端已按 host 区分:平台 host 由 `PlatformAdminHandler` 服务(含 `/admin/tenants` CRUD),租户 host 由 `TenantAdminHandler` 服务(全部按当前租户 scope)。

#### 改动

1. **注入角色**:`templateData` 增加 `IsPlatformAdmin bool` 字段。`Index` handler 中通过 `core.GetPlatformHost(c.Request().Context())` 判断平台 host,渲染时写入。同时把 `IsPlatformAdmin` 输出到页面(作为 `data-` 属性或 Alpine 初始状态),供 JS 使用。

2. **新增 Tenants 管理页**(平台 host 专属):
   - 新模板 `templates/page-tenants.html`,渲染租户列表 + 新建/编辑/停用操作。
   - 新 JS 模块 `static/js/modules/tenants.js`,调用 `GET/POST/PATCH/DELETE /admin/tenants`。
   - sidebar 增加 "Tenants" 导航项,用 `x-show="isPlatformAdmin"` 控制仅在平台 host 显示。
   - 复用现有 dashboard 页面模式(Alpine 组件 + 模块工厂挂 `window.dashboardTenantsModule`,配 `.test.cjs` 单测,遵循 CODEBUDDY.md 约定)。

3. **现有导航项不变**:Overview/Models/Audit Logs/Usage/Budgets/API Keys/Workflows/Guardrails/Settings 对两种角色都保留——后端 `TenantAdminHandler` 已将其按当前租户 scope,租户 admin 管理本租户数据是合理能力,不应在 UI 层隐藏。

4. **Dashboard 测试**:`dashboard_test.go` 加用例断言平台/租户 host 渲染时 `IsPlatformAdmin` 正确;新增 tenants 模块 `.test.cjs`。

### 2.3 跨租户端到端集成测试

新建 `internal/server/p7_e2e_integration_test.go`,复用 `tenant_visibility_integration_test.go` 的 full-chain + SQLite 模式。测试矩阵:

| 用例 | 场景 | 断言 |
|---|---|---|
| 租户数据隔离 | tenant-a 与 tenant-b 各建 auth key,互相不可见 | List 返回各自结果 |
| Admin 路由分发 | 平台 host 访问 `/admin/tenants` 200;租户 host 访问 `/admin/tenants` 404(hostGuard) | 状态码 |
| Host Guard | 平台 host 访问 `/v1/*` 404;租户 host 访问成功 | 状态码 |
| 禁用租户 | disabled tenant 访问租户 API → 403 | 状态码 |
| 配额中间件 | 超限租户 /v1 请求 → 402;未超限 → 通过 | 状态码 |
| 两级 key × host 矩阵 | tenant admin key 在平台 host → 401;master key 全通 | 状态码 |

### 2.4 部署文档

新建 `docs/deployment/multi-tenant.md`,覆盖:

- **多租户架构图**:`*.base_domain` → TenantResolver → 平台/租户 handler 分流。
- **配置项**:`server.base_domain`、`server.platform_host`、`server.bootstrap_default_tenant`(默认 true)、两级 key(`auth.master_key` + 租户 admin key 签发)。
- **环境变量**:上表配置对应的 env var 名(对照 `config.example.yaml`)。
- **启动流程**:空库自动 bootstrap `default` 租户 → 平台 host 用 master key 建租户 → 签 tenant admin key → 租户 admin 配 virtual_models 等。
- **启动检查清单**:验证 `/health`、`/ready`、平台 host 登录、租户 host 登录、推理请求。
- **已知限制**:P6 deferred items 汇总(ForTenant 写透传时延、ResolveRequestModel context.Background、RefreshAll 语义等),注明为文档性记录。

### 2.5 ROADMAP 修正

`docs/superpowers/ROADMAP.md`:
- P4 行状态 `⬜ 待开始` → `✅ 完成`,补完成日期与 commit 范围(需从 git log 确认)。
- 新增 P7 完成记录段。

---

## 3. 错误处理与边界

- Dashboard Tenants 页的 API 错误直接复用 admin API 返回的 JSON 错误格式,页面展示错误消息。
- Tenants 页新建/编辑校验沿用 `PlatformAdminHandler` 现有校验(保留子域名、格式等),前端仅做最小提示。
- 未启用多租户(`base_domain` 未配置)时:`IsPlatformAdmin` 恒为 true(兼容现有单租户部署,平台视角完整)。

---

## 4. 测试策略

1. **nil-ctx guard**: `request_model_resolution_test.go`(或新建)加 nil-ctx 用例。
2. **Dashboard role**: `dashboard_test.go` 断言两种 host 下 `IsPlatformAdmin` 渲染;tenants JS 模块 `.test.cjs`。
3. **E2E 集成**: `p7_e2e_integration_test.go` 覆盖 2.3 矩阵。
4. **回归**: `go build ./...`、`go test ./...`、`go vet ./...`、dashboard JS `node --test`。

## 5. 交付物清单

| 文件 | 改动 |
|---|---|
| `internal/gateway/request_model_resolution.go` | nil-ctx guard |
| `internal/gateway/request_model_resolution_test.go`(新建) | nil-ctx 用例 |
| `internal/admin/dashboard/dashboard.go` | templateData 加 IsPlatformAdmin |
| `internal/admin/dashboard/templates/sidebar.html` | Tenants 导航项 + x-show |
| `internal/admin/dashboard/templates/page-tenants.html`(新建) | 租户管理页 |
| `internal/admin/dashboard/static/js/modules/tenants.js`(新建) | 租户管理 JS 模块 |
| `internal/admin/dashboard/static/js/modules/tenants.test.cjs`(新建) | 模块单测 |
| `internal/admin/dashboard/dashboard_test.go` | role 渲染断言 |
| `internal/server/p7_e2e_integration_test.go`(新建) | 跨租户 E2E 集成测试 |
| `docs/deployment/multi-tenant.md`(新建) | 部署文档 |
| `docs/superpowers/ROADMAP.md` | P4 状态、P7 完成记录 |
