# SmartRouter SaaS 多租户改造 — P4: Admin 拆分 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把单一的 `internal/admin.Handler` 拆成 `PlatformAdminHandler`(挂 `app.smart-router.com/admin/*`,平台运营视角)与 `TenantAdminHandler`(挂 `xyz.smart-router.com/admin/*`,租户自服务视角),路由按 Host 分流;补全 `tenants` 的 CRUD(Update/软删除);让六个"配置类"Service(virtualmodels/failover/guardrails/workflows/pricingoverrides/tagging)与 `authkeys.Service` 的**管理面**方法接受显式 `tenantID`,使 `TenantAdminHandler` 能正确管理某个真实租户自己的覆盖配置而不泄露给其它租户。

**Architecture:** 六个配置类 Service 目前各自只有一份"进程级单值缓存"(`atomic.Value` 或 mutex 保护的 snapshot),被推理热路径复用,且刷新时硬编码只读 `tenant_id='default'`。**P4 不改这份缓存和推理热路径**——那是 P5("provider 按租户可见性")的范围,涉及把缓存改成 `map[tenantID]snapshot` 并给 4 个目前无 `ctx` 参数的接口(`FailoverResolver.ResolveFailovers`、`WorkflowPolicyResolver.Match`、`usage.PricingResolver.ResolvePricing`、tagging 的 `ExtractLabels`/`StripHeaders`)加参数,影响面远超 admin 拆分。P4 只给每个 Service 新增一组 `XxxForTenant` 管理面方法,直接打 Store(带显式 `tenantID`),完全绕开共享缓存;当 `tenantID` 恰好等于该 Service 当前硬编码的默认租户(`"default"`)时才顺带刷新共享缓存(保持现有平台默认行为不变),其余情况不碰缓存——零风险、纯新增。`hostGuard` 中间件(设计 §6.1)按 `core.GetPlatformHost(ctx)` 校验路由分组与 Host 类型一致,404 掉跑错分组的请求,与 `TenantResolver`/认证中间件配合完成"路由级"隔离,`enforceTenantAndRole`(P2)继续做"认证级"隔离。

**Tech Stack:** Go 1.x, Echo v5, `database/sql`(SQLite/`modernc.org/sqlite`)、`pgxpool`(Postgres)、Mongo driver v2、`testify`(`require`/`assert`)。

## Global Constraints

- **直接在 `master` 上执行**(P1-P7 既定约定:不开 feature 分支/worktree,本地提交,不主动 push)。
- **Dashboard 角色感知 UI 不在 P4 范围内**,推迟到 P7。(`ROADMAP.md` 原文把它写进 P4,但 P1 计划的 Self-Review 明确排除到 P7;2026-08-03 已与用户确认以 P7 为准。)本计划完成后需要更新 `ROADMAP.md` 该行描述以消除歧义(见 Task 13)。
- **推理热路径的租户可见性不在 P4 范围内**,推迟到 P5。P4 新增的 `XxxForTenant` 方法只服务 admin API,不改任何 `atomic.Value`/mutex 缓存的结构,不改 `gateway.FailoverResolver`/`gateway.WorkflowPolicyResolver`/`usage.PricingResolver`/tagging 中间件的接口签名。2026-08-03 已与用户确认此边界。
- **租户删除仅软删除**:`DELETE /admin/tenants/:id` 复用既有 `UpdateStatus(ctx, id, StatusDisabled, now)`,不级联删除 usage/audit/budget/会话/配置数据。2026-08-03 已与用户确认。
- **保留子域名**:`default`、`www`、当前配置的 `platform_host`(默认 `app`)。`tenants.Create`(或其上层)必须拒绝这些值,防止真实租户抢占平台/默认子域名。
- **`XxxForTenant` 命名约定**:六个配置类 Service 新增的管理面方法一律以 `ForTenant` 结尾,签名形如 `func (s *Service) XxxForTenant(ctx context.Context, tenantID string, ...) (...)`,直接调用 Store,不经过共享缓存;仅当 `tenantID == s.tenantID`(该 Service 当前硬编码的默认值,始终是 `"default"`)时才在写方法末尾调用 `s.Refresh(ctx)`,其余情况完全不碰缓存字段。
- **`authkeys.Store` 的 `tenantID == ""` 语义已在 P3 确定为"跨租户/不限定"**(平台管理员读取所有租户的 key)。P4 的 `authkeys.Service` 方法新增显式 `tenantID string` 参数时沿用这个约定:`""` = 不限定,具体值 = 限定到该租户。
- **认证枚举复用 P2 的 `enforceTenantAndRole`**,P4 不改它的规则,只修它的已知缺陷(`isAdminPath` 不匹配裸 `/admin`——P2 完成记录里明确记为 P4 待办)。
- **JSON 字段修复**:`authkeys.AuthKey.IsTenantAdmin` 的 `json:"is_tenant_admin,omitempty"` 去掉 `omitempty`(P2 完成记录里记为 P4 待办:admin UI 需要显式看到 `false`)。
- 遵循项目约定:每个源文件配同包同基名 `_test.go`,使用 `testify`(`require`/`assert`);模型/路径命名为 `smartrouter`(非 `gomodel`)。
- PostgreSQL 占位符是位置参数 `$1..$N`——本计划涉及的 PG 改动如新增列/索引不改变现有查询的参数顺序,无需重新编号,但改动前仍需 grep 确认。
- 迁移遵循 per-store 模式:SQLite `ALTER TABLE ... ADD COLUMN`/`CREATE INDEX IF NOT EXISTS`,PG `ADD COLUMN IF NOT EXISTS`/`CREATE INDEX IF NOT EXISTS`,复用各 store 已有的重复列容错 helper。

---

## Scope / File Structure

| Task | 包/文件 | 内容 |
|---|---|---|
| 1 | `internal/tenants` | CRUD 补全:`Update`(name/plan)、保留子域名校验、PG `idx_tenants_status`、PG/Mongo 重复 subdomain 错误转译 |
| 2 | `internal/server/host_guard.go`(新) | `hostGuard(kind)` 中间件 |
| 3 | `internal/virtualmodels` | `ForTenant` 管理面方法 |
| 4 | `internal/failover` | `ForTenant` 管理面方法 |
| 5 | `internal/guardrails` | `ForTenant` 管理面方法 |
| 6 | `internal/workflows` | `ForTenant` 管理面方法 |
| 7 | `internal/pricingoverrides` | `ForTenant` 管理面方法 |
| 8 | `internal/tagging` | `ForTenant` 管理面方法 |
| 9 | `internal/authkeys` | `Service.List`/`UpdateLabels`/`Deactivate`/`ListViews` 加显式 `tenantID` 参数 |
| 10 | `internal/admin` | 拆分 `PlatformAdminHandler` + `TenantAdminHandler`(含各自 routes) |
| 11 | `internal/server/http.go`、`internal/app/app.go` | 路由按 Host 分流挂载 + 装配 + P2 遗留清理(`isAdminPath`、死代码、JSON tag) |
| 12 | `internal/server` | 端到端集成测试(两租户 + 平台管理员) |
| 13 | (验证) | 全量构建/测试 + Completion Notes + 更新 `ROADMAP.md` |

---

### Task 1: `internal/tenants` CRUD 补全

**Files:**
- Modify: `internal/tenants/store.go`(接口加 `Update`)、`store_sqlite.go`、`store_postgresql.go`、`store_mongodb.go`、`service.go`
- Test: `internal/tenants/store_sqlite_test.go`、`service_test.go`

**Interfaces:**
- Produces: `tenants.Store.Update(ctx, id, name, plan string, updatedAt time.Time) error`、`tenants.Service.Update(ctx, id, name, plan string) error`、`tenants.ErrReservedSubdomain`、`tenants.IsReservedSubdomain(subdomain string) bool`、`tenants.ErrSubdomainTaken`、`tenants.IsSubdomainTaken(err error) bool`。

- [ ] **Step 1: 写失败测试**

在 `internal/tenants/store_sqlite_test.go` 追加:

```go
func TestSQLiteStore_Update(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-upd", Subdomain: "upd", Name: "Old", Status: StatusActive, Plan: "free", CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, store.Update(ctx, "t-upd", "New Name", "pro", now.Add(time.Second)))
	got, err := store.GetByID(ctx, "t-upd")
	require.NoError(t, err)
	require.Equal(t, "New Name", got.Name)
	require.Equal(t, "pro", got.Plan)
}

func TestSQLiteStore_Update_NotFound(t *testing.T) {
	store := newTestSQLiteStore(t)
	err := store.Update(context.Background(), "missing", "n", "p", time.Now().UTC())
	require.True(t, IsNotFound(err))
}
```

在 `internal/tenants/service_test.go` 追加:

```go
func TestService_Create_RejectsReservedSubdomain(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, time.Minute)
	for _, sub := range []string{"default", "www", "app"} {
		err := svc.Create(context.Background(), Tenant{ID: "x", Subdomain: sub, Name: "x", Status: StatusActive})
		require.Error(t, err)
		require.True(t, IsReservedSubdomain(err), sub)
	}
}

func TestService_Update_InvalidatesCache(t *testing.T) {
	store := &fakeStore{tenant: Tenant{ID: "t-1", Subdomain: "xyz", Status: StatusActive}}
	svc := NewService(store, time.Minute)
	_, _ = svc.ResolveBySubdomain(context.Background(), "xyz") // 预热缓存
	require.NoError(t, svc.Update(context.Background(), "t-1", "New", "pro"))
	// 更新后下一次解析应命中新数据(缓存已失效)
	store.tenant = Tenant{ID: "t-1", Subdomain: "xyz", Name: "New", Plan: "pro", Status: StatusActive}
	got, err := svc.ResolveBySubdomain(context.Background(), "xyz")
	require.NoError(t, err)
	require.Equal(t, "New", got.Name)
}
```

`fakeStore` 需要新增 `Update` 方法(在 `internal/tenants/service_test.go` 已有的 `fakeStore` 定义旁加):

```go
func (f *fakeStore) Update(_ context.Context, _ string, name, plan string, _ time.Time) error {
	f.tenant.Name = name
	f.tenant.Plan = plan
	return nil
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/tenants/... -run "TestSQLiteStore_Update|TestService_Create_RejectsReservedSubdomain|TestService_Update_InvalidatesCache" -v`
Expected: 编译失败(`Update`、`IsReservedSubdomain` 未定义)。

- [ ] **Step 3: 加 `Update` 到 `Store` 接口与三个后端**

`internal/tenants/store.go` 的 `Store` 接口(`UpdateStatus` 之后)加:

```go
	Update(ctx context.Context, id, name, plan string, updatedAt time.Time) error
```

`store_sqlite.go`(参考已有 `UpdateStatus` 的写法):

```go
func (s *SQLiteStore) Update(ctx context.Context, id, name, plan string, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE tenants SET name = ?, plan = ?, updated_at = ? WHERE id = ?
	`, name, plan, updatedAt.Unix(), id)
	if err != nil {
		return fmt.Errorf("update tenant: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read update rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
```

`store_postgresql.go`(执行前 `grep -n "func.*UpdateStatus" internal/tenants/store_postgresql.go` 确认现有写法与占位符编号,照抄结构):

```go
func (s *PostgreSQLStore) Update(ctx context.Context, id, name, plan string, updatedAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenants SET name = $1, plan = $2, updated_at = $3 WHERE id = $4
	`, name, plan, updatedAt.Unix(), id)
	if err != nil {
		return fmt.Errorf("update tenant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
```

`store_mongodb.go`(执行前 `grep -n "func.*UpdateStatus" internal/tenants/store_mongodb.go` 确认写法):

```go
func (s *MongoDBStore) Update(ctx context.Context, id, name, plan string, updatedAt time.Time) error {
	res, err := s.coll.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"name": name, "plan": plan, "updated_at": updatedAt.Unix()}})
	if err != nil {
		return fmt.Errorf("update tenant: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: 加保留子域名校验 + 重复 subdomain 错误类型到 `types.go`**

在 `internal/tenants/types.go` 末尾加:

```go
// reservedSubdomains lists subdomains no real tenant may claim. platformHost
// is checked dynamically (see IsReservedSubdomainFor) because it is
// configurable; this set covers the fixed reservations.
var reservedSubdomains = map[string]struct{}{
	"default": {},
	"www":     {},
}

// ErrReservedSubdomain is returned when a Create call targets a reserved
// subdomain (default, www, or the configured platform host).
var ErrReservedSubdomain = errors.New("subdomain is reserved")

// IsReservedSubdomain reports whether err wraps ErrReservedSubdomain.
func IsReservedSubdomain(err error) bool { return errors.Is(err, ErrReservedSubdomain) }

// IsReservedSubdomainName reports whether subdomain is unconditionally
// reserved (independent of the configured platform host).
func IsReservedSubdomainName(subdomain string) bool {
	_, ok := reservedSubdomains[strings.ToLower(strings.TrimSpace(subdomain))]
	return ok
}

// ErrSubdomainTaken is returned when Create targets a subdomain that
// already belongs to another tenant (translated from the backend's unique
// constraint violation).
var ErrSubdomainTaken = errors.New("subdomain already in use")

// IsSubdomainTaken reports whether err wraps ErrSubdomainTaken.
func IsSubdomainTaken(err error) bool { return errors.Is(err, ErrSubdomainTaken) }
```

加 `"strings"` import。

- [ ] **Step 5: `Service.Create` 校验保留字,`Service.Update` 新增,失效对应缓存**

在 `internal/tenants/service.go` 的 `Create` 方法开头加:

```go
func (s *Service) Create(ctx context.Context, t Tenant) error {
	if IsReservedSubdomainName(t.Subdomain) || strings.EqualFold(t.Subdomain, s.platformHost) {
		return fmt.Errorf("create tenant %q: %w", t.Subdomain, ErrReservedSubdomain)
	}
	if err := s.store.Create(ctx, t); err != nil {
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("create tenant %q: %w", t.Subdomain, ErrSubdomainTaken)
		}
		return err
	}
	return nil
}
```

`Service` 结构体需要一个 `platformHost string` 字段来判断保留字(默认调用方传空串时跳过该项校验)。在 `NewService` 签名加一个可选参数,执行前 `grep -rn "tenants.NewService(" internal/app/app.go internal/tenants/*_test.go` 确认所有调用点,改成:

```go
func NewService(store Store, ttl time.Duration, platformHost string) *Service {
	...
	return &Service{store: store, ttl: ttl, platformHost: strings.ToLower(strings.TrimSpace(platformHost)), entries: make(map[string]cacheEntry)}
}
```

更新 `internal/app/app.go` 里 `tenants.NewService(tenantStore, time.Minute)` 调用为 `tenants.NewService(tenantStore, time.Minute, appCfg.Server.PlatformHost)`。更新所有现存测试里 `NewService(store, ttl)` 的调用,补第三个参数(测试里传 `""` 即可,跳过 platform-host 校验)。

`isUniqueConstraintErr` 复用 `store_sqlite.go` 已有的 `isSQLiteUniqueConstraintError`(SQLite 路径),PG/Mongo 路径需要在各自 Store 的 `Create` 里直接返回 `ErrSubdomainTaken`(而不是留给 Service 判断字符串)——执行前 `grep -n "func.*Create" internal/tenants/store_postgresql.go internal/tenants/store_mongodb.go`,在两处 `Create` 的错误分支加唯一约束判断,PG 用 `pgconn.PgError.Code == "23505"`,Mongo 用 `mongo.IsDuplicateKeyError(err)`,命中时返回 `ErrSubdomainTaken` 包装错误,未命中原样透传。Service 层的 `isUniqueConstraintErr` 相应地只需处理 SQLite 分支(PG/Mongo 已经在 Store 层转译好,Service 直接 `errors.Is(err, ErrSubdomainTaken)` 透传即可,不需要重复判断)。

在 `Service` 加:

```go
func (s *Service) Update(ctx context.Context, id, name, plan string) error {
	if err := s.store.Update(ctx, id, name, plan, time.Now().UTC()); err != nil {
		return err
	}
	s.invalidate(id)
	return nil
}
```

(`invalidate` 已在 P1 的 `UpdateStatus` 里使用,直接复用。)

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/tenants/... -v`
Expected: 全部 PASS。

- [ ] **Step 7: 加 PG `idx_tenants_status` 索引(P1 遗留项)**

在 `internal/tenants/store_postgresql.go` 的 `NewPostgreSQLStore` 建表逻辑之后加:

```go
	if _, err := pool.Exec(context.Background(), `CREATE INDEX IF NOT EXISTS idx_tenants_status ON tenants(status)`); err != nil {
		return nil, fmt.Errorf("create tenants status index: %w", err)
	}
```

- [ ] **Step 8: 全量构建 + 测试 + 提交**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS(PG/Mongo 测试默认 skip)。

```bash
git add internal/tenants/ internal/app/app.go
git commit -m "feat(tenants): add Update, reserved-subdomain guard, and duplicate-subdomain translation"
```

---

### Task 2: `hostGuard` 中间件

**Files:**
- Create: `internal/server/host_guard.go`
- Create: `internal/server/host_guard_test.go`

**Interfaces:**
- Consumes: `core.GetPlatformHost(ctx)`(P1)。
- Produces: `server.hostGuard(kind string) echo.MiddlewareFunc`,`kind` 取值 `"platform"` 或 `"tenant"`。

**行为约定(设计 §6.1):**
- `kind == "platform"`:请求必须命中平台 host(`core.GetPlatformHost(ctx) == true`),否则 404。
- `kind == "tenant"`:请求必须**不是**平台 host(`core.GetPlatformHost(ctx) == false`),否则 404。
- `base_domain` 未配置时(`TenantResolver` no-op,`isPlatformHost` 恒为 `false`):`kind == "tenant"` 放行(开发模式向后兼容,现有单租户部署继续用 `/admin/*` 走 tenant 分组);`kind == "platform"` 返回 404(开发模式下没有平台 host 概念,平台专属端点如租户 CRUD 在本地开发时不可达——这是已知取舍,记入 Completion Notes)。

- [ ] **Step 1: 写失败测试**

创建 `internal/server/host_guard_test.go`:

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/core"
)

func TestHostGuard_Platform_Allows(t *testing.T) {
	e := echo.New()
	e.Use(hostGuard("platform"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(core.WithPlatformHost(req.Context(), true))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestHostGuard_Platform_RejectsTenantHost(t *testing.T) {
	e := echo.New()
	e.Use(hostGuard("platform"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHostGuard_Tenant_Allows(t *testing.T) {
	e := echo.New()
	e.Use(hostGuard("tenant"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestHostGuard_Tenant_RejectsPlatformHost(t *testing.T) {
	e := echo.New()
	e.Use(hostGuard("tenant"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(core.WithPlatformHost(req.Context(), true))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHostGuard_Tenant_AllowsDevMode(t *testing.T) {
	// base_domain 未配置:isPlatformHost 恒为 false,tenant 分组放行
	e := echo.New()
	e.Use(hostGuard("tenant"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/... -run TestHostGuard -v`
Expected: 编译失败(`hostGuard` 未定义)。

- [ ] **Step 3: 实现 `internal/server/host_guard.go`**

```go
package server

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/core"
)

// hostGuard restricts a route group to requests matching the given Host
// kind ("platform" or "tenant"), returning 404 on mismatch so the other
// kind's routes never appear to exist on the wrong host.
func hostGuard(kind string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			isPlatform := core.GetPlatformHost(c.Request().Context())
			switch kind {
			case "platform":
				if !isPlatform {
					return c.NoContent(http.StatusNotFound)
				}
			case "tenant":
				if isPlatform {
					return c.NoContent(http.StatusNotFound)
				}
			}
			return next(c)
		}
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/server/... -run TestHostGuard -v`
Expected: 5 个测试全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/server/host_guard.go internal/server/host_guard_test.go
git commit -m "feat(server): add hostGuard middleware for platform/tenant route separation"
```

---

### Task 3: `internal/virtualmodels` — `ForTenant` 管理面方法

**Files:**
- Create: `internal/virtualmodels/tenant_admin.go`
- Create: `internal/virtualmodels/tenant_admin_test.go`
- Modify: `internal/virtualmodels/service.go`(把 `Upsert` 里的校验/规范化逻辑抽成私有 helper,供新方法复用)

**Interfaces:**
- Produces: `Service.ListForTenant(ctx, tenantID) ([]VirtualModel, error)`、`Service.ListEffectiveForTenant(ctx, tenantID) ([]VirtualModel, error)`、`Service.GetForTenant(ctx, tenantID, source) (*VirtualModel, error)`、`Service.UpsertForTenant(ctx, tenantID, vm VirtualModel) error`、`Service.DeleteForTenant(ctx, tenantID, source string) error`。

- [ ] **Step 1: 写失败测试**

创建 `internal/virtualmodels/tenant_admin_test.go`(用真实 SQLite store,不用 mock,确保覆盖率与隔离性一致于 P3 的测试风格;执行前 `grep -n "func newTestSQLiteStore\|func newTestService" internal/virtualmodels/*_test.go` 确认现有 helper 名并复用):

```go
package virtualmodels

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_UpsertForTenant_Isolation(t *testing.T) {
	store := newTestSQLiteStore(t) // 复用现有 helper
	svc, err := NewService(store, stubCatalog{}, true)
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", VirtualModel{Source: "s1", Targets: []Target{{Model: "m1", Provider: "openai"}}}))
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-b", VirtualModel{Source: "s1", Targets: []Target{{Model: "m2", Provider: "openai"}}}))

	gotA, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, "m1", gotA[0].Targets[0].Model)

	gotB, err := svc.ListForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, "m2", gotB[0].Targets[0].Model)
}

func TestService_UpsertForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, stubCatalog{}, true)
	require.NoError(t, err)
	before := svc.List() // 共享缓存(始终代表 "default" 租户)初始为空

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", VirtualModel{Source: "s1", Targets: []Target{{Model: "m1", Provider: "openai"}}}))

	require.Equal(t, before, svc.List(), "non-default tenant write must not touch the shared cache")
}

func TestService_UpsertForTenant_DefaultTenant_RefreshesSharedCache(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, stubCatalog{}, true)
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "default", VirtualModel{Source: "s1", Targets: []Target{{Model: "m1", Provider: "openai"}}}))

	require.Len(t, svc.List(), 1, "default-tenant write must refresh the shared cache like Upsert does")
}

func TestService_DeleteForTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, stubCatalog{}, true)
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", VirtualModel{Source: "s1", Targets: []Target{{Model: "m1", Provider: "openai"}}}))

	require.NoError(t, svc.DeleteForTenant(context.Background(), "tenant-a", "s1"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}
```

执行前 `grep -n "type stubCatalog\|func.*Catalog" internal/virtualmodels/*_test.go` 确认现有 `Catalog` 测试替身的类型名,替换成实际名称(若不存在,按 `Catalog` 接口最小实现构造)。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/virtualmodels/... -run TestService_.*ForTenant -v`
Expected: 编译失败(`ListForTenant`/`ListEffectiveForTenant`/`UpsertForTenant`/`DeleteForTenant` 未定义)。

- [ ] **Step 3: 抽取 `Upsert` 的规范化逻辑到私有 helper**

打开 `internal/virtualmodels/service.go`,找到现有 `Upsert(ctx context.Context, vm VirtualModel) error` 方法的完整实现。把函数体里"生成 `normalized`(校验/默认值填充/`ResolveUpsertEnabled` 等)"的部分——即除最后 `s.store.Upsert(ctx, s.tenantID, normalized)` 与 `return s.Refresh(ctx)` 之外的全部逻辑——搬到新私有方法:

```go
// normalizeForUpsert validates and fills defaults on vm, returning the
// row ready to persist. Extracted from Upsert so ForTenant variants share
// the same validation without duplicating it.
func (s *Service) normalizeForUpsert(vm VirtualModel) (VirtualModel, error) {
	// 把原 Upsert 方法体里生成 normalized 的那部分逻辑原样搬到这里,
	// 最终 return normalized, nil(或校验失败时的错误)。
}
```

`Upsert` 改为:

```go
func (s *Service) Upsert(ctx context.Context, vm VirtualModel) error {
	normalized, err := s.normalizeForUpsert(vm)
	if err != nil {
		return err
	}
	if err := s.store.Upsert(ctx, s.tenantID, normalized); err != nil {
		return err
	}
	return s.Refresh(ctx)
}
```

**在这一步跑一次现有全量测试,确认这个纯重构不引入任何行为变化:**

Run: `go test ./internal/virtualmodels/... -v`
Expected: 全部 PASS(与重构前完全一致的测试集合)。若不一致,说明抽取时遗漏或改变了逻辑,回头核对原 `Upsert` 实现逐行搬运。

- [ ] **Step 4: 实现 `internal/virtualmodels/tenant_admin.go`**

```go
package virtualmodels

import "context"

// ListForTenant returns the raw override rows stored for tenantID (no
// platform-default merge), bypassing the shared inference-time cache.
// Intended for admin handlers managing one tenant's overrides directly.
func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	return s.store.List(ctx, tenantID)
}

// ListEffectiveForTenant returns tenantID's effective view (tenant
// override merged over the platform default), bypassing the shared cache.
func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	return s.store.ListEffective(ctx, tenantID)
}

// GetForTenant returns a single row scoped to tenantID, bypassing the cache.
func (s *Service) GetForTenant(ctx context.Context, tenantID, source string) (*VirtualModel, error) {
	return s.store.Get(ctx, tenantID, source)
}

// UpsertForTenant upserts vm under tenantID. The shared inference-time
// cache is refreshed only when tenantID is the platform-default tenant
// (s.tenantID) — a non-default tenant's override does not affect live
// routing until P5 makes the cache tenant-aware.
func (s *Service) UpsertForTenant(ctx context.Context, tenantID string, vm VirtualModel) error {
	normalized, err := s.normalizeForUpsert(vm)
	if err != nil {
		return err
	}
	if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
		return err
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}

// DeleteForTenant deletes source under tenantID. See UpsertForTenant for
// the shared-cache refresh rule.
func (s *Service) DeleteForTenant(ctx context.Context, tenantID, source string) error {
	if err := s.store.Delete(ctx, tenantID, source); err != nil {
		return err
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/virtualmodels/... -v`
Expected: 全部 PASS(含新测试)。

- [ ] **Step 6: 全量构建 + 提交**

Run: `go build ./...`

```bash
git add internal/virtualmodels/
git commit -m "feat(virtualmodels): add ForTenant admin methods bypassing the shared cache"
```

---

### Task 4: `internal/failover` — `ForTenant` 管理面方法

**Files:**
- Create: `internal/failover/tenant_admin.go`
- Create: `internal/failover/tenant_admin_test.go`
- Modify: `internal/failover/service.go`(抽取 `Upsert` 规范化逻辑)

**Interfaces:**
- Produces: `Service.ListForTenant(ctx, tenantID) ([]Rule, error)`、`Service.ListEffectiveForTenant(ctx, tenantID) ([]Rule, error)`、`Service.GetForTenant(ctx, tenantID, source) (*Rule, error)`、`Service.UpsertForTenant(ctx, tenantID, rule Rule) error`、`Service.DeleteForTenant(ctx, tenantID, source string) error`、`Service.DeleteAllForTenant(ctx, tenantID string) error`。

**背景:** `failover.Service` 目前没有 `tenantID` 字段,所有写方法直接把字面量 `"default"` 传给 store(`service.go:210,217,231,241`)。`ForTenant` 变体需要一个判断"是否等于默认租户"的常量,新增包级常量 `const defaultTenantID = "default"`,替换掉 `Refresh`/`Upsert`/`Delete`/`ResetDashboardRules` 里的字面量 `"default"`(纯重构,不改变行为,与 Task 3 Step 3 同理需要先跑现有测试确认无回归)。

- [ ] **Step 1: 写失败测试**

创建 `internal/failover/tenant_admin_test.go`(执行前 `grep -n "func newTestSQLiteStore" internal/failover/*_test.go` 确认 helper):

```go
package failover

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"smartrouter/config"
)

func TestService_UpsertForTenant_Isolation(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, config.FailoverConfig{})
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Rule{Source: "s1", Fallbacks: []string{"f1"}}))
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-b", Rule{Source: "s1", Fallbacks: []string{"f2"}}))

	gotA, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, []string{"f1"}, gotA[0].Fallbacks)

	gotB, err := svc.ListForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, []string{"f2"}, gotB[0].Fallbacks)
}

func TestService_UpsertForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, config.FailoverConfig{})
	require.NoError(t, err)
	before := svc.List()

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Rule{Source: "s1", Fallbacks: []string{"f1"}}))

	require.Equal(t, before, svc.List())
}

func TestService_DeleteForTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, config.FailoverConfig{})
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Rule{Source: "s1", Fallbacks: []string{"f1"}}))

	require.NoError(t, svc.DeleteForTenant(context.Background(), "tenant-a", "s1"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestService_DeleteAllForTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, config.FailoverConfig{})
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Rule{Source: "s1", Fallbacks: []string{"f1"}}))

	require.NoError(t, svc.DeleteAllForTenant(context.Background(), "tenant-a"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/failover/... -run TestService_.*ForTenant -v`
Expected: 编译失败。

- [ ] **Step 3: 引入 `defaultTenantID` 常量替换字面量(纯重构)**

在 `internal/failover/service.go` 顶部加 `const defaultTenantID = "default"`,把 `Refresh`(`store.ListEffective(ctx, "default")`)、`Upsert`(`store.Get`/`store.Upsert` 的 `"default"`)、`Delete`(`store.Delete` 的 `"default"`)、`ResetDashboardRules`(`store.DeleteAll` 的 `"default"`)里的字面量 `"default"` 全部替换成 `defaultTenantID`。

Run: `go test ./internal/failover/... -v`
Expected: 全部 PASS(纯重构,行为不变)。

- [ ] **Step 4: 抽取 `Upsert` 规范化逻辑**

同 Task 3 Step 3 的做法:把 `Upsert` 方法里生成 `normalized`(校验 `Source`/`Fallbacks` 等)的部分抽到 `func (s *Service) normalizeForUpsert(rule Rule) (Rule, error)`,`Upsert` 改为调用该 helper 后传 `defaultTenantID`。跑 `go test ./internal/failover/... -v` 确认无回归。

- [ ] **Step 5: 实现 `internal/failover/tenant_admin.go`**

```go
package failover

import "context"

func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]Rule, error) {
	return s.store.List(ctx, tenantID)
}

func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]Rule, error) {
	return s.store.ListEffective(ctx, tenantID)
}

func (s *Service) GetForTenant(ctx context.Context, tenantID, source string) (*Rule, error) {
	return s.store.Get(ctx, tenantID, source)
}

func (s *Service) UpsertForTenant(ctx context.Context, tenantID string, rule Rule) error {
	normalized, err := s.normalizeForUpsert(rule)
	if err != nil {
		return err
	}
	if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
		return err
	}
	if tenantID == defaultTenantID {
		return s.Refresh(ctx)
	}
	return nil
}

func (s *Service) DeleteForTenant(ctx context.Context, tenantID, source string) error {
	if err := s.store.Delete(ctx, tenantID, source); err != nil {
		return err
	}
	if tenantID == defaultTenantID {
		return s.Refresh(ctx)
	}
	return nil
}

func (s *Service) DeleteAllForTenant(ctx context.Context, tenantID string) error {
	if err := s.store.DeleteAll(ctx, tenantID); err != nil {
		return err
	}
	if tenantID == defaultTenantID {
		return s.Refresh(ctx)
	}
	return nil
}
```

- [ ] **Step 6: 运行测试确认通过 + 全量构建 + 提交**

Run: `go test ./internal/failover/... -v && go build ./...`

```bash
git add internal/failover/
git commit -m "feat(failover): add ForTenant admin methods bypassing the shared cache"
```

---

### Task 5: `internal/guardrails` — `ForTenant` 管理面方法

**Files:**
- Create: `internal/guardrails/tenant_admin.go`
- Create: `internal/guardrails/tenant_admin_test.go`
- Modify: `internal/guardrails/service.go`(抽取 `Upsert` 规范化逻辑)

**Interfaces:**
- Produces: `Service.ListForTenant(ctx, tenantID) ([]Definition, error)`、`Service.ListEffectiveForTenant(ctx, tenantID) ([]Definition, error)`、`Service.GetForTenant(ctx, tenantID, name) (*Definition, bool, error)`、`Service.UpsertForTenant(ctx, tenantID, def Definition) error`、`Service.DeleteForTenant(ctx, tenantID, name string) error`。

**注意:** `guardrails.Store` 目前的 `List`/`Get` 接口签名执行前需要 `grep -n "type Store interface" -A 10 internal/guardrails/store.go` 确认(P3 只保证加了 `tenantID` 参数,具体是否有 `Get(ctx, tenantID, name)` 需核实——若没有 `Get`,`GetForTenant` 改为对 `ListForTenant` 结果做本地查找)。

- [ ] **Step 1: 写失败测试**

创建 `internal/guardrails/tenant_admin_test.go`(执行前 `grep -n "func newTestSQLiteStore\|func.*Definition{" internal/guardrails/*_test.go` 确认 helper 与 `Definition` 构造字段):

```go
package guardrails

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_UpsertForTenant_Isolation(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store)
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Definition{Name: "g1", Type: "pii_redaction"}))
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-b", Definition{Name: "g1", Type: "profanity_filter"}))

	gotA, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, "pii_redaction", gotA[0].Type)

	gotB, err := svc.ListForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, "profanity_filter", gotB[0].Type)
}

func TestService_UpsertForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store)
	require.NoError(t, err)
	before := svc.List()

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Definition{Name: "g1", Type: "pii_redaction"}))

	require.Equal(t, before, svc.List())
}

func TestService_DeleteForTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store)
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Definition{Name: "g1", Type: "pii_redaction"}))

	require.NoError(t, svc.DeleteForTenant(context.Background(), "tenant-a", "g1"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}
```

调整 `Definition{...}` 字面量字段为实际类型定义(执行前 `grep -n "type Definition struct" -A 15 internal/guardrails/*.go`)。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/guardrails/... -run TestService_.*ForTenant -v`

- [ ] **Step 3: 抽取 `Upsert` 规范化逻辑(纯重构,先验证无回归)**

同前面任务的模式:把 `Upsert` 里 `normalizeDefinition(definition)` 之后、`s.store.List(ctx, s.tenantID)` 之前到 `s.store.Upsert(ctx, s.tenantID, normalized)` 之间"读现有 + 合并 + 建 snapshot"的逻辑抽成 `func (s *Service) buildUpsertSnapshot(ctx context.Context, tenantID string, normalized Definition) (serviceSnapshot, error)`(注意这个 helper 需要 `ctx` 与 `tenantID`,因为它内部要读 `s.store.List(ctx, tenantID)`,与 virtualmodels/failover 的抽取方式略有不同——guardrails 的 Upsert 逻辑依赖"先读现有全集再重建 snapshot",不能像 virtualmodels 那样是纯校验函数)。`Upsert` 改为:

```go
func (s *Service) Upsert(ctx context.Context, definition Definition) error {
	normalized, err := normalizeDefinition(definition)
	if err != nil {
		return err
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	next, err := s.buildUpsertSnapshot(ctx, s.tenantID, normalized)
	if err != nil {
		return err
	}
	if err := s.store.Upsert(ctx, s.tenantID, normalized); err != nil {
		return guardrailServiceError("upsert guardrail", err)
	}
	s.mu.Lock()
	s.snapshot = next
	s.mu.Unlock()
	return nil
}
```

Run: `go test ./internal/guardrails/... -v`
Expected: 全部 PASS(无回归)。

- [ ] **Step 4: 实现 `internal/guardrails/tenant_admin.go`**

```go
package guardrails

import "context"

func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]Definition, error) {
	return s.store.List(ctx, tenantID)
}

func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]Definition, error) {
	return s.store.ListEffective(ctx, tenantID)
}

func (s *Service) GetForTenant(ctx context.Context, tenantID, name string) (*Definition, error) {
	rows, err := s.store.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func (s *Service) UpsertForTenant(ctx context.Context, tenantID string, definition Definition) error {
	normalized, err := normalizeDefinition(definition)
	if err != nil {
		return err
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if tenantID == s.tenantID {
		next, err := s.buildUpsertSnapshot(ctx, tenantID, normalized)
		if err != nil {
			return err
		}
		if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
			return guardrailServiceError("upsert guardrail", err)
		}
		s.mu.Lock()
		s.snapshot = next
		s.mu.Unlock()
		return nil
	}
	if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
		return guardrailServiceError("upsert guardrail", err)
	}
	return nil
}

func (s *Service) DeleteForTenant(ctx context.Context, tenantID, name string) error {
	if err := s.store.Delete(ctx, tenantID, name); err != nil {
		return guardrailServiceError("delete guardrail", err)
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}
```

`GetForTenant` 返回值签名按 Step 1 测试预期是否需要 `(*Definition, error)` 还是 `(*Definition, bool, error)`——执行前核对现有 `Get(name)` 的返回形状(`Get(name string) (*Definition, bool)`,无 `error`),为保持包内一致性,`GetForTenant` 采用 `(*Definition, error)`(找不到时返回 `nil, nil`,与 store 层 List 后本地过滤的语义一致,不强行套用 `Get` 的 `bool` 形状,因为这里是直接读 store 而非缓存)。若与 Step 1 测试的调用方式不一致,以此处签名为准调整测试。

- [ ] **Step 5: 运行测试确认通过 + 全量构建 + 提交**

Run: `go test ./internal/guardrails/... -v && go build ./...`

```bash
git add internal/guardrails/
git commit -m "feat(guardrails): add ForTenant admin methods bypassing the shared cache"
```

---

### Task 6: `internal/workflows` — `ForTenant` 管理面方法

**Files:**
- Create: `internal/workflows/tenant_admin.go`
- Create: `internal/workflows/tenant_admin_test.go`

**Interfaces:**
- Produces: `Service.ListActiveForTenant(ctx, tenantID) ([]Version, error)`、`Service.ListEffectiveForTenant(ctx, tenantID) ([]Version, error)`、`Service.GetForTenant(ctx, tenantID, id) (*Version, error)`、`Service.CreateForTenant(ctx, tenantID, input CreateInput) (*Version, error)`、`Service.DeactivateForTenant(ctx, tenantID, id string) error`。

**注意:** 与前三个任务不同,workflows 的写路径(`Create`/`Deactivate`)目前直接刷新**唯一**的全局 `snapshot`(`s.current`,由 `refreshLocked` 全量重建,或 `storeActivatedCompiledLocked`/`storeDeactivatedVersionLocked` 增量修改)。P4 的原则是"非默认租户的写操作完全不碰共享缓存"——对 workflows 来说,`CreateForTenant`/`DeactivateForTenant` 在 `tenantID != s.tenantID` 时只调用 store,**不调用**任何缓存重建函数(既不调 `Refresh`,也不调 `storeActivatedCompiledLocked` 之类的增量方法),避免非默认租户的 workflow 意外污染进程级的 `CompiledWorkflow` 缓存(该缓存被推理热路径的 `Match`/`PipelineForContext` 直接读取)。

- [ ] **Step 1: 写失败测试**

创建 `internal/workflows/tenant_admin_test.go`(执行前 `grep -n "func newTestSQLiteStore\|type CreateInput struct" internal/workflows/*.go` 确认 helper 与 `CreateInput` 字段):

```go
package workflows

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_CreateForTenant_Isolation(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, stubCompiler{})
	require.NoError(t, err)

	verA, err := svc.CreateForTenant(context.Background(), "tenant-a", CreateInput{Name: "wf-a"})
	require.NoError(t, err)
	verB, err := svc.CreateForTenant(context.Background(), "tenant-b", CreateInput{Name: "wf-b"})
	require.NoError(t, err)

	listA, err := svc.ListActiveForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, listA, 1)
	require.Equal(t, verA.ID, listA[0].ID)

	listB, err := svc.ListActiveForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, listB, 1)
	require.Equal(t, verB.ID, listB[0].ID)
}

func TestService_CreateForTenant_DoesNotTouchSharedSnapshot(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, stubCompiler{})
	require.NoError(t, err)
	require.NoError(t, svc.EnsureDefaultGlobal(context.Background(), CreateInput{Name: "global"}))

	_, err = svc.Match(core.WorkflowSelector{}) // 走默认全局 workflow,不应报错
	require.NoError(t, err)

	_, err = svc.CreateForTenant(context.Background(), "tenant-a", CreateInput{Name: "wf-a"})
	require.NoError(t, err)

	_, err = svc.Match(core.WorkflowSelector{}) // 共享缓存不受影响,仍能正常匹配默认全局
	require.NoError(t, err)
}
```

执行前 `grep -n "type stubCompiler\|Compiler interface" internal/workflows/*_test.go internal/workflows/*.go` 确认 `Compiler` 接口测试替身名;`core.WorkflowSelector{}` 的具体零值是否能匹配默认全局 workflow,需要执行前用现有测试(如 `service_test.go` 里 `TestService_Match` 一类)核对匹配条件,若零值选择器不匹配,替换成现有测试里验证默认全局命中的实际选择器构造方式。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/workflows/... -run TestService_.*ForTenant -v`

- [ ] **Step 3: 实现 `internal/workflows/tenant_admin.go`**

```go
package workflows

import "context"

func (s *Service) ListActiveForTenant(ctx context.Context, tenantID string) ([]Version, error) {
	return s.store.ListActive(ctx, tenantID)
}

func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]Version, error) {
	return s.store.ListEffective(ctx, tenantID)
}

func (s *Service) GetForTenant(ctx context.Context, tenantID, id string) (*Version, error) {
	return s.store.Get(ctx, tenantID, id)
}

// CreateForTenant persists a new workflow version under tenantID. The
// shared compiled-workflow cache used by the live inference path is
// rebuilt only when tenantID is the platform-default tenant (s.tenantID);
// a non-default tenant's workflow does not affect live traffic until P5.
func (s *Service) CreateForTenant(ctx context.Context, tenantID string, input CreateInput) (*Version, error) {
	normalized, err := s.normalizeCreateInput(input)
	if err != nil {
		return nil, err
	}
	version, err := s.store.Create(ctx, tenantID, normalized)
	if err != nil {
		return nil, err
	}
	if tenantID == s.tenantID {
		if err := s.Refresh(ctx); err != nil {
			return nil, err
		}
	}
	return version, nil
}

func (s *Service) DeactivateForTenant(ctx context.Context, tenantID, id string) error {
	version, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if err := s.store.Deactivate(ctx, tenantID, version.ID); err != nil {
		return err
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}
```

`normalizeCreateInput` 需要从现有 `Create` 方法里抽取(执行前打开 `internal/workflows/service.go` 的 `Create` 方法全文,把生成/校验 `normalized` 输入的部分抽成 `func (s *Service) normalizeCreateInput(input CreateInput) (CreateInput, error)`,`Create` 相应改为调用该 helper——与 Task 3 Step 3 同样先跑 `go test ./internal/workflows/... -v` 确认这步纯重构无回归,再继续实现 `CreateForTenant`)。

- [ ] **Step 4: 运行测试确认通过 + 全量构建 + 提交**

Run: `go test ./internal/workflows/... -v && go build ./...`

```bash
git add internal/workflows/
git commit -m "feat(workflows): add ForTenant admin methods bypassing the shared cache"
```

---

### Task 7: `internal/pricingoverrides` — `ForTenant` 管理面方法

**Files:**
- Create: `internal/pricingoverrides/tenant_admin.go`
- Create: `internal/pricingoverrides/tenant_admin_test.go`
- Modify: `internal/pricingoverrides/service.go`(抽取 `Upsert`/`Delete` 规范化与回滚逻辑)

**Interfaces:**
- Produces: `Service.ListForTenant(ctx, tenantID) ([]Override, error)`、`Service.ListEffectiveForTenant(ctx, tenantID) ([]Override, error)`、`Service.GetForTenant(ctx, tenantID, selector) (*Override, error)`、`Service.UpsertForTenant(ctx, tenantID, override Override) error`、`Service.DeleteForTenant(ctx, tenantID, selector string) error`。

**注意:** `pricingoverrides.Service` 的推理热路径方法 `ResolvePricing(model, providerName string) *core.ModelPricing` 没有 `ctx` 参数,本任务**不改**这个方法或 `usage.PricingResolver` 接口——那属于 P5(见计划开头 Architecture 说明)。这里只加管理面的 `ForTenant` 方法。

- [ ] **Step 1: 写失败测试**

创建 `internal/pricingoverrides/tenant_admin_test.go`(执行前 `grep -n "func newTestSQLiteStore\|type Override struct" internal/pricingoverrides/*.go` 确认 helper 与字段):

```go
package pricingoverrides

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_UpsertForTenant_Isolation(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, stubCatalog{}, nil)
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Override{Selector: "openai:gpt-4o"}))

	gotA, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)

	gotB, err := svc.ListForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Empty(t, gotB)
}

func TestService_UpsertForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, stubCatalog{}, nil)
	require.NoError(t, err)
	before := svc.List()

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Override{Selector: "openai:gpt-4o"}))

	require.Equal(t, before, svc.List())
}

func TestService_DeleteForTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, stubCatalog{}, nil)
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Override{Selector: "openai:gpt-4o"}))

	require.NoError(t, svc.DeleteForTenant(context.Background(), "tenant-a", "openai:gpt-4o"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/pricingoverrides/... -run TestService_.*ForTenant -v`

- [ ] **Step 3: 抽取 `Upsert`/`Delete` 规范化逻辑(纯重构,先验证无回归)**

打开 `internal/pricingoverrides/service.go` 的 `Upsert`(127-164)与 `Delete`(167-202)全文。把两者里"校验 `Override`/构造 rollback 快照"的部分分别抽成:

```go
func (s *Service) normalizeForUpsert(override Override) (Override, error) { ... }
func (s *Service) prepareDelete(selector string) (rollback func(), err error) { ... } // 若 Delete 现有实现有 rollback 概念,按实际结构调整签名
```

具体抽取方式取决于 `Delete` 现有实现里 rollback 快照的具体做法——执行前完整阅读该方法,若逻辑与 `store.Delete` 调用强耦合难以拆开,退而求其次:保留 `Delete` 原样不动,`DeleteForTenant` 直接调用 `s.store.Delete(ctx, tenantID, selector)`(不含 rollback,因为 `ForTenant` 路径本来就不动共享缓存,没有需要回滚的缓存状态)。

Run: `go test ./internal/pricingoverrides/... -v`
Expected: 全部 PASS(无回归)。

- [ ] **Step 4: 实现 `internal/pricingoverrides/tenant_admin.go`**

```go
package pricingoverrides

import "context"

func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]Override, error) {
	return s.store.List(ctx, tenantID)
}

func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]Override, error) {
	return s.store.ListEffective(ctx, tenantID)
}

func (s *Service) GetForTenant(ctx context.Context, tenantID, selector string) (*Override, error) {
	rows, err := s.store.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Selector == selector {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func (s *Service) UpsertForTenant(ctx context.Context, tenantID string, override Override) error {
	normalized, err := s.normalizeForUpsert(override)
	if err != nil {
		return err
	}
	if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
		return err
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}

func (s *Service) DeleteForTenant(ctx context.Context, tenantID, selector string) error {
	if err := s.store.Delete(ctx, tenantID, selector); err != nil {
		return err
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}
```

- [ ] **Step 5: 运行测试确认通过 + 全量构建 + 提交**

Run: `go test ./internal/pricingoverrides/... -v && go build ./...`

```bash
git add internal/pricingoverrides/
git commit -m "feat(pricingoverrides): add ForTenant admin methods bypassing the shared cache"
```

---

### Task 8: `internal/tagging` — `ForTenant` 管理面方法

**Files:**
- Create: `internal/tagging/tenant_admin.go`
- Create: `internal/tagging/tenant_admin_test.go`

**Interfaces:**
- Produces: `Service.GetRulesForTenant(ctx, tenantID) ([]Rule, error)`、`Service.ListEffectiveRulesForTenant(ctx, tenantID) ([]Rule, error)`、`Service.SaveRulesForTenant(ctx, tenantID, rules []Rule) ([]Rule, error)`。

**注意:** `tagging.Service` 的推理热路径方法 `ExtractLabels`/`StripHeaders` 没有 `ctx` 参数,本任务不改这两个方法——那属于 P5。

- [ ] **Step 1: 写失败测试**

创建 `internal/tagging/tenant_admin_test.go`(执行前 `grep -n "func newTestSQLiteStore\|type Rule struct" internal/tagging/*.go` 确认 helper 与字段):

```go
package tagging

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_SaveRulesForTenant_Isolation(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc := NewService(nil, store)

	_, err := svc.SaveRulesForTenant(context.Background(), "tenant-a", []Rule{{Header: "X-Tag-A", Label: "a"}})
	require.NoError(t, err)
	_, err = svc.SaveRulesForTenant(context.Background(), "tenant-b", []Rule{{Header: "X-Tag-B", Label: "b"}})
	require.NoError(t, err)

	gotA, err := svc.GetRulesForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, "X-Tag-A", gotA[0].Header)

	gotB, err := svc.GetRulesForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, "X-Tag-B", gotB[0].Header)
}

func TestService_SaveRulesForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc := NewService(nil, store)
	before := svc.Rules()

	_, err := svc.SaveRulesForTenant(context.Background(), "tenant-a", []Rule{{Header: "X-Tag-A", Label: "a"}})
	require.NoError(t, err)

	require.Equal(t, before, svc.Rules())
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/tagging/... -run TestService_.*ForTenant -v`

- [ ] **Step 3: 实现 `internal/tagging/tenant_admin.go`**

```go
package tagging

import "context"

func (s *Service) GetRulesForTenant(ctx context.Context, tenantID string) ([]Rule, error) {
	return s.store.GetRules(ctx, tenantID)
}

func (s *Service) ListEffectiveRulesForTenant(ctx context.Context, tenantID string) ([]Rule, error) {
	return s.store.ListEffectiveRules(ctx, tenantID)
}

// SaveRulesForTenant persists rules for tenantID. The shared in-memory
// snapshot used by the live TaggingCapture middleware is refreshed only
// when tenantID is the platform-default tenant (s.tenantID); a non-default
// tenant's rules do not affect live traffic until P5.
func (s *Service) SaveRulesForTenant(ctx context.Context, tenantID string, rules []Rule) ([]Rule, error) {
	if err := NormalizeRules(rules); err != nil {
		return nil, fmt.Errorf("tagging rules: %w", err)
	}
	if err := s.store.SaveRules(ctx, tenantID, rules); err != nil {
		return nil, err
	}
	if tenantID == s.tenantID {
		if err := s.Refresh(ctx); err != nil {
			return nil, err
		}
	}
	return rules, nil
}
```

加 `"fmt"` import。（`NormalizeRules` 的确切签名执行前 `grep -n "func NormalizeRules" internal/tagging/*.go` 确认——若返回值不是纯 `error`,按实际签名调整。）

- [ ] **Step 4: 运行测试确认通过 + 全量构建 + 提交**

Run: `go test ./internal/tagging/... -v && go build ./...`

```bash
git add internal/tagging/
git commit -m "feat(tagging): add ForTenant admin methods bypassing the shared cache"
```

---

### Task 9: `internal/authkeys` — Service 层补齐显式 `tenantID`

**Files:**
- Modify: `internal/authkeys/service.go`(`List`/`UpdateLabels`/`Deactivate`/`ListViews` 加 `tenantID` 参数)
- Test: `internal/authkeys/service_test.go`
- Modify: 所有现存调用点(`internal/admin/handler_authkeys.go` 等)——本任务只改 Service 签名与其现存调用点,不新建 admin handler(那是 Task 10)。

**Interfaces:**
- Produces: `Service.List(ctx, tenantID string) ([]AuthKey, error)`、`Service.UpdateLabels(ctx, tenantID, id string, labels []string) (AuthKey, error)`、`Service.Deactivate(ctx, tenantID, id string) error`、`Service.ListViews(tenantID string) []View`(或若现有 `ListViews` 已经是 `ctx`-based,保留 `ctx` 参数,新增 `tenantID`)。

**行为约定:** `tenantID == ""` = 不限定(跨租户,P3 已确定的约定),`tenantID != ""` = 只返回/操作该租户的 key。

- [ ] **Step 1: 写失败测试**

在 `internal/authkeys/service_test.go` 追加(执行前 `grep -n "func newTestService\|func.*List(ctx\|func.*ListViews" internal/authkeys/service.go internal/authkeys/service_test.go` 确认当前精确签名):

```go
func TestService_List_ScopedByTenant(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{Name: "a-key", TenantID: "tenant-a"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, CreateInput{Name: "b-key", TenantID: "tenant-b"})
	require.NoError(t, err)

	listA, err := svc.List(ctx, "tenant-a")
	require.NoError(t, err)
	require.Len(t, listA, 1)
	require.Equal(t, "a-key", listA[0].Name)

	listAll, err := svc.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, listAll, 2)
}

func TestService_ListViews_ScopedByTenant(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{Name: "a-key", TenantID: "tenant-a"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, CreateInput{Name: "b-key", TenantID: "tenant-b"})
	require.NoError(t, err)

	require.NoError(t, svc.Refresh(ctx))
	viewsA := svc.ListViews("tenant-a")
	require.Len(t, viewsA, 1)
}

func TestService_Deactivate_ScopedByTenant(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	issued, err := svc.Create(ctx, CreateInput{Name: "a-key", TenantID: "tenant-a"})
	require.NoError(t, err)

	// 用错误的 tenantID 去停用应该失败(找不到该租户下这个 id)
	err = svc.Deactivate(ctx, "tenant-b", issued.ID)
	require.Error(t, err)

	require.NoError(t, svc.Deactivate(ctx, "tenant-a", issued.ID))
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/authkeys/... -run "TestService_List_ScopedByTenant|TestService_ListViews_ScopedByTenant|TestService_Deactivate_ScopedByTenant" -v`
Expected: 编译失败(参数数量不匹配)。

- [ ] **Step 3: 修改 `Service` 方法签名**

打开 `internal/authkeys/service.go`,把现有 `List(ctx)`(内部传 `""` 给 `store.List`)改成 `List(ctx context.Context, tenantID string) ([]AuthKey, error)`,把内部的 `s.store.List(ctx, "")` 改成 `s.store.List(ctx, tenantID)`。对 `UpdateLabels`/`Deactivate` 做同样改动:签名加 `tenantID string` 作为第二个参数(`ctx` 之后、原有 `id` 之前),内部把原来传 `""` 的位置改传 `tenantID`。`ListViews` 若现有签名是 `ListViews() []View`(读内部缓存,无参数),改成 `ListViews(tenantID string) []View`,在遍历缓存时按 `key.TenantID == tenantID`(或 `tenantID == ""` 时不过滤)过滤。

- [ ] **Step 4: 更新所有现存调用点**

Run: `grep -rn "\.List(ctx)\|\.UpdateLabels(ctx\|\.Deactivate(ctx\|\.ListViews()" internal/admin/handler_authkeys.go internal/authkeys/*.go`

把每个调用点按新签名补上 `tenantID` 参数。`internal/admin/handler_authkeys.go` 目前是全局 `Handler`(Task 10 才拆分),这一步先传 `""`(不限定,保持现有单一 handler 的行为不变),Task 10 会把这些调用点迁移到 `TenantAdminHandler`(传 `core.GetTenantID(ctx)`)与 `PlatformAdminHandler`(传显式目标 tenantID 或 `""`)。

- [ ] **Step 5: 运行测试确认通过 + 全量构建 + 提交**

Run: `go test ./internal/authkeys/... -v && go build ./...`

```bash
git add internal/authkeys/ internal/admin/handler_authkeys.go
git commit -m "feat(authkeys): thread explicit tenantID through Service List/UpdateLabels/Deactivate/ListViews"
```

---

### Task 10: `internal/admin` — 拆分 `PlatformAdminHandler` / `TenantAdminHandler`

**Files:**
- Create: `internal/admin/platform_handler.go`、`internal/admin/platform_routes.go`、`internal/admin/platform_handler_test.go`
- Create: `internal/admin/tenant_handler.go`、`internal/admin/tenant_routes.go`、`internal/admin/tenant_handler_test.go`
- Modify: 现有 `handler_*.go` 文件——把方法接收者从单一 `*Handler` 分流到 `*PlatformAdminHandler`/`*TenantAdminHandler`(见下方分工表),共享的辅助函数(`handleError`、日期解析等)保持包级函数不变,供两者共用。
- 保留(不删除):`internal/admin/handler.go`、`routes.go` 暂时保留但不再被 `http.go` 引用(Task 11 处理接线;若确认无引用,Task 13 验证阶段可以决定是否物理删除——本任务先只做拆分,不做清理,避免一个任务里混编"新增"与"删除"两种性质的改动)。

**端点分工(对照设计 §7.1/§7.2):**

| Handler | 端点 | 底层调用 |
|---|---|---|
| `PlatformAdminHandler` | `POST/GET/PATCH/DELETE /admin/tenants`、`POST /admin/tenants/:id/admin-keys` | `tenants.Service`(Task 1 新增的 `Update`)、`authkeys.Service.Create`(强制 `IsTenantAdmin=true`) |
| `PlatformAdminHandler` | `GET /admin/providers` | 复用现有 `handler_providers.go` 的 `ProviderStatus`(全局,无需按租户) |
| `PlatformAdminHandler` | `/admin/virtual-models`、`/admin/failover`、`/admin/guardrails`、`/admin/workflows`、`/admin/model-pricing-overrides`、`/admin/tagging`(平台默认视角) | 各 Service 现有的**未改名**方法(`List`/`Upsert`/`Delete` 等,隐式操作 `"default"` 租户——与拆分前行为完全一致) |
| `PlatformAdminHandler` | `GET /admin/auth-keys?tenant_id=`(跨租户或指定租户) | `authkeys.Service.List(ctx, req.TenantID)`(Task 9) |
| `TenantAdminHandler` | `/admin/auth-keys`(自动 scope 当前租户,创建时强制 `TenantID = core.GetTenantID(ctx)`、`IsTenantAdmin = false`) | `authkeys.Service.List/UpdateLabels/Deactivate(ctx, core.GetTenantID(ctx), ...)`、`Create` |
| `TenantAdminHandler` | `/admin/usage`、`/admin/audit` | 复用现有 `handler_usage.go`/`handler_audit.go`(已经按 `core.GetTenantID(ctx)` 读取,直接搬方法即可) |
| `TenantAdminHandler` | `/admin/budgets` | 复用现有 `handler_budgets.go`(`budget.Service` 已经按 ctx 派生 tenantID,直接搬方法即可) |
| `TenantAdminHandler` | `/admin/virtual-models`、`/admin/failover`、`/admin/guardrails`、`/admin/workflows`、`/admin/model-pricing-overrides`、`/admin/tagging`(本租户覆盖) | 各 Service Task 3-8 新增的 `ForTenant` 方法,传 `core.GetTenantID(ctx)` |

- [ ] **Step 1: 写 `PlatformAdminHandler` 的失败测试**

创建 `internal/admin/platform_handler_test.go`:

```go
package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/tenants"
)

func newTestTenantsService(t *testing.T) *tenants.Service {
	t.Helper()
	store, err := tenants.NewSQLiteStore(newMemoryDB(t)) // 执行前确认 internal/admin 测试里是否已有 newMemoryDB;若没有,按 internal/tenants/store_sqlite_test.go 的模式本地构造
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return tenants.NewService(store, 0, "app")
}

func TestPlatformAdminHandler_CreateTenant(t *testing.T) {
	h := &PlatformAdminHandler{Tenants: newTestTenantsService(t)}
	body := `{"subdomain":"xyz","name":"XYZ Inc","plan":"pro"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.CreateTenant(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)
}

func TestPlatformAdminHandler_CreateTenant_RejectsReservedSubdomain(t *testing.T) {
	h := &PlatformAdminHandler{Tenants: newTestTenantsService(t)}
	body := `{"subdomain":"default","name":"Nope"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.CreateTenant(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPlatformAdminHandler_DeleteTenant_SoftDisables(t *testing.T) {
	svc := newTestTenantsService(t)
	require.NoError(t, svc.Create(context.Background(), tenants.Tenant{ID: "t-1", Subdomain: "xyz", Name: "XYZ", Status: tenants.StatusActive}))
	h := &PlatformAdminHandler{Tenants: svc}

	req := httptest.NewRequest(http.MethodDelete, "/admin/tenants/t-1", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("t-1")

	err := h.DeleteTenant(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, rec.Code)

	got, err := svc.GetByID(context.Background(), "t-1")
	require.NoError(t, err)
	require.True(t, got.IsDisabled())
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/admin/... -run TestPlatformAdminHandler -v`
Expected: 编译失败(`PlatformAdminHandler` 未定义)。

- [ ] **Step 3: 实现 `internal/admin/platform_handler.go`(租户 CRUD 部分)**

```go
package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/tenants"
)

// PlatformAdminHandler serves /admin/* on the platform host (app.<base_domain>).
// It manages tenants and platform-default configuration. Every dependency
// is optional and nil-checked, matching the existing admin.Handler pattern.
type PlatformAdminHandler struct {
	Tenants  *tenants.Service
	AuthKeys *authkeys.Service
	Default  *Handler // delegates platform-default config/provider endpoints unchanged
}

type createTenantRequest struct {
	Subdomain string `json:"subdomain"`
	Name      string `json:"name"`
	Plan      string `json:"plan,omitempty"`
}

type tenantResponse struct {
	ID        string    `json:"id"`
	Subdomain string    `json:"subdomain"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Plan      string    `json:"plan"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func tenantToResponse(t tenants.Tenant) tenantResponse {
	return tenantResponse{ID: t.ID, Subdomain: t.Subdomain, Name: t.Name, Status: string(t.Status), Plan: t.Plan, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func (h *PlatformAdminHandler) CreateTenant(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	var req createTenantRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	req.Subdomain = strings.ToLower(strings.TrimSpace(req.Subdomain))
	if req.Subdomain == "" || req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": "subdomain and name are required"}})
	}
	now := time.Now().UTC()
	t := tenants.Tenant{ID: newTenantID(), Subdomain: req.Subdomain, Name: req.Name, Plan: req.Plan, Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}
	if err := h.Tenants.Create(c.Request().Context(), t); err != nil {
		if tenants.IsReservedSubdomain(err) || tenants.IsSubdomainTaken(err) {
			return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_subdomain", "message": err.Error()}})
		}
		return handleError(c, "create tenant", err)
	}
	return c.JSON(http.StatusCreated, tenantToResponse(t))
}

func (h *PlatformAdminHandler) ListTenants(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	list, err := h.Tenants.List(c.Request().Context())
	if err != nil {
		return handleError(c, "list tenants", err)
	}
	out := make([]tenantResponse, 0, len(list))
	for _, t := range list {
		out = append(out, tenantToResponse(t))
	}
	return c.JSON(http.StatusOK, map[string]any{"tenants": out})
}

func (h *PlatformAdminHandler) GetTenant(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	t, err := h.Tenants.GetByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		if tenants.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "tenant_not_found"}})
		}
		return handleError(c, "get tenant", err)
	}
	return c.JSON(http.StatusOK, tenantToResponse(t))
}

type updateTenantRequest struct {
	Name string `json:"name"`
	Plan string `json:"plan"`
}

func (h *PlatformAdminHandler) UpdateTenant(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	var req updateTenantRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	if err := h.Tenants.Update(c.Request().Context(), c.Param("id"), req.Name, req.Plan); err != nil {
		if tenants.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "tenant_not_found"}})
		}
		return handleError(c, "update tenant", err)
	}
	got, err := h.Tenants.GetByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return handleError(c, "get tenant", err)
	}
	return c.JSON(http.StatusOK, tenantToResponse(got))
}

// DeleteTenant soft-deletes a tenant by disabling it (no cascading data
// deletion — see Global Constraints).
func (h *PlatformAdminHandler) DeleteTenant(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	if err := h.Tenants.UpdateStatus(c.Request().Context(), c.Param("id"), tenants.StatusDisabled, time.Now().UTC()); err != nil {
		if tenants.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "tenant_not_found"}})
		}
		return handleError(c, "delete tenant", err)
	}
	return c.NoContent(http.StatusNoContent)
}
```

`newTenantID()` 用 `uuid.NewString()`(执行前 `grep -n "uuid.NewString" internal/authkeys/service.go` 确认项目已用的 uuid 包并保持一致,如 `github.com/google/uuid`),在文件顶部加对应 import 与:

```go
func newTenantID() string { return uuid.NewString() }
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/admin/... -run TestPlatformAdminHandler -v`
Expected: PASS。

- [ ] **Step 5: 实现签发/重置租户管理员 key 端点**

在 `platform_handler_test.go` 追加:

```go
func TestPlatformAdminHandler_IssueTenantAdminKey(t *testing.T) {
	tenantsSvc := newTestTenantsService(t)
	require.NoError(t, tenantsSvc.Create(context.Background(), tenants.Tenant{ID: "t-1", Subdomain: "xyz", Name: "XYZ", Status: tenants.StatusActive}))
	authSvc := newTestAuthKeysService(t) // 执行前 grep 确认 internal/admin 测试里现有的 authkeys 测试 service 构造 helper
	h := &PlatformAdminHandler{Tenants: tenantsSvc, AuthKeys: authSvc}

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants/t-1/admin-keys", strings.NewReader(`{"name":"xyz admin"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("t-1")

	err := h.IssueTenantAdminKey(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)
}
```

实现:

```go
type issueTenantAdminKeyRequest struct {
	Name string `json:"name"`
}

// IssueTenantAdminKey creates a tenant-admin auth key for the tenant
// identified by the :id path param. Only the platform admin (master key on
// the platform host) can call this — it is the only way a tenant admin
// credential is minted.
func (h *PlatformAdminHandler) IssueTenantAdminKey(c *echo.Context) error {
	if h.AuthKeys == nil || h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	tenantID := c.Param("id")
	if _, err := h.Tenants.GetByID(c.Request().Context(), tenantID); err != nil {
		if tenants.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "tenant_not_found"}})
		}
		return handleError(c, "get tenant", err)
	}
	var req issueTenantAdminKeyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	issued, err := h.AuthKeys.Create(c.Request().Context(), authkeys.CreateInput{
		Name:          req.Name,
		TenantID:      tenantID,
		IsTenantAdmin: true,
	})
	if err != nil {
		return handleError(c, "issue tenant admin key", err)
	}
	return c.JSON(http.StatusCreated, issued)
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/admin/... -run "TestPlatformAdminHandler" -v`
Expected: 全部 PASS。

- [ ] **Step 7: 实现 `internal/admin/platform_routes.go`**

```go
package admin

func (h *PlatformAdminHandler) RegisterRoutes(g RouteRegistrar) {
	g.POST("/tenants", h.CreateTenant)
	g.GET("/tenants", h.ListTenants)
	g.GET("/tenants/:id", h.GetTenant)
	g.PATCH("/tenants/:id", h.UpdateTenant)
	g.DELETE("/tenants/:id", h.DeleteTenant)
	g.POST("/tenants/:id/admin-keys", h.IssueTenantAdminKey)
	if h.AuthKeys != nil {
		g.GET("/auth-keys", h.ListAuthKeysAcrossTenants)
	}
	if h.Default != nil {
		h.Default.RegisterRoutes(g) // 平台默认配置端点(virtual-models/failover/guardrails/workflows/pricing/tagging/providers)复用现有实现,隐式操作 "default" 租户
	}
}
```

`ListAuthKeysAcrossTenants` 新增在 `platform_handler.go`:

```go
func (h *PlatformAdminHandler) ListAuthKeysAcrossTenants(c *echo.Context) error {
	tenantID := c.QueryParam("tenant_id") // 空 = 不限定,跨所有租户
	list, err := h.AuthKeys.List(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, "list auth keys", err)
	}
	return c.JSON(http.StatusOK, map[string]any{"auth_keys": list})
}
```

**注意:** `g.PATCH` 需要 `RouteRegistrar` 接口新增 `PATCH` 方法(现有接口只有 `GET/POST/PUT/DELETE`,`routes.go:9-14`)。在 `internal/admin/routes.go` 的 `RouteRegistrar` 接口加:

```go
	PATCH(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
```

`echo.Group`/`echo.Echo` 本身已经有 `PATCH` 方法(标准 Echo v5 API),接口加上之后无需其它改动即可满足。

- [ ] **Step 8: 运行全量 admin 包测试确认无回归 + 提交**

Run: `go test ./internal/admin/... -v && go build ./...`

```bash
git add internal/admin/platform_handler.go internal/admin/platform_routes.go internal/admin/platform_handler_test.go internal/admin/routes.go
git commit -m "feat(admin): add PlatformAdminHandler with tenant CRUD and admin-key issuance"
```

- [ ] **Step 9: 写 `TenantAdminHandler` 的失败测试**

创建 `internal/admin/tenant_handler_test.go`:

```go
package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
)

func TestTenantAdminHandler_CreateAuthKey_AutoScopesToCurrentTenant(t *testing.T) {
	authSvc := newTestAuthKeysService(t)
	h := &TenantAdminHandler{AuthKeys: authSvc}

	// 请求体尝试冒充别的租户 / 提权为 tenant admin —— 都应被忽略
	body := `{"name":"escalate","tenant_id":"someone-else","is_tenant_admin":true}`
	req := httptest.NewRequest(http.MethodPost, "/admin/auth-keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.CreateAuthKey(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)

	list, err := authSvc.List(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.False(t, list[0].IsTenantAdmin, "tenant admin must not be able to self-escalate")
}

func TestTenantAdminHandler_ListAuthKeys_ScopedToCurrentTenant(t *testing.T) {
	authSvc := newTestAuthKeysService(t)
	_, err := authSvc.Create(context.Background(), authkeys.CreateInput{Name: "a", TenantID: "tenant-a"})
	require.NoError(t, err)
	_, err = authSvc.Create(context.Background(), authkeys.CreateInput{Name: "b", TenantID: "tenant-b"})
	require.NoError(t, err)

	h := &TenantAdminHandler{AuthKeys: authSvc}
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err = h.ListAuthKeys(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"a"`)
	require.NotContains(t, rec.Body.String(), `"b"`)
}
```

`newTestAuthKeysService` 执行前 `grep -n "func newTestAuthKeysService\|func newTestService" internal/authkeys/service_test.go internal/admin/*_test.go` 确认现有 helper 或按现有模式在 `internal/admin` 测试文件内构造(真实 SQLite store + `authkeys.NewService`)。

- [ ] **Step 10: 运行测试确认失败**

Run: `go test ./internal/admin/... -run TestTenantAdminHandler -v`
Expected: 编译失败(`TenantAdminHandler` 未定义)。

- [ ] **Step 11: 实现 `internal/admin/tenant_handler.go`**

```go
package admin

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
)

// TenantAdminHandler serves /admin/* on a tenant's own host
// (<subdomain>.<base_domain>). Every method auto-scopes to
// core.GetTenantID(c.Request().Context()) — it never accepts a tenant_id
// from the caller for its own tenant's resources.
type TenantAdminHandler struct {
	AuthKeys *authkeys.Service
	Config   *Handler // delegates usage/audit/budgets endpoints unchanged (already ctx-scoped)
}

type createTenantAuthKeyRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	UserPath    string   `json:"user_path,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// CreateAuthKey issues a regular API key scoped to the current tenant.
// tenant_id and is_tenant_admin from the request body (if any) are
// ignored — a tenant admin can never mint another admin key or escalate.
func (h *TenantAdminHandler) CreateAuthKey(c *echo.Context) error {
	if h.AuthKeys == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	var req createTenantAuthKeyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	issued, err := h.AuthKeys.Create(c.Request().Context(), authkeys.CreateInput{
		Name:        req.Name,
		Description: req.Description,
		UserPath:    req.UserPath,
		Labels:      req.Labels,
		TenantID:    tenantID,
		// IsTenantAdmin intentionally omitted (defaults to false) — only
		// PlatformAdminHandler.IssueTenantAdminKey can set it.
	})
	if err != nil {
		return handleError(c, "create auth key", err)
	}
	return c.JSON(http.StatusCreated, issued)
}

func (h *TenantAdminHandler) ListAuthKeys(c *echo.Context) error {
	if h.AuthKeys == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	list, err := h.AuthKeys.List(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, "list auth keys", err)
	}
	return c.JSON(http.StatusOK, map[string]any{"auth_keys": list})
}

type updateAuthKeyLabelsRequest struct {
	Labels []string `json:"labels"`
}

func (h *TenantAdminHandler) UpdateAuthKeyLabels(c *echo.Context) error {
	if h.AuthKeys == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	var req updateAuthKeyLabelsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	updated, err := h.AuthKeys.UpdateLabels(c.Request().Context(), tenantID, c.Param("id"), req.Labels)
	if err != nil {
		return handleError(c, "update auth key labels", err)
	}
	return c.JSON(http.StatusOK, updated)
}

func (h *TenantAdminHandler) DeactivateAuthKey(c *echo.Context) error {
	if h.AuthKeys == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	if err := h.AuthKeys.Deactivate(c.Request().Context(), tenantID, c.Param("id")); err != nil {
		return handleError(c, "deactivate auth key", err)
	}
	return c.NoContent(http.StatusNoContent)
}
```

**执行前** `grep -n "func (h \*Handler) CreateAuthKey\|ListAuthKeys\|UpdateAuthKeyLabels\|DeactivateAuthKey" internal/admin/handler_authkeys.go` 核对现有 `*Handler` 同名方法的请求体结构体字段与响应格式,尽量保持字段名与响应形状一致,减少客户端(dashboard/API 调用方)感知到的破坏性变化。

- [ ] **Step 12: 实现 `internal/admin/tenant_routes.go`**

```go
package admin

func (h *TenantAdminHandler) RegisterRoutes(g RouteRegistrar) {
	g.POST("/auth-keys", h.CreateAuthKey)
	g.GET("/auth-keys", h.ListAuthKeys)
	g.PUT("/auth-keys/:id/labels", h.UpdateAuthKeyLabels)
	g.DELETE("/auth-keys/:id", h.DeactivateAuthKey)
	if h.Config != nil {
		h.Config.RegisterRoutes(g) // usage/audit/budgets 端点复用现有实现(已按 ctx tenantID 读取)
	}
}
```

- [ ] **Step 13: 运行测试确认通过**

Run: `go test ./internal/admin/... -run TestTenantAdminHandler -v`
Expected: PASS。

- [ ] **Step 14: 补齐 virtual-models/failover/guardrails/workflows/pricing/tagging 的 `TenantAdminHandler` 方法**

参考 `internal/admin/handler_virtualmodels.go` 现有的 `ListVirtualModels`/`UpsertVirtualModel`/`DeleteVirtualModel` 请求体/响应结构(执行前 `grep -n "func (h \*Handler)" internal/admin/handler_virtualmodels.go internal/admin/handler_failover.go internal/admin/handler_guardrails.go internal/admin/handler_workflows.go internal/admin/handler_model_pricing_overrides.go internal/admin/handler_tagging.go` 逐个核对),在 `internal/admin/tenant_handler.go` 追加对应方法,每个方法体只有两处不同于平台版本:(a) 调用 Task 3-8 新增的 `XxxForTenant(ctx, tenantID, ...)` 而不是 `Xxx(ctx, ...)`;(b) `tenantID := core.GetTenantID(c.Request().Context())`。例如:

```go
func (h *TenantAdminHandler) ListVirtualModels(c *echo.Context) error {
	if h.VirtualModels == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "virtual_models_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	rows, err := h.VirtualModels.ListEffectiveForTenant(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, "list virtual models", err)
	}
	return c.JSON(http.StatusOK, map[string]any{"virtual_models": rows})
}
```

`TenantAdminHandler` struct 相应加 `VirtualModels *virtualmodels.Service`、`FailoverRules *failover.Service`、`Guardrails *guardrails.Service`、`Workflows *workflows.Service`、`PricingOverrides *pricingoverrides.Service`、`Tagging *tagging.Service` 字段(与 `internal/admin/handler.go` 现有 `Handler` 结构体的对应字段同名同类型,便于装配时直接复用同一批 Service 实例)。按同样模式为每个资源写 `List`/`Upsert`/`Delete` 三个方法(照抄现有 `Handler` 对应方法的请求体解析/错误处理逻辑,只替换底层调用为 `ForTenant` 版本 + 传 `tenantID`),并在 `tenant_routes.go` 里为每个资源加对应路由(路径与现有 `internal/admin/routes.go` 中 `RegisterRoutes` 的路径保持一致,执行前 `grep -n "g\.\(GET\|POST\|PUT\|DELETE\)" internal/admin/routes.go` 抄路径)。

- [ ] **Step 15: 运行全量 admin 包测试 + 全量构建 + 提交**

Run: `go test ./internal/admin/... -v && go build ./...`

```bash
git add internal/admin/tenant_handler.go internal/admin/tenant_routes.go internal/admin/tenant_handler_test.go
git commit -m "feat(admin): add TenantAdminHandler auto-scoped to the current tenant"
```

---

### Task 11: 路由挂载 + 装配 + P2 遗留清理

**Files:**
- Modify: `internal/server/http.go`(`Config` 加字段、按 Host 分流挂载、删除死代码、`isAdminPath` 修复)
- Modify: `internal/app/app.go`(构造 `PlatformAdminHandler`/`TenantAdminHandler`,替换现有单一 `AdminHandler` 装配)
- Modify: `internal/authkeys/types.go`(`IsTenantAdmin` JSON tag 去掉 `omitempty`)
- Modify: `internal/server/auth.go`(重命名误导性测试名——若在本包测试文件里)
- Test: `internal/server/http_test.go`、`internal/app/app_test.go`(若存在)

**Interfaces:**
- Consumes: Task 2 的 `hostGuard`,Task 10 的 `PlatformAdminHandler`/`TenantAdminHandler`。
- Produces: `server.Config` 的 `AdminHandler *admin.Handler` 字段替换为 `PlatformAdminHandler *admin.PlatformAdminHandler`、`TenantAdminHandler *admin.TenantAdminHandler`。

- [ ] **Step 1: 写路由分流的失败测试**

在 `internal/server/http_test.go`(或新建 `internal/server/admin_routing_test.go`)追加:

```go
func TestAdminRouting_PlatformHost_TenantRoutesNotFound(t *testing.T) {
	cfg := &Config{
		AdminEndpointsEnabled: true,
		PlatformAdminHandler:  &admin.PlatformAdminHandler{},
		TenantAdminHandler:    &admin.TenantAdminHandler{},
	}
	e := New(cfg) // 执行前 grep 确认 internal/server 包对外暴露的构造入口函数名(可能是 New / NewEcho / BuildEcho)

	req := httptest.NewRequest(http.MethodPost, "/admin/auth-keys", nil)
	req = req.WithContext(core.WithPlatformHost(req.Context(), true))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "tenant-only route (auth-keys create) must 404 on platform host")
}

func TestAdminRouting_TenantHost_PlatformRoutesNotFound(t *testing.T) {
	cfg := &Config{
		AdminEndpointsEnabled: true,
		PlatformAdminHandler:  &admin.PlatformAdminHandler{},
		TenantAdminHandler:    &admin.TenantAdminHandler{},
	}
	e := New(cfg)

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "platform-only route (tenant create) must 404 on tenant host")
}
```

执行前 `grep -n "^func New\b" internal/server/http.go` 确认真实构造函数签名(可能不叫 `New`,也可能不接受 `*Config` 而是别的形式),按实际签名调整测试。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/... -run TestAdminRouting -v`
Expected: 编译失败(`Config.PlatformAdminHandler`/`TenantAdminHandler` 字段不存在)。

- [ ] **Step 3: 修改 `internal/server/http.go` 的 `Config` 与挂载逻辑**

把 `Config` 里的 `AdminHandler *admin.Handler` 字段替换为:

```go
	// PlatformAdminHandler serves /admin/* on the platform host.
	PlatformAdminHandler *admin.PlatformAdminHandler
	// TenantAdminHandler serves /admin/* on each tenant's own host.
	TenantAdminHandler *admin.TenantAdminHandler
```

把现有挂载逻辑(`http.go:402-421` 一带)改成:

```go
	if cfg != nil && cfg.AdminEndpointsEnabled {
		if cfg.PlatformAdminHandler != nil {
			platformGroup := e.Group("/admin", hostGuard("platform"))
			cfg.PlatformAdminHandler.RegisterRoutes(platformGroup)
		}
		if cfg.TenantAdminHandler != nil {
			tenantGroup := e.Group("/admin", hostGuard("tenant"))
			cfg.TenantAdminHandler.RegisterRoutes(tenantGroup)
		}
		if cfg.PlatformAdminHandler != nil {
			legacy := e.Group("/admin/api/v1", adminLegacyDeprecationMiddleware, hostGuard("platform"))
			cfg.PlatformAdminHandler.RegisterRoutes(legacy)
		}
	}
```

（legacy `/admin/api/v1` 路径按现有行为只挂平台分组即可——它是废弃路径,不必为它单独维护租户版本;若现有测试依赖 legacy 路径在租户 host 也可用,执行前 `grep -rn "admin/api/v1" internal/server/*_test.go` 排查并按需调整。）

保留 `DashboardHandler` 挂载逻辑不变(`http.go:412-417` 一带的 `/admin/dashboard`/`/admin/static/*`)——Dashboard 不在 P4 范围内,继续对两种 host 都开放,与拆分前行为一致。

- [ ] **Step 4: 修复 `isAdminPath` 不匹配裸 `/admin`(P2 遗留项)**

在 `internal/server/auth.go`,把:

```go
func isAdminPath(path string) bool {
	return strings.HasPrefix(path, "/admin/")
}
```

改成:

```go
func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}
```

Run: `go test ./internal/server/... -run TestAuthMiddleware -v`(P2 已有的测试)确认无回归。

- [ ] **Step 5: 删除 `http.go` 里注释掉的死代码(P2 遗留项)**

Run: `grep -n "MasterKey == \"\"" internal/server/http.go`,找到 P2 完成记录里提到的注释块(`// if cfg != nil && cfg.MasterKey == "" ...`),整段物理删除(不再只是注释)。

- [ ] **Step 6: `IsTenantAdmin` JSON tag 去掉 `omitempty`(P2 遗留项)**

在 `internal/authkeys/types.go`,把:

```go
	IsTenantAdmin bool `json:"is_tenant_admin,omitempty" bson:"is_tenant_admin,omitempty"`
```

改成:

```go
	IsTenantAdmin bool `json:"is_tenant_admin" bson:"is_tenant_admin,omitempty"`
```

(只去掉 JSON 的 `omitempty`,BSON 保持不变——P2 完成记录明确说 BSON 已经不带 `omitempty` 了,这里核实一下,若已经没有则跳过。)

- [ ] **Step 7: 修改 `internal/app/app.go` 装配逻辑**

打开 `internal/app/app.go`,找到现有单一 `admin.NewHandler(...)` 调用(约 988-1006 行)与其结果赋值给 `serverCfg.AdminHandler`(约 526-527 行)。改成:

```go
	adminDefault, dashHandler, err := initAdmin(ctx, appCfg, ...) // 参数与现有 initAdmin 调用保持不变
	if err != nil {
		return nil, err // 保留现有 unwind 逻辑
	}
	platformHandler := &admin.PlatformAdminHandler{
		Tenants:  tenantSvc,
		AuthKeys: authKeysSvc, // 沿用现有变量名,执行前 grep 确认
		Default:  adminDefault,
	}
	tenantHandler := &admin.TenantAdminHandler{
		AuthKeys:         authKeysSvc,
		Config:           adminDefault,
		VirtualModels:    virtualModelsSvc,
		FailoverRules:    failoverSvc,
		Guardrails:       guardrailsSvc,
		Workflows:        workflowsSvc,
		PricingOverrides: pricingOverridesSvc,
		Tagging:          taggingSvc,
	}
	serverCfg.AdminEndpointsEnabled = true
	serverCfg.PlatformAdminHandler = platformHandler
	serverCfg.TenantAdminHandler = tenantHandler
```

执行前 `grep -n "virtualModelsSvc\|failoverSvc\|guardrailsSvc\|workflowsSvc\|pricingOverridesSvc\|taggingSvc\|authKeysSvc\|tenantSvc" internal/app/app.go` 确认这些 Service 实例在 `app.New` 里的实际变量名(本计划全篇假设的变量名仅为占位,必须以 grep 结果为准替换)。同时把 P1 完成记录里提到的"`tenantSvc` 未存到 `App` struct"这一遗留项一并处理:若 `App` struct 目前没有保存 `tenants.Service` 的字段,加一个(`tenants *tenants.Service`),赋值处补上,供后续阶段复用。

- [ ] **Step 8: 运行全量测试确认通过**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS。

- [ ] **Step 9: 提交**

```bash
git add internal/server/http.go internal/server/auth.go internal/authkeys/types.go internal/app/app.go
git commit -m "feat(server): mount PlatformAdminHandler/TenantAdminHandler by host; fix isAdminPath and remove dead skipPaths code"
```

---

### Task 12: 端到端集成测试

**Files:**
- Create: `internal/server/admin_split_integration_test.go`
- (无生产代码改动)

**目标(对照设计 §7、§6.3):** 完整 Echo + 真实 `tenants.Service` + 真实 `authkeys.Service` + 真实 `virtualmodels.Service` 下验证:

1. 平台管理员(master key,平台 host)创建租户 A、租户 B,各自签发租户管理员 key。
2. 租户 A 管理员在 `a.smart-router.com` 创建一条 virtual-model 覆盖;租户 B 管理员在 `b.smart-router.com` 的 `GET /admin/virtual-models` 看不到租户 A 的覆盖(反之亦然)。
3. 租户 A 管理员在 `a.smart-router.com` 创建一个 API key(自动 scope 到租户 A,即使请求体尝试传别的 `tenant_id`/`is_tenant_admin=true` 也不生效)。
4. 租户 A 的 API key 在 `b.smart-router.com` 访问 `/v1/*` → 401(P2 已有行为,这里做端到端复核)。
5. 平台管理员 key 在租户 host 访问 `/admin/tenants` → 404(`hostGuard("platform")` 生效,不是 401/403——路由级隔离与认证级隔离的区别在这里体现)。
6. 租户管理员 key 在平台 host 访问 `/admin/auth-keys`(租户端点)→ 404(`hostGuard("tenant")` 生效)。

- [ ] **Step 1: 写测试**

创建 `internal/server/admin_split_integration_test.go`,按上述 6 点各写一个断言,复用 Task 2/6(P2 的 `auth_tenant_integration_test.go`)已经验证过的 `doAuthReq`/`newTestAuthKeysSQLiteStore`/`newMemoryDB` helper 模式(执行前 `grep -n "func doAuthReq\|func newMemoryDB\|func newTestAuthKeysSQLiteStore" internal/server/*_test.go` 确认可直接复用,不重复定义)。测试装配需要同时叠加 `TenantResolver` → `AuthMiddleware` → `hostGuard` → 目标 Handler 的路由,与 `internal/server/http.go` Task 11 实现的真实链路一致(不要重新发明一套装配顺序)。

- [ ] **Step 2: 运行测试**

Run: `go test ./internal/server/... -run TestAdminSplit -v`
Expected: PASS。若失败,根据具体断言调试(常见问题:`hostGuard` 与 `enforceTenantAndRole` 的先后顺序、`RouteRegistrar` 分组是否正确应用了 `hostGuard` 中间件)。

- [ ] **Step 3: 跑全量测试 + 提交**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/server/admin_split_integration_test.go
git commit -m "test(server): add end-to-end platform/tenant admin split integration test"
```

---

### Task 13: 验证 + Completion Notes + 更新 ROADMAP

**Files:**
- 无生产代码改动。
- Modify: `docs/superpowers/ROADMAP.md`(把 P4 行描述里的"dashboard role-aware"移到 P7,消除与本计划 Global Constraints 的歧义)、本计划文件(追加 Completion Notes)。

- [ ] **Step 1: 全量构建**

Run: `go build ./...`
Expected: PASS,无遗留调用点还在用旧签名。

- [ ] **Step 2: 全量测试**

Run: `go test ./...`
Expected: 全部 PASS(PG/Mongo 相关测试默认 skip,不计入失败)。

- [ ] **Step 3: `go vet ./...`**

Expected: 干净(或只有 P3 完成记录里提到的既有 3 个 core/ 包 dup json tag 警告,无新增)。

- [ ] **Step 4: Dashboard JS 测试(确认未受影响)**

Run: `node --test internal/admin/dashboard/static/js/modules/*.test.cjs`
Expected: PASS(P4 未改动 dashboard,这一步纯粹确认没有意外触碰)。

- [ ] **Step 5: 更新 `ROADMAP.md`**

把 P4 行的"核心内容"列从:

```
PlatformAdminHandler vs TenantAdminHandler、路由按 Host 分流、dashboard role-aware
```

改成:

```
PlatformAdminHandler vs TenantAdminHandler、路由按 Host 分流(hostGuard)、tenant CRUD、六个配置类 Service 的管理面 tenantID 补全
```

把 P7 行的"核心内容"列(若尚未提及 dashboard)加上"dashboard role-aware UI"。

- [ ] **Step 6: 追加 Completion Notes 到本计划文件末尾**

写入实际的 commit range、验证结果、已知的 deferred 项(至少应包含:①推理热路径 6 个 Service 的租户可见性仍是 P5 的工作,②`internal/admin/handler.go`/`routes.go` 是否物理删除或保留作为两个新 Handler 内部共用的"平台默认视角"底座——按 Task 10 Step 7/12 的实际接线方式如实记录,③`tenants.Store` 的 subdomain 格式校验(长度/字符集)P1 遗留项是否顺带处理还是继续推迟),并给出面向 P5-P7 的分类建议表(格式参照 P1/P2/P3 Completion Notes)。

- [ ] **Step 7: 提交**

```bash
git add docs/superpowers/ROADMAP.md docs/superpowers/plans/2026-08-03-saas-multi-tenant-p4.md
git commit -m "docs(plan): append P4 completion notes; clarify P4/P7 dashboard boundary in ROADMAP"
```

---

## Self-Review

**1. Spec coverage(对照设计文档 §6.1、§7.1、§7.2、§3.1 补全项):**
- §6.1 `hostGuard(kind)` 中间件 → Task 2 ✓
- §7.1 `PlatformAdminHandler`:租户 CRUD、签发租户管理员 key、全局 provider 只读(复用现有)、平台默认配置(复用现有 `Handler`)→ Task 1 + Task 10 ✓
- §7.2 `TenantAdminHandler`:本租户 auth-key 自动 scope、本租户 usage/audit/budget(复用现有已 ctx-scoped 实现)、本租户配置覆盖 → Task 9 + Task 10 ✓
- §6.2/§6.3 认证矩阵 → P2 已完成,P4 只修 `isAdminPath` 缺陷(Task 11)不改规则本身
- §7.3 Dashboard role-aware → **明确推迟到 P7**,已与用户确认,记入 Global Constraints 与 ROADMAP 更新(Task 13)

**P4 明确排除(归 P5-P7):** 推理热路径的六个 Service 缓存改造为按租户(map[tenantID]snapshot)+ 4 个接口签名改动(`FailoverResolver`/`WorkflowPolicyResolver`/`PricingResolver`/tagging 中间件方法)(P5,"provider 按租户可见性");配额中间件(P5);内存 store DB 化的收尾(P6,P3 已完成大部分,`conversationstore` SQL 后端仍缺,不在 P4 触碰范围);Dashboard 角色感知 UI(P7)。均已在 Global Constraints 与各任务说明中声明。

**2. Placeholder scan:** 无 TBD/TODO。多处"执行前 grep 确认现有签名/helper 名"是必要的探查指令(与 P1-P3 计划的既有风格一致),不是占位符——实现者必须执行该 grep 并按实际结果调整,编译器会兜底暴露不一致。Task 5(guardrails)、Task 6(workflows)、Task 7(pricingoverrides)里对"如果现有 Upsert/Delete 实现细节和预期不同,如何降级处理"的说明是给implementer 的决策依据,不是含糊指令。

**3. Type consistency:**
- 六个 `ForTenant` 系列方法(Task 3-8)命名规则统一:`List(EffectiveFor)?Tenant(ctx, tenantID)`、`GetForTenant(ctx, tenantID, key)`、`UpsertForTenant(ctx, tenantID, row)`、`DeleteForTenant(ctx, tenantID, key)`,签名跨任务一致。
- `tenants.Service.Update(ctx, id, name, plan string) error` 在 Task 1 定义,Task 10 `PlatformAdminHandler.UpdateTenant` 调用——一致。
- `authkeys.Service.List/UpdateLabels/Deactivate` 的新 `tenantID` 参数位置(`ctx` 之后)在 Task 9 定义,Task 10 `PlatformAdminHandler.ListAuthKeysAcrossTenants`/`TenantAdminHandler` 三个方法调用——一致。
- `PlatformAdminHandler`/`TenantAdminHandler` 结构体字段名在 Task 10 定义、Task 11 `app.go` 装配处引用——一致(装配时的具体 Service 变量名以 Task 11 Step 7 的 grep 结果为准)。

**4. 已知风险与执行注意事项:**
- Task 3-8 六个"抽取现有 Upsert/Create 规范化逻辑到私有 helper"的步骤都要求先跑一次全量包测试确认"纯重构"无回归,再继续写新代码——这是防止在保留字段被误删的关键防线,实现者不得跳过。
- Task 10 的路由分工表格是本计划最容易出现"看似合理实则遗漏字段/路径"的地方——Step 14 明确要求逐个 `grep` 现有 `handler_*.go` 的请求体结构核对,而不是凭空重新设计字段名,降低客户端破坏性变更的风险。
- Task 11 Step 7 的 `app.go` 装配代码大量使用"执行前 grep 确认变量名"的占位注释——这是因为 `app.New` 内部变量命名未在本次计划撰写过程中逐一核实到 100% 精确(与 P1 Self-Review 对 `app.go` 装配点的处理方式一致),实现者必须按实际代码调整,编译失败会立即暴露不一致。
- `internal/admin/handler.go`/`routes.go`(拆分前的单一 `Handler`)在本计划里**继续作为两个新 Handler 内部复用的"平台默认视角"/"usage-audit-budget 视角"底座**,不做删除——这是刻意的最小改动选择(见 Task 10 Files 说明),避免在一个已经很大的计划里再引入"物理删除大量现有代码"的额外风险;是否在后续阶段彻底移除该文件,留给 P5+ 视实际情况决定。
