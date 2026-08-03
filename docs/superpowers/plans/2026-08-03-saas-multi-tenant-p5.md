# SmartRouter SaaS 多租户改造 — P5: 推理热路径租户可见性 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 6 个配置类 Service 的推理热路径缓存从单租户(`atomic.Value` / `RWMutex` 持有单一 `snapshot`)改为按租户缓存(`map[tenantID]*snapshot`)、给 4 个无 `ctx` 参数的推理接口加 `context.Context`、把推理 `/v1/*` 路由挂上 `hostGuard("tenant")`(租户 host 独占)、修复推理路径 `enforceTenantAndRole` 的 `ctxTenantID==""` 同款漏洞。

**Architecture:** P4 不给推理热路径加租户意识,只给 6 个 Service 各补了一组 `XxxForTenant` 管理面方法做 admin 隔离,推理热路径继续用进程级单租户缓存。P5 的改动集中在推理热路径——把 `snapshot` 从单值改为 `map[string]*snapshot`,Refesh 改为按租户分片更新,热路径方法通过 `context.Context` 拿 `core.GetTenantID(ctx)` 选正确的 snapshot。provider 按租户可见性通过 `virtualmodels.Service` 的 per-tenant policy snapshot 自动实现——`FilterPublicModels(ctx, ...)` 已经有了 ctx,只需底层 snapshot 变成按租户即可。配额中间件为全新实现,利用 `core.GetTenantID(ctx)` 与 `budget.Service`(已有 userPath 维度)组合出租户级限额。改动遵循 P1-P7 增量模式:不重写已有逻辑,只在"读 snapshot → 选 snapshot"与"缺少 ctx → 补 ctx"两个切面改动;六个 Service 的改动模式完全相同,遵循统一命名约定。

**Tech Stack:** Go 1.x, Echo v5, `database/sql`(SQLite/`modernc.org/sqlite`)、`pgxpool`(Postgres)、Mongo driver v2、`testify`(`require`/`assert`)、`sync`/`atomic`。

## Global Constraints

- **直接在 `master` 上执行**(P1-P7 既定约定:不开 feature 分支/worktree,本地提交,不主动 push)。
- **决策 2026-08-03(与用户确认):** dev mode(未配置 base_domain)的推理路由也挂 `hostGuard("tenant")`,与 admin 平面的 P4 最终修复一致——推理 `/v1/*` 只在 tenant host 上开放,平台 host 不可达,dev mode 下 tenant host 解析不到租户 ID 也无影响(TenantResolver no-op,`GetTenantID` 返回 `""`,推理用租户 `default`)。
- **provider 按租户可见性不单独做任务:** 通过 `virtualmodels.Service` 的 per-tenant policy snapshot 自动实现——`FilterPublicModels(ctx, []core.Model)` 已经有 ctx,`ModelAuthorizer = virtualmodels.Service`,只需其底层 snapshot 为按租户即可。不增加新的 provider-level 过滤层。
- **配额中间件(租户级 budget check)为可选任务(Task 9):** P5 核心交付物是 6 Service 的按租户缓存+4 接口 ctx 参数变更+推理路由 host guard;配额中间件依赖这些基础(需要从 ctx 稳定拿到 tenantID),但逻辑独立——可推迟到 P6 不影响验证。
- **六个 ForTenant 方法的 P4 deferred 对齐(nil-store guard、错误包装、name trim 等)不进入 P5 主线——它们影响的是管理面(P4),不影响推理热路径。** 仅在 background refresh 迭代租户列表的实现中顺带处理,不做独立任务。其中 `GetForTenant` selector 字面字符串匹配的改进不在此范围(P4 管理面 admin handler 传给它的 selector 已经是 `NormalizeInput` 规范化的)。
- **building-then-breaking 原则:** 接口签名变更(add ctx)时,所有实现与调用点必须同一任务内更新——不允许中间 commit 编译不过。

## File Structure

| 文件 | 职责 |
|---|---|
| `internal/server/http.go` | 推理路由挂 `hostGuard("tenant")`(Task 1) |
| `internal/server/auth.go` | 推理路径 `enforceTenantAndRole` 修复(Task 1) |
| `internal/failover/service.go` | snapshot → `map[string]*ruleSnapshot`; Resolver.ResolveFailovers +ctx(Task 2) |
| `internal/failover/resolver.go` | `ResolveFailovers` 方法体从 ctx 拿 tenantID(Task 2) |
| `internal/gateway/failover.go` | `FailoverSelectors` 补 ctx 参数(Task 2) |
| `internal/gateway/inference_execute.go` | `tryFailoverResponse`/`tryFailoverStream` 传 ctx(Task 2) |
| `internal/gateway/interfaces.go` | `FailoverResolver` 接口 `ResolveFailovers` +ctx(Task 2) |
| `internal/workflows/service.go` | snapshot → `map[string]snapshot`; Match +ctx(Task 3) |
| `internal/gateway/interfaces.go` | `WorkflowPolicyResolver` 接口 `Match` +ctx(Task 3) |
| `internal/gateway/workflow_policy.go` | `ApplyWorkflowPolicy` 传 ctx 进 Match(Task 3) |
| `internal/server/model_validation.go` | `applyWorkflowPolicy` 传 ctx(Task 3) |
| `internal/gateway/batch_orchestrator.go` | batch 路径 `ApplyWorkflowPolicy` 传 ctx(Task 3) |
| `internal/pricingoverrides/service.go` | snapshot → `map[string]snapshot`(Task 4) |
| `internal/pricingoverrides/resolver.go` | `ResolvePricing` +ctx(Task 4) |
| `internal/usage/pricing.go` | `PricingResolver` 接口 `ResolvePricing` +ctx(Task 4) |
| `internal/gateway/usage.go` | `logUsage` 传 ctx 进 ResolvePricing(Task 4) |
| `internal/gateway/batch_usage.go` | `LogBatchUsageFromBatchResults` 补 ctx 或内部派生 context(Task 4) |
| `internal/usage/stream_observer.go` | SSE observer 补 ctx 或内部派生(Task 4) |
| `internal/usage/recalculate_pricing.go` | recalculate 补 ctx(Task 4) |
| `internal/responsecache/usage_hit.go` | cache-hit usage 传 ctx(Task 4) |
| `internal/server/audio_service.go` | audio usage 传 ctx(Task 4) |
| `internal/server/realtime_service.go` | realtime usage 传 ctx(Task 4) |
| `internal/providers/registry_metadata.go` | ModelRegistry.ResolvePricing +ctx(Task 4) |
| `internal/tagging/service.go` | snapshot → `map[string]snapshot`; ExtractLabels/StripHeaders +ctx(Task 5) |
| `internal/tagging/tagging.go` | 纯函数签名不动(Task 5) |
| `internal/server/tagging.go` | `TaggingCapture` 传 ctx(Task 5) |
| `internal/virtualmodels/service.go` | snapshot → `map[string]snapshot`; 无 ctx 的 hot-path 方法加 ctx(Task 6) |
| `internal/virtualmodels/resolve.go` | `ResolveModel`/`Supports` 等加 ctx(Task 6) |
| `internal/server/handlers.go` | ListModels 等已经传 ctx,保持不变(Task 6) |
| `internal/guardrails/service.go` | snapshot → `map[string]serviceSnapshot`; BuildPipeline +ctx(Task 7) |
| `internal/gateway/inference_execute.go` | BuildPipeline 调用点补 ctx(Task 7) |
| `internal/app/app.go` | 启动时 seed 租户列表、background refresh 改为多租户迭代(Task 8,依赖 Tasks 1-7) |
| `internal/server/tenant_guard_test.go` | P5 集成测试(Task 10) |

---

## Shared Cache Infrastructure (全 6 Service 共用模式)

本节描述 Task 2-7 共用的缓存改造模式,避免每任务重复,各任务引此模式时只写差异。

### 改前(当前,单租户)

```go
type Service struct {
    tenantID  string       // "default"
    current   atomic.Value // *ruleSnapshot (for failover) or snapshot (other services)
    refreshMu sync.Mutex
}

func (s *Service) snapshot() snapshot {
    return s.current.Load().(snapshot)
}

func (s *Service) Refresh(ctx context.Context) error {
    s.refreshMu.Lock()
    defer s.refreshMu.Unlock()
    rows, err := s.store.ListEffective(ctx, s.tenantID)
    // ... build snapshot from rows ...
    s.current.Store(builtSnapshot)
}
```

### 改后(多租户)

```go
type Service struct {
    snapshots  atomic.Value // map[string]*ruleSnapshot (or map[string]snapshot)
    refreshMu  sync.Mutex   // serializes map rebuilds
}

func (s *Service) snapshotFor(ctx context.Context) ruleSnapshot {
    tenantID := core.GetTenantID(ctx)
    if tenantID == "" {
        tenantID = "default"
    }
    m := s.snapshots.Load().(map[string]*ruleSnapshot)
    if snap, ok := m[tenantID]; ok {
        return *snap
    }
    return emptyRuleSnapshot()
}

func (s *Service) RefreshAll(ctx context.Context, tenantIDs []string) error {
    // 全量重建:逐个租户读 store、建 snapshot、放入新 map、原子 swap
    // 由 app.go 在启动与后台刷新时调用
}

func (s *Service) Refresh(ctx context.Context) error {
    // 单租户刷新:加载当前 map、clone、修改一个 key、原子 swap
    // 用于 Recalculate 等即时刷新
}
```

**关键约定(所有 Service 通用):**
- `snapshotFor(ctx)` 返回的 snapshot 在调用期间有效(snapshot 指针稳定——map swap 只换整个 map,不修改已存 snapshot)。不需要读写锁。
- `tenantID` 为空时 fallback 到 `"default"`——覆盖 dev mode 与 P3 约定(`""` = 跨租户/unscoped)。
- `refreshMu` 串行化 map 重建,避免并发 Refresh 互相覆盖。

**注意:** guardrails 当前用 `sync.RWMutex` 持单一 `serviceSnapshot`,不是 `atomic.Value`.改造成 `map[string]*serviceSnapshot` 后改用 `sync.Mutex` + 普通 map 访问(guardrails 原版就没有用 `atomic.Value` 的"整个 swap"语义,继续保持 `sync.Mutex` 模式是更自然的过渡)。

### Background refresh 变化

当前每个 Service 的后台刷新是单租户:

```go
func (s *Service) StartBackgroundRefresh(interval time.Duration) func() {
    ...
    go func() {
        for range ticker.C {
            ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
            s.Refresh(ctx)    // 只有 default 租户
            cancel()
        }
    }()
}
```

P5 中每次 tick 跑 `s.RefreshAll(ctx, tenantIDs)`,tenantIDs 从 `tenants.Service.ListActive(ctx)` 获取。六个 Service 的后台刷新均改成此模式(Task 8,app.go 统一驱动)。

### 六个 Service 的 snapshot 类型清单

| Service | snapshot 类型 | 空快照构造 | package |
|---------|-------------|-----------|---------|
| failover | `ruleSnapshot`(struct, value) | `&ruleSnapshot{}` | `internal/failover` |
| workflows | `serviceSnapshot`(struct, value) | `emptySnapshot()` | `internal/workflows` |
| pricingoverrides | `snapshot`(struct, value) | (按 `snapshot` 零值) | `internal/pricingoverrides` |
| tagging | `snapshotTags`(struct, value) | `snapshotTags{}` | `internal/tagging` |
| virtualmodels | `snapshot`(struct, value) | `emptySnapshot(defaultEnabled)` | `internal/virtualmodels` |
| guardrails | `serviceSnapshot`(struct, value) | `serviceSnapshot{}` | `internal/guardrails` |

---

### Task 1: 推理 /v1/* hostGuard + 推理路径 enforceTenantAndRole 修复

**Files:**
- Modify: `internal/server/http.go`(推理路由组加 hostGuard)
- Modify: `internal/server/auth.go`(inference 路径 enforceTenantAndRole)
- Modify: `internal/server/auth_test.go`(TestAuthMiddleware_TenantAdminOnUnresolvedHost_InferencePath_OK 改为断言 401)
- Modify: `internal/server/http_test.go`(受 hostGuard 影响的现有推理路由测试——加 platform-host context 让测试通过)

**Interfaces:**
- Consumes: Task 1(Task 2/P4) 的 `hostGuard`。
- Produces: 推理 `/v1/*` 路由只在租户 host 上开放;平台 host 上访问推理路由 → 404(hostGuard("tenant"));未解析 host 上租户 key → 401(enforceTenantAndRole 拒绝)。

**行为约束:**
- 推理路由 `/v1/*`(chat/completions, responses, embeddings, audio/*, batches, realtime, models, files, images 等)加 `hostGuard("tenant")`——与 admin 平面 P4 的 `hostGuard("platform")` 形成镜像。
- 加 hostGuard 的方式:所有 `/v1/*` 路由注册到一个 `e.Group("/v1", hostGuard("tenant"))` 下,或用 `e.Use` 在 `/v1` 组上挂中间件。选择与现有代码结构一致的方式——若现有路由直接 `e.POST(...)` 而非组,则创建一个 group 后重新注册。
- 与 P4 B2 修复一致:dev mode(未配置 base_domain)也挂 hostGuard("tenant")——在 dev mode 下,hostGuard("tenant") 放行(非平台 host → passes),推理路由仍可达。
- `enforceTenantAndRole` 推理路径分支(当前 auth.go:219):把条件从 `ctxTenantID != "" && result.TenantID != "" && result.TenantID != ctxTenantID` 改为 `result.TenantID != "" && result.TenantID != ctxTenantID`——与 P4 对 admin 路径的修复一致。测试 `TestAuthMiddleware_TenantAdminOnUnresolvedHost_InferencePath_OK` 断言从 200 改为 401。

- [ ] **Step 1: 写路由分流的失败测试**

在 `internal/server/http_test.go`(或 `internal/server/tenant_guard_test.go`)追加:

```go
func TestInferenceRouting_TenantGuard_RejectsPlatformHost(t *testing.T) {
    cfg := defaultTestConfig(t) // 执行前 grep 确认 http_test.go 现有 test config 构造 helper
    e := buildTestServer(t, cfg) // 执行前 grep 确认真实构造入口

    // POST /v1/chat/completions with platform-host context → 404
    req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
    req.Header.Set("Content-Type", "application/json")
    req = req.WithContext(core.WithPlatformHost(req.Context(), true))
    rec := httptest.NewRecorder()
    e.ServeHTTP(rec, req)
    require.Equal(t, http.StatusNotFound, rec.Code, "inference routes must 404 on platform host")
}

func TestInferenceRouting_TenantGuard_AllowsTenantHost(t *testing.T) {
    cfg := defaultTestConfig(t)
    e := buildTestServer(t, cfg)

    req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
    req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
    rec := httptest.NewRecorder()
    e.ServeHTTP(rec, req)
    // tenant guard 放行,下一步才到 auth middleware(认证 check)
    require.NotEqual(t, http.StatusNotFound, rec.Code, "inference routes must pass hostGuard on tenant host")
}
```

执行前 `grep -n "^func.*Test.*Config\b\|^func buildTestServer\|^func defaultTestConfig" internal/server/http_test.go` 确认 test server 构造方式与 helper 名称,按实际签名调整。

- [ ] **Step 2: 写认证修复的失败测试**

在 `internal/server/auth_test.go`,把现有测试 `TestAuthMiddleware_TenantAdminOnUnresolvedHost_InferencePath_OK` 的断言从 200 改为 401 + `key_tenant_mismatch`:

```go
func TestAuthMiddleware_TenantAdminOnUnresolvedHost_InferencePath_401(t *testing.T) {
    // P5 fix: the inference-path enforceTenantAndRole now mirrors the admin-path
    // P4 fix — a tenant-scoped key on an unresolved host is rejected.
    ...
    require.Equal(t, http.StatusUnauthorized, rec.Code)
    require.Contains(t, rec.Body.String(), "key_tenant_mismatch")
}
```

执行前 `grep -n "TestAuthMiddleware_TenantAdminOnUnresolvedHost_InferencePath" internal/server/auth_test.go` 确认当前测试函数名与具体断言行号,按实际调整。

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/server/... -run "TestInferenceRouting_TenantGuard|TestAuthMiddleware_TenantAdminOnUnresolvedHost_InferencePath" -v`
Expected: 推理路由测试编译或失败(hostGuard 尚未挂到 /v1/*)。

- [ ] **Step 4: 实现推理路由 hostGuard + 认证修复**

1. 在 `internal/server/http.go`,把当前 `/v1/*` 路由注册方式(可能是直接用 `e.POST("/v1/chat/completions", ...)` 或已有 `v1Group`)改成统一加 `hostGuard("tenant")`。执行前 grep 当前 `/v1/` 路由注册方式,选择最小改动——若已有 `e.Group("/v1")`,给该 group 加 `hostGuard("tenant")`;若路由直接挂在 `e` 上,创建 group 并搬运。示例:

```go
// before:
e.POST("/v1/chat/completions", handler.ChatCompletion)
e.GET("/v1/models", handler.ListModels)

// after (if using a group):
v1 := e.Group("/v1", hostGuard("tenant"))
v1.POST("/chat/completions", handler.ChatCompletion)
v1.GET("/models", handler.ListModels)
```

注意:legacy `/admin/api/v1` 路径是 admin 平面(P4 已处理),与推理 `/v1/*` 是不同的前缀,不冲突。

2. 在 `internal/server/auth.go`,修改推理路径分支:

```go
// Inference path — P5 fix: reject tenant-scoped keys without resolved tenant.
if result.TenantID != "" && result.TenantID != ctxTenantID {
    return (&core.GatewayError{
        Type:       core.ErrorType("key_tenant_mismatch"),
        Message:    "auth key does not belong to this tenant",
        StatusCode: http.StatusUnauthorized,
    }).WithCode("key_tenant_mismatch")
}
```

注释掉(或删除)原有的 `ctxTenantID != ""` 条件。

- [ ] **Step 5: 检查并修复受影响的现有测试**

Run: `go test ./internal/server/... -v` 检查 fails。

常见受影响的测试:
- `http_test.go` 中构造 server 并打推理路由的测试现在需要 platform-host context 变成 404——这些测试以前没有 hostGuard 所以 200/其它状态码。逐个检查,对"本意是平台 host 打推理路由"的测试:要么加 `core.WithPlatformHost(ctx, true)`(预期 404),要么加 `core.WithTenantID(ctx, "some-tenant")`(预期 pass guard)。判断依据:测试名/注释里的业务场景。
- `auth_test.go` 中 `TestAuthMiddleware_TenantAdminOnUnresolvedHost_InferencePath_OK` 断言改为 401(Step 2 已做)。

调整完成后 `go test ./internal/server/... -v` 全部通过。

- [ ] **Step 6: 提交**

```bash
git add internal/server/http.go internal/server/auth.go internal/server/auth_test.go internal/server/http_test.go
git commit -m "feat(server): add hostGuard(tenant) to inference /v1/* routes; tighten inference-path enforceTenantAndRole"
```

---

### Task 2: failover — per-tenant cache + ResolveFailovers ctx

**Files:**
- Modify: `internal/failover/service.go`(snapshot → map, RefreshAll, snapshotFor, 删除旧 `tenantID` 字段/`Refresh`/`snapshot()`)
- Modify: `internal/failover/resolver.go`(`ResolveFailovers` +ctx 参数)
- Modify: `internal/gateway/interfaces.go`(`FailoverResolver.ResolveFailovers` +ctx)
- Modify: `internal/gateway/failover.go`(`FailoverSelectors` +ctx)
- Modify: `internal/gateway/inference_execute.go`(`tryFailoverResponse`/`tryFailoverStream` 传 ctx)
- Modify: 任何 `ResolveFailovers` 的测试 stub/mock

**Interfaces:**
- Consumes: Shared Cache Infrastructure(本计划开头的共用模式),`core.GetTenantID(ctx)`。
- Produces: `func (r *Resolver) ResolveFailovers(ctx context.Context, resolution *core.RequestModelResolution, op core.Operation) []core.ModelSelector`。`func (s *Service) RefreshAll(ctx context.Context, tenantIDs []string) error`;`func (s *Service) Refresh(ctx context.Context) error`(保留,从 ctx 拿 tenantID 做单租户刷新,用于即时 Recalculate 路径)。

**缓存改造(按 Shared Cache Infrastructure 模式,差异如下):**

- snapshot 类型:`map[string]*ruleSnapshot`
- 移除此 Service 原有的 `tenantID string` 字段(因为 snapshot 现在按租户 key 索引,不需要硬编码 default)
- `Refresh(ctx)` 改为从 `core.GetTenantID(ctx)` 拿 tenantID:
```go
func (s *Service) Refresh(ctx context.Context) error {
    s.refreshMu.Lock()
    defer s.refreshMu.Unlock()
    tenantID := core.GetTenantID(ctx)
    if tenantID == "" { tenantID = "default" }
    rows, err := s.store.ListEffective(ctx, tenantID)
    if err != nil { return err }
    next := s.buildSnapshot(rows, s.configRows)
    m := s.currentSnapshotMap()
    cloned := cloneMap(m) // 浅拷贝 map
    cloned[tenantID] = next
    s.snapshots.Store(cloned)
    return nil
}
```

- `RefreshAll(ctx, tenantIDs)` 全量重建(用于 startup + background tick),不依赖 ctx 中的 tenantID:
```go
func (s *Service) RefreshAll(ctx context.Context, tenantIDs []string) error {
    s.refreshMu.Lock()
    defer s.refreshMu.Unlock()
    if len(tenantIDs) == 0 { tenantIDs = []string{"default"} }
    newMap := make(map[string]*ruleSnapshot, len(tenantIDs))
    for _, tid := range tenantIDs {
        rows, err := s.store.ListEffective(ctx, tid)
        if err != nil { return fmt.Errorf("failover refresh tenant %s: %w", tid, err) }
        newMap[tid] = s.buildSnapshot(rows, s.configRows)
    }
    s.snapshots.Store(newMap)
    return nil
}
```

- `snapshotFor(ctx)`:
```go
func (s *Service) snapshotFor(ctx context.Context) *ruleSnapshot {
    tenantID := core.GetTenantID(ctx)
    if tenantID == "" { tenantID = "default" }
    m := s.snapshots.Load().(map[string]*ruleSnapshot)
    if snap, ok := m[tenantID]; ok {
        return snap
    }
    return &ruleSnapshot{}
}
```

- 调用点变化:`Resolver.ResolveFailovers` 读 snapshot 的方式从 `r.service.snapshot()` 改为 `r.service.snapshotFor(ctx)`。
- `ForTenant` 方法(admin 面,P4 加的)不受影响——它们直接打 store,不调 snapshot。

**ctx 参数的线程方式:**

- [ ] **Step 1: 重命名旧的 `FailoverResolver` 并定义新的 ctx 版本**

在 `internal/gateway/interfaces.go`,把现有接口定义加注释并替换为带 ctx 的:

```go
// FailoverResolver resolves alternate concrete model selectors for a translated
// request after the primary selector has been resolved.
// The ctx parameter carries the resolved tenant ID (core.GetTenantID(ctx)).
type FailoverResolver interface {
    ResolveFailovers(ctx context.Context, resolution *core.RequestModelResolution, op core.Operation) []core.ModelSelector
}
```

- [ ] **Step 2: 更新 `internal/failover/service.go` 的缓存结构**

按 Shared Cache Infrastructure 中的 failover 差异实现;删除原有 `tenantID` 字段(在 `NewService` 中移除初始化);保留 `StartBackgroundRefresh` 但内部改成调 `RefreshAll`(tenantIDs 从参数传入——Task 8 接线时才需要 `tenants.Service` 活性;在此之前传 `[]string{"default"}` 兼容)。

执行前 `grep -n "tenantID\|s\.tenantID" internal/failover/service.go` 确认所有引用点,逐处替换为 tenantID-from-ctx 或删除(该字段不再需要)。

执行前 `grep -n "StartBackgroundRefresh" internal/failover/service.go internal/app/app.go` 确认调用方式与 interval 参数。

- [ ] **Step 3: 更新 `internal/failover/resolver.go`**

```go
func (r *Resolver) ResolveFailovers(ctx context.Context, resolution *core.RequestModelResolution, op core.Operation) []core.ModelSelector {
    if r.service == nil || resolution == nil {
        return nil
    }
    current := r.service.snapshotFor(ctx)
    // ... rest of logic unchanged, using current instead of r.service.snapshot() ...
}
```

- [ ] **Step 4: 更新 `internal/gateway/failover.go`**

```go
func (o *InferenceOrchestrator) FailoverSelectors(ctx context.Context, workflow *core.Workflow) []core.ModelSelector {
    if o.failoverResolver == nil || workflow == nil || workflow.Resolution == nil || !workflow.FailoverEnabled() {
        return nil
    }
    return o.failoverResolver.ResolveFailovers(ctx, workflow.Resolution, workflow.Endpoint.Operation)
}
```

- [ ] **Step 5: 更新 `tryFailoverResponse`/`tryFailoverStream` 调用点**

`internal/gateway/inference_execute.go`:在调用 `FailoverSelectors` 处补 `ctx` 参数。执行前 `grep -n "FailoverSelectors" internal/gateway/inference_execute.go` 确认确切位置。

- [ ] **Step 6: 运行全量测试确认通过**

Run: `go test ./internal/failover/... ./internal/gateway/... -v`
Expected: 全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add internal/failover/ internal/gateway/interfaces.go internal/gateway/failover.go internal/gateway/inference_execute.go
git commit -m "feat(failover): per-tenant rule cache with ctx-aware ResolveFailovers"
```

---

### Task 3: workflows — per-tenant cache + Match ctx

**Files:**
- Modify: `internal/workflows/service.go`(snapshot → map, snapshotFor, RefreshAll; Match +ctx; 删除旧 tenantID 字段)
- Modify: `internal/gateway/interfaces.go`(`WorkflowPolicyResolver.Match` +ctx)
- Modify: `internal/gateway/workflow_policy.go`(`ApplyWorkflowPolicy` 传 ctx 进 Match)
- Modify: `internal/server/model_validation.go`(`applyWorkflowPolicy` 传 ctx)
- Modify: `internal/gateway/batch_orchestrator.go`(batch 路径传 ctx)
- Modify: 所有 `Match` 测试 stub/mock

**Interfaces:**
- Consumes: Shared Cache Infrastructure,`core.GetTenantID(ctx)`。
- Produces: `func (s *Service) Match(ctx context.Context, selector core.WorkflowSelector) (*core.ResolvedWorkflowPolicy, error)`

**缓存改造:** 按 Shared Cache Infrastructure 模式(workflows 差异:snapshot 类型 `map[string]serviceSnapshot`,空快照 `emptySnapshot()`)。Match 改为读 `s.snapshotFor(ctx)`。

**注意:** workflows 的 `Match` 返回 `(*core.ResolvedWorkflowPolicy, error)`,其中 `error` 是 `guardrailServiceError` 的包装——不要改动其错误语义,只加 ctx 参数,内部从 ctx 拿 tenantID 选 snapshot。

- [ ] **Step 1: 接口签名变更**

`internal/gateway/interfaces.go`: `Match(ctx context.Context, selector core.WorkflowSelector) (*core.ResolvedWorkflowPolicy, error)`

- [ ] **Step 2: Service 缓存改造 + Match ctx**

`internal/workflows/service.go`: snapshot → map[tenantID]serviceSnapshot; `Match(ctx, ...)` 读 `s.snapshotFor(ctx)`。

- [ ] **Step 3: 调用点更新**

`internal/gateway/workflow_policy.go`: `resolver.Match(ctx, selector)`。
`internal/server/model_validation.go`: `applyWorkflowPolicy` 函数传 ctx 进入 `gateway.ApplyWorkflowPolicy`。
`internal/gateway/batch_orchestrator.go`: batch 路径传 ctx。

执行前 `grep -rn "\.Match(" internal/` 找所有调用点(含测试文件)。批量更新——测试 stub 也需要更新签名但保留行为不变(忽略 ctx)。

- [ ] **Step 4: 测试 + 提交**

Run: `go test ./internal/workflows/... ./internal/gateway/... ./internal/server/... -v`
Expected: PASS。

```bash
git add internal/workflows/ internal/gateway/interfaces.go internal/gateway/workflow_policy.go internal/server/model_validation.go internal/gateway/batch_orchestrator.go
git commit -m "feat(workflows): per-tenant compiled workflow cache with ctx-aware Match"
```

---

### Task 4: pricingoverrides — per-tenant cache + ResolvePricing ctx

**Files:**
- Modify: `internal/pricingoverrides/service.go`(snapshot → map, snapshotFor, RefreshAll)
- Modify: `internal/pricingoverrides/resolver.go`(`ResolvePricing` +ctx)
- Modify: `internal/usage/pricing.go`(`PricingResolver` 接口 +ctx)
- Modify: `internal/providers/registry_metadata.go`(`ModelRegistry.ResolvePricing` +ctx——这需要改 `providers.ModelRegistry` 的接口实现)
- Modify: 所有 `ResolvePricing` 调用点(7 个,见探索报告)

**Interfaces:**
- Consumes: Shared Cache Infrastructure,`core.GetTenantID(ctx)`。
- Produces: `func (s *Service) ResolvePricing(ctx context.Context, model, providerType string) *core.ModelPricing`
- `usage.PricingResolver` 本身定义在 `internal/usage/pricing.go`,需要加 ctx。
- `providers.ModelRegistry` 也实现了 `ResolvePricing`(registry_metadata.go),需要同步加 ctx——它在 `usage.PricingResolver` 接口下。

**缓存改造:** 按 Shared Cache Infrastructure 模式(pricingoverrides 差异:snapshot 类型 `map[string]snapshot`)。`ResolvePricing` 加 `ctx context.Context` 参数,内部 `s.snapshotFor(ctx)`。

**调用点更新特别注意:**
- `internal/gateway/batch_usage.go` — `LogBatchUsageFromBatchResults` 当前无 ctx,需要加 `ctx context.Context` 参数或从内部派生(该函数签名链需要改动)。
- `internal/usage/stream_observer.go` — SSE observer 无 ctx,需要加字段或从 observer 创建时传入 context。
- `internal/usage/recalculate_pricing.go` — `recalculateEntryCosts` 当前无 ctx,需要补或传 `context.Background()`(recalculate 是 admin 后台触发,不经过请求 context;租户信息从 usage entry 中派生还是传 ""?)。判断:P4 的 admin `RecalculateUsagePricing` handler 调用处有 ctx,其内部调 `recalculateEntryCosts`——把 ctx 传下去。

- [ ] **Step 1: 接口签名变更**

`internal/usage/pricing.go`: `ResolvePricing(ctx context.Context, model, providerType string) *core.ModelPricing`

- [ ] **Step 2: pricingoverrides.Service 缓存改造 + ResolvePricing ctx**

`internal/pricingoverrides/service.go`: snapshot → map[tenantID]snapshot。
`internal/pricingoverrides/resolver.go`: `ResolvePricing(ctx, model, providerName)` → `s.snapshotFor(ctx)`。

- [ ] **Step 3: providers.ModelRegistry.ResolvePricing 加 ctx**

`internal/providers/registry_metadata.go`: `func (r *ModelRegistry) ResolvePricing(ctx context.Context, model, providerType string) *core.ModelPricing`——内部忽略 ctx(registry 是全局的,定价从数据文件来,不按租户)。

- [ ] **Step 4: 所有 7 个调用点更新**

逐个 grep 确认位置后更新签名与调用。对于没有 ctx 的函数:
- `LogBatchUsageFromBatchResults` → 加 `ctx context.Context` 参数(它由 gateway batch orchestrator 调用,后者有 ctx)。
- `stream_observer.go` → observer struct 加 `ctx context.Context` 字段(在 observer 创建时注入)。
- `recalculate_pricing.go` → 从上游传 ctx(handler → service → recalculate)。

- [ ] **Step 5: 测试 + 提交**

Run: `go test ./internal/pricingoverrides/... ./internal/usage/... ./internal/providers/... ./internal/gateway/... ./internal/server/... -v`
Expected: PASS。

```bash
git add internal/pricingoverrides/ internal/usage/pricing.go internal/providers/registry_metadata.go internal/gateway/ internal/server/
git commit -m "feat(pricing): per-tenant pricing override cache with ctx-aware ResolvePricing"
```

---

### Task 5: tagging — per-tenant cache + ExtractLabels/StripHeaders ctx

**Files:**
- Modify: `internal/tagging/service.go`(snapshot → map, snapshotFor, RefreshAll; ExtractLabels +ctx; StripHeaders +ctx)
- Modify: `internal/tagging/tagging.go`(纯函数 `ExtractLabels(rules, headers)` 签名不动——它是包级函数,由 Service 的 `ExtractLabels` 传入 rules)
- Modify: `internal/server/tagging.go`(`TaggingCapture` 中间件传 ctx)

**Interfaces:**
- Consumes: Shared Cache Infrastructure,`core.GetTenantID(ctx)`。
- Produces: `func (s *Service) ExtractLabels(ctx context.Context, headers http.Header) []string`;`func (s *Service) StripHeaders(ctx context.Context) map[string]struct{}`

**缓存改造:** 按 Shared Cache Infrastructure 模式(tagging 差异:snapshot 类型 `map[string]snapshotTags`)。

**调用点:** `internal/server/tagging.go:23,27`——`TaggingCapture` 中间件已有 `req.Context()`,传入即可。

- [ ] **Step 1: 缓存改造 + 方法签名变更**

`internal/tagging/service.go`: snapshot → map;`ExtractLabels(ctx, headers)`/`StripHeaders(ctx)` 调用 `s.snapshotFor(ctx)`。

- [ ] **Step 2: 调用点更新**

`internal/server/tagging.go`: `service.ExtractLabels(req.Context(), req.Header)` / `service.StripHeaders(req.Context())`

- [ ] **Step 3: 测试 + 提交**

Run: `go test ./internal/tagging/... ./internal/server/... -v`

```bash
git add internal/tagging/service.go internal/server/tagging.go
git commit -m "feat(tagging): per-tenant tagging rules cache with ctx-aware ExtractLabels/StripHeaders"
```

---

### Task 6: virtualmodels — per-tenant cache + 无 ctx 方法补 ctx

**Files:**
- Modify: `internal/virtualmodels/service.go`(snapshot → map, snapshotFor, RefreshAll; 删除旧 `tenantID` 字段)
- Modify: `internal/virtualmodels/resolve.go`(无 ctx 的热路径方法: `ResolveModel`、`Resolve`、`Supports`、`ExposedModels`、`ExposedModelsFiltered`、`ExposedModelsForUserPath`——加 ctx;已经有 ctx 的 `ResolveModelForUserPath`、`ValidateModelAccess`、`AllowsModel`、`FilterPublicModels` 只改内部 snapshot 读法)
- Modify: 所有调用 `ResolveModel`/`Resolve`/`Supports` 等方法的调用点(需要 grep 全仓库找)

**Interfaces:**
- Consumes: Shared Cache Infrastructure,`core.GetTenantID(ctx)`。
- Produces: 热路径方法的 ctx 版本。

**缓存改造:** 按 Shared Cache Infrastructure 模式(virtualmodels 差异:snapshot 类型 `map[string]snapshot`,空快照 `emptySnapshot(defaultEnabled)`)。

**特别注意:** virtualmodels 的 `ResolveModel`/`Supports` 等在 gateway 层被广泛使用(`inference_prepare.go`、`inference_execute.go`、provider dispatch 等)——需要全量 grep 调用点并更新。编译时接口 check(`var _ interface { ... } = (*Service)(nil)` 在 `service.go:568`) 需要更新签名。

- [ ] **Step 1: 缓存改造**

`internal/virtualmodels/service.go`: snapshot → map[tenantID]snapshot;加入 `snapshotFor(ctx)`。

- [ ] **Step 2: 方法签名变更(grep 全仓库调用点)**

逐个调查并更新:
```go
// resolve.go
func (s *Service) ResolveModel(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error)
func (s *Service) Resolve(ctx context.Context, model, provider string) (core.ModelSelector, bool, error)
func (s *Service) Supports(ctx context.Context, model string) bool
func (s *Service) ExposedModels(ctx context.Context) []core.Model
func (s *Service) ExposedModelsFiltered(ctx context.Context, allow func(core.ModelSelector) bool) []core.Model
func (s *Service) ExposedModelsForUserPath(ctx context.Context, userPath string, allow func(core.ModelSelector) bool) []core.Model
```

已有 ctx 的方法只改内部 snapshot 读取:
```go
func (s *Service) ResolveModelForUserPath(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error)
func (s *Service) ValidateModelAccess(ctx context.Context, selector core.ModelSelector) error
func (s *Service) AllowsModel(ctx context.Context, selector core.ModelSelector) bool
func (s *Service) FilterPublicModels(ctx context.Context, models []core.Model) []core.Model
```

- [ ] **Step 3: 全量调用点更新**

执行 `grep -rn "\.ResolveModel\b\|\.Supports\b\|\.ExposedModels\b\|\.ExposedModelsFiltered\b\|\.ExposedModelsForUserPath\b\|\.Resolve\b" internal/` 找所有调用点(排除 virtualmodels 包内部方法定义)。最关键的调用点在 `internal/gateway/inference_prepare.go`(模型解析)与 `internal/providers`(provider dispatch)。

- [ ] **Step 4: 编译时接口 check 更新**

`internal/virtualmodels/service.go:568`——把 `var _ interface { ... } = (*Service)(nil)` 中的方法签名同步为 ctx 版本。

- [ ] **Step 5: 测试 + 提交**

Run: `go test ./internal/virtualmodels/... ./internal/gateway/... ./internal/server/... ./internal/providers/... -v`
Expected: PASS(所有 compile-time 错误解决后)。

```bash
git add internal/virtualmodels/ internal/gateway/ internal/server/ internal/providers/
git commit -m "feat(virtualmodels): per-tenant model resolution cache with ctx-aware hot-path methods"
```

---

### Task 7: guardrails — per-tenant cache + BuildPipeline ctx

**Files:**
- Modify: `internal/guardrails/service.go`(snapshot → map, snapshotFor, RefreshAll; BuildPipeline +ctx; 删除旧 `tenantID` 字段)
- Modify: `internal/gateway/inference_execute.go`(BuildPipeline 调用点补 ctx)
- Modify: 所有 `BuildPipeline` 调用点(含测试 stub)

**Interfaces:**
- Consumes: Shared Cache Infrastructure(guardrails 差异: 用 `sync.Mutex` 保护 `map[string]*serviceSnapshot`,不用 atomic.Value),`core.GetTenantID(ctx)`。
- Produces: `func (s *Service) BuildPipeline(ctx context.Context, steps []StepReference) (*Pipeline, string, error)`

**缓存改造:** guardrails 当前用 `sync.RWMutex` 持单一 `serviceSnapshot`,不 swap。改造成 map 后继续保持 mutex 模式:
- `mu sync.Mutex` 保护 `snapshots map[string]*serviceSnapshot`
- `snapshotFor(ctx)` 读:mu.Lock();snap=snapshots[key];mu.Unlock()
- `RefreshAll`/`Refresh` 重建:mu.Lock();构建新 map;赋值;mu.Unlock()

- [ ] **Step 1: BuildPipeline +ctx**

`internal/guardrails/service.go`: `BuildPipeline(ctx context.Context, steps []StepReference) (*Pipeline, string, error)`

- [ ] **Step 2: 缓存改造 + ctx 线程**

Service 改为 `snapshots map[string]*serviceSnapshot` + `mu sync.Mutex`;`snapshotFor(ctx)` 方法。

`Refresh`/`RefreshAll` 改为按租户重建 snapshot。

- [ ] **Step 3: 调用点更新**

`grep -rn "\.BuildPipeline(" internal/` 找出所有调用点,补 ctx(内部从 ctx 拿 tenantID 读正确的 guardrail pipeline)。

- [ ] **Step 4: 测试 + 提交**

Run: `go test ./internal/guardrails/... ./internal/gateway/... -v`

```bash
git add internal/guardrails/ internal/gateway/
git commit -m "feat(guardrails): per-tenant guardrail pipeline cache with ctx-aware BuildPipeline"
```

---

### Task 8: app.go 装配 + background 多租户 refresh

**Files:**
- Modify: `internal/app/app.go`(六 Service 启动时 seed 初始租户列表,background refresh 传多租户列表)
- Modify: 各 Service 的 `StartBackgroundRefresh`(改为从参数接收租户 ID 列表)

**Interfaces:**
- Consumes: Tasks 1-7 的所有改动,`tenants.Service.List(ctx)`。
- Produces: 全 Service 在 app 启动时按所有 active 租户初始化 snapshot,后台定期全量刷新。

**assembly 变化:**

当前 pattern(以 failover 为例,`internal/app/app.go` 中):
```go
// 初始化后立即 Refresh(default 租户)
if err := failoverSvc.Refresh(ctx); err != nil { ... }
// 启动后台单租户刷新
stopFailoverRefresh := failoverSvc.StartBackgroundRefresh(30 * time.Second)
```

P5 需要改为:
```go
// 获取 active 租户列表
activeTenants, err := tenantSvc.List(ctx)
if err != nil { return nil, err }
tenantIDs := make([]string, 0, len(activeTenants))
for _, t := range activeTenants {
    if t.IsDisabled() { continue }
    tenantIDs = append(tenantIDs, t.ID)
}
if len(tenantIDs) == 0 { tenantIDs = []string{"default"} }

// 初始化 seed 多租户
if err := failoverSvc.RefreshAll(ctx, tenantIDs); err != nil { ... }
// 后台多租户 refresh——传给 StartBackgroundRefresh 的参数包含 tenantSvc 引用(或者每次 tick 调 tenantSvc.List 取最新列表)
```

**注意:** `tenants.Service.List(ctx)` 中 ctx 需要支持取消。后台 refresh 使用 `context.WithTimeout(context.Background(), 30*time.Second)` 即可。

- [ ] **Step 1: 改各 Service 的 StartBackgroundRefresh**

6 个 Service 的 `StartBackgroundRefresh` 方法的 timer handler 从 `s.Refresh(ctx)`(单租户)改为 `s.RefreshAll(ctx, tenantIDs)`。tenantIDs 通过以下方式之一获取:
(a) 作为 `StartBackgroundRefresh` 的新参数传入 `getTenantIDs func(context.Context) ([]string, error)`——由 app.go 提供闭包。
(b) 在 `StartBackgroundRefresh` 中直接传 Service 一个 `tenants.Service` 引用。

选 (a)——避免让各个 Service 依赖 `tenants.Service`(不引入循环依赖)。

- [ ] **Step 2: 改 app.go**

在 `internal/app/app.go` 中,每次初始化一个 Service 时:
1. 调 `svc.RefreshAll(ctx, tenantIDs)` 替代原来 `svc.Refresh(ctx)`。
2. 把 `svc.StartBackgroundRefresh(interval)` 改为 `svc.StartBackgroundRefresh(interval, func(ctx context.Context) ([]string, error) { ... 调 tenantSvc.List() ... })`。

其余装配逻辑不变(TenantAdminHandler/PlatformAdminHandler 等沿用 P4 装配)。

- [ ] **Step 3: 全量构建 + 测试**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/app/app.go internal/failover/service.go internal/workflows/service.go internal/pricingoverrides/service.go internal/tagging/service.go internal/virtualmodels/service.go internal/guardrails/service.go
git commit -m "feat(app): seed per-tenant caches at startup; background multi-tenant refresh for all 6 config services"
```

---

### Task 9: 配额中间件(租户级 budget check)——可选

**Files:**
- Create: `internal/server/tenant_quota.go`
- Modify: `internal/server/http.go`(全局中间件链挂 quota checker)
- Modify: `internal/budget/service.go`(或新建 per-tenant 接口)

**目标:** 基于 `core.GetTenantID(ctx)` 与 `budget.Service` 现有 user-path 维度,组合出租户级限额。

**行为:**
- 中间件在全局 middleware chain 中挂载(在 auth 之后、inference handler 之前)。
- 读 `core.GetTenantID(ctx)`(或 fallback `"default"`)。
- 调 `budget.Service.Check(ctx, tenantID)`——如果 Budget Service 当前 Check 签名是 `Check(ctx context.Context, userPath string, now time.Time) error`,则需扩展为 `CheckTenant(ctx context.Context, tenantID string, now time.Time) error`(新增接口方法)。
- 超限时返回 429 Too Many Requests 或 402 Payment Required(与现有 budget 错误一致)。

**注意:** 此任务为可选——若 P5 时间紧张可延至 P6,因为核心交付(6 Service 按租户缓存 + 4 接口 ctx + 推理 host guard)在 Tasks 1-8 已完成。若包含此任务,独立测试与提交。

- [ ] **Step 1: 写中间件与测试**
- [ ] **Step 2: 装配 + 全量测试**
- [ ] **Step 3: 提交**

```bash
git add internal/server/tenant_quota.go internal/server/http.go internal/budget/
git commit -m "feat(server): add per-tenant quota middleware for inference requests"
```

---

### Task 10: 端到端集成测试

**Files:**
- Create: `internal/server/tenant_visibility_integration_test.go`

**目标(对照 P5 交付):**
1. 平台 host POST /v1/chat/completions → 404(hostGuard("tenant") 生效)。
2. 租户 A 的 API key 在租户 A 的 host 上请求推理 → 200,且命中的 failover/workflow 等流程用的是该租户自己的 snapshot(非 default 租户的)。
3. 租户 A 的 API key 在未解析 host(foreign Host)上 → 401(enforceTenantAndRole P5 fix)。
4. 租户 A 创建一条 virtual-model 覆盖(failover/guardrail 规则),其推理请求受此覆盖影响(通过 tenant-aware snapshot);租户 B 不受影响。

- [ ] **Step 1: 装配测试链(复用 P4 的 TenantResolver + AuthMiddleware + hostGuard 模式)**
- [ ] **Step 2: 写测试**
- [ ] **Step 3: 运行 + 提交**

```bash
git add internal/server/tenant_visibility_integration_test.go
git commit -m "test(server): add end-to-end tenant visibility integration test"
```

---

### Task 11: 验证 + Completion Notes

**Files:**
- 无生产代码改动。
- Modify: `docs/superpowers/plans/2026-08-03-saas-multi-tenant-p5.md`(追加 Completion Notes)

- [ ] **Step 1: `go build ./...`** (expected: PASS)
- [ ] **Step 2: `go test ./...`** (expected: 全部 PASS;PG/Mongo skip 不计入失败)
- [ ] **Step 3: `go vet ./...`** (expected: 只有 P3 既有 3 个 core/ dup json tag 警告,无新增)
- [ ] **Step 4: Dashboard JS 测试** `node --test internal/admin/dashboard/static/js/modules/*.test.cjs`(确认未受影响)
- [ ] **Step 5: 追加 Completion Notes**

写入实际的 commit range、验证结果、已知 deferred 项(ForTenant P4 deferred 对齐哪些做了哪些没做、quota 中间件若没做注明建议 P6)、面向 P6/P7 的建议表。

```bash
git add docs/superpowers/plans/2026-08-03-saas-multi-tenant-p5.md
git commit -m "docs(plan): append P5 completion notes"
```

---

## Self-Review

**1. Spec coverage(对照 P4 Completion Notes 中 P5 deferred 表与之前的设计文档):**
- 推理热路径 6 Service 按租户缓存 → Tasks 2-7 ✓
- 4 个接口签名加 ctx → Tasks 2-5,Task 7(BuildPipeline 无正式接口但有调用点) ✓
- 推理 /v1/* host guard → Task 1 ✓
- 推理路径 enforceTenantAndRole ctxTenantID=="" 漏洞 → Task 1 ✓
- provider 按租户可见性 → 通过 virtualmodels per-tenant snapshot 自动实现,不单独任务 ✓
- 配额中间件 → Task 9(可选,独立可推迟) ✓
- ForTenant P4 deferred 对齐(nil-store guard 等) → 不在 P5 主线,只在 background refresh 迭代时顺带处理 ✓

**2. Placeholder scan:** 所有"执行前 grep 确认"是必要探查指令(与 P1-P4 风格一致),不是占位符——实现者必须执行并按实际结果调整,编译期会暴露不一致。无 TBD/TODO。

**3. Type consistency:**
- Shared Cache Infrastructure 模式定义了 `snapshotFor(ctx)`、`Refresh(ctx)`、`RefreshAll(ctx, tenantIDs)` 的精确签名,六个 Service 在各 Task 中复用,差异列表明确。
- `FailoverResolver.ResolveFailovers(ctx, ...)` 在 Task 2 定义,Task 2 的 gateway 调用点引用——一致。
- `WorkflowPolicyResolver.Match(ctx, ...)` 在 Task 3 定义,Task 3 的调用点引用——一致。
- `PricingResolver.ResolvePricing(ctx, ...)` 在 Task 4 定义,Task 4 的所有调用点(7 个)逐个列出——需逐个核实。
- `StartBackgroundRefresh` 新参数在 Task 8 定义,六个 Service 的调用点一致。

**4. 已知风险与执行注意事项:**
- **最大的集成风险:** virtualmodels 的 `ResolveModel`/`Supports` 等在很深的调用栈中被使用(provider dispatch、model validation、gateway prepare 等),加 ctx 可能触发连锁签名变更。Task 6 的调用点 grep 必须全量覆盖,不能漏任何文件。建议在 Task 6 实现前先用 `go build ./...` 确认只有 virtualmodels 包自己编译不过,再逐个文件修复。
- **batch_usage.go 与 stream_observer.go** 这两个调用点没有 ctx,PricingResolver 加 ctx 后需要重构其签名链——Task 4 Step 4 已在计划中指出。
- **guardrails 缓存不是 atomic.Value**——它当前用 RWMutex,所以 snapshotFor 的实现需要 Lock/Unlock(map 读),与另 5 个 Service 的 atomic.Load 不同。Task 7 的 step 已在模式差异中说明。
- **接口签名的 building-then-breaking:** 每个 "加 ctx" 的任务都是全部实现+调用点一起改,不存在中间 commit 编译不过。但 go.sum 不变,go.mod 不需改动。
- **测试 Stub/Mock 同步:** 所有 `.Match`/`.ResolveFailovers`/`.ResolvePricing` 的测试替身都需要同步改签名。通常分布在 gateway 的 `*_test.go` 和 server 的 `*_test.go` 中。每个 Task 的 Step 2-3 已要求 grep 调用点并更新,但测试 stub 是一类特别容易漏掉的调用点——建议每个 Task 先跑 `go test ./... 2>&1 | grep -E "not enough|too many"` 定位编译错误。
- P5 改动涉及 6 个 Service 包 + gateway + server + app,编译影响面比 P4 大。建议用 SDD 的 subagent 模式逐个 Task 推进,确保每个 Task review 干净再继续下一 Task。

---

## P5 Completion Notes (2026-08-03)

**Status:** Complete. Tasks 1–8 + 10 + 11 done; Task 9 (quota middleware) DEFERRED to P6 per the plan's optional semantics. Full verification passed.

**Commits:** `754a7fa..980ea69` on master (10 commits) + this docs commit. Base before Task 1: `dc556cc` (P4 final fix). Task-by-task:

| Commit | Task | Message |
|---|---|---|
| `754a7fa` | — | docs(plan): add P5 inference-hot-path tenant-visibility implementation plan |
| `58c0d51` | 1 | feat(server): add hostGuard(tenant) to inference /v1/* routes; tighten inference-path enforceTenantAndRole |
| `35214ab` | 2 | feat(failover): per-tenant rule cache with ctx-aware ResolveFailovers |
| `3ef64a9` | 3 | feat(workflows): per-tenant compiled workflow cache with ctx-aware Match |
| `7c59c16` | 4 | feat(pricing): per-tenant pricing override cache with ctx-aware ResolvePricing |
| `e58c6f1` | 5 | feat(tagging): per-tenant tagging rules cache with ctx-aware ExtractLabels/StripHeaders |
| `1827bb3` | 6 | feat(virtualmodels): per-tenant model resolution cache with ctx-aware hot-path methods |
| `56051b0` | 7 | feat(guardrails): per-tenant guardrail pipeline cache with ctx-aware BuildPipeline |
| `29c9608` | 8 | feat(app): seed per-tenant caches at startup; background multi-tenant refresh for all 6 config services |
| `980ea69` | 10 | test(server): add end-to-end tenant visibility integration test |

Task 9 (quota middleware) was intentionally not implemented — deferred to P6, no commit.

**Verification (2026-08-03):**
- `go build ./...` — PASS
- `go test ./...` — PASS (61 packages `ok`, 0 `FAIL`; PG/Mongo-dependent tests skip by default, per P3/P4 convention)
- `go vet ./...` — only the 3 pre-existing `internal/core` duplicate-`json`-tag warnings (`responses.go:148`, `types.go:79`, `types.go:124`) already recorded in the P3/P4 Completion Notes; no new warnings
- Dashboard JS tests (`node --test internal/admin/dashboard/static/js/modules/*.test.cjs`) — 399/402 pass. 3 pre-existing `readCSSRule` assertion failures in `dashboard-layout` / `timezone-layout` / `workflows-layout` `.test.cjs` (expected CSS rules absent from the stylesheet). Re-verified unrelated to P5: `git log 754a7fa^..HEAD -- internal/admin/dashboard` is empty and `git diff 754a7fa^ HEAD -- internal/admin/dashboard` shows 0 changed files.

**Design deviations (documented):**
1. **Task 1:** `TestInferenceRouting_TenantGuard_AllowsTenantHost` needed a valid model payload to actually reach `hostGuard` — malformed inference requests hit WorkflowResolution validation (400/404) before the route-level guard on the platform host. Inherent Echo ordering, same as the admin plane in P4; does not weaken isolation for valid requests. The test asserts only `!=404` (passes pre-fix too) as an over-blocking control — asserting 200 would pin behavior.
2. **Task 7:** the plan assumed BuildPipeline's call site in `gateway/inference_execute.go` — stale. The only production call is via the `guardrails.Catalog` interface used by the workflows compiler. Threaded ctx through `internal/workflows/` (`Compile` → `compileGuardrails` → `BuildPipeline`) and updated the `Catalog`/`Registry` signatures. Verified correct and complete.
3. **Task 8:** tagging has no background refresh loop — it never had one (only startup `RefreshAll` seeding is wired); guardrails' background loop lives in `factory.go`, not `service.go`. Both were handled as found rather than forcing the plan's uniform pattern.

**Deferred items for P6–P7 (triage):**

| Item | Source | Target |
|------|--------|--------|
| Quota middleware (Task 9): per-tenant budget check based on `core.GetTenantID(ctx)` + `budget.Service` | Task 9, optional per plan | P6 |
| ForTenant non-default writes don't refresh that tenant's live snapshot — tenant override affects live routing only after the next background RefreshAll tick (default interval up to 1h). Task 8 wires the active-tenant list into the tick (bounds staleness) but does NOT add write-through refresh. Consider write-through refresh after ForTenant writes, or accept tick latency explicitly | Task 6 IMPORTANT | P6 |
| RefreshAll full-map-swap semantics: each tick must pass the complete active-tenant list (tenants omitted are dropped); a transient `tenantSvc.List` error at tick falls back to `["default"]` and evicts all non-default tenants (spec-mandated availability tradeoff) | Task 8 | P6 |
| Data race window on `tenantSvc` pointer in app.go: getter closure read from background goroutines vs write at app.go:447 (benign ms window, comment acknowledges; fix = assign-before-factories or `atomic.Pointer`) | Task 8 | P6 |
| `workflows/compiler.go:79` — `c.registry.Len()` reads the DEFAULT tenant snapshot even when compiling a non-default tenant; if a tenant has guardrails while default has none, RefreshAll fails | Task 7 | P6 |
| `RefreshAll` must always include `"default"` in tenantIDs or the platform-admin surface goes empty | Task 6 | P6 |
| nil-ctx guards on `snapshotFor` / `PipelineForWorkflow` (unreachable in practice) | Tasks 3, 7 minors | P6+ |
| `ResolveRequestModel` wrapper hardcodes `context.Background()` (test-only callers; footgun) | Task 6 | P6+ |
| ForTenant P4 deferred alignment items not done in P5 (as scoped): nil-store guard, store-error unwrap, `CreatedAt` preservation, name trim/empty-name validation, managed-source/catalog-availability/snapshot validation, `SaveRulesForTenant` managed-header rejection + `Managed=false` stamping, guardrails `refreshWorkflowsAfterGuardrailChange`, `GetForTenant` selector literal-matching | P4 ledger Tasks 3–8 | P6+ |
| Task 2 minors: `Rules(ctx)`/`Disabled(ctx)` dropped nil-receiver guard; Refresh error string gained tenant suffix (cosmetic); `SuggestFailovers` keeps default-tenant `context.Background()` (admin-only path, tenant-scoping deferred) | Task 2 | P6+ |
| Task 3: `tenantID` field kept because P4 `tenant_admin.go` references `s.tenantID` (justified) | Task 3 | P6+ |
| Task 4 minors: transitional gap (non-default tenants resolved base pricing until Task 8 — plan-acknowledged, resolved); `Refresh(ctx)` contract subtly changed to `tenantIDFromContext` (all current callers consistent); no single-tenant `Refresh(ctx-with-tenant)` clone-swap test | Task 4 | P6+ (test gap) |
| Task 5 minors: `SaveRules` persists to `s.tenantID` while `Refresh(ctx)` could target non-default tenant if ctx carried one (latent, no current caller); 3 separate `atomic.Load`s per request in middleware (pre-existing, benign) | Task 5 | P6+ |
| Task 6: ForTenant default-tenant refresh derives tenant from ctx while write targets `s.tenantID` (implicit invariant) | Task 6 | P6+ |
| Task 7 minors: `registry==nil` branch in BuildPipeline now dead code; `SetExecutor` rebuilds all snapshots holding `s.mu` (rare admin op, plan-sanctioned); RefreshAll wraps build errors as `fmt.Errorf` vs Refresh's `guardrailServiceError` (cosmetic); test gaps: no `Refresh(ctx-with-tenant)` single-tenant test, no `SetExecutor` non-default snapshot test | Task 7 | P6+ (test gap) |
| Task 8: factories still do single-tenant `Refresh` at construction immediately before app-level `RefreshAll` seeding (N+1 store queries, pre-existing) | Task 8 | P6+ |
| Task 10 minors: dead `e *echo.Echo` field in `tenant_visibility_integration_test.go:50`; `gotTenantID`/`gotResolution` overwritten per-request (execution-order dependent, safe today); goal 2 doesn't exercise failover (no `RequestFailoverResolver` wired — core snapshot claim still fully verified via redirect resolution + default control) | Task 10 | P6+ (test hygiene) |
| Task 1: `TestInferenceRouting_TenantGuard_AllowsTenantHost` asserts only `!=404` (over-blocking control, passes pre-fix) | Task 1 | P6+ |

**Notable fix completing P4:** the inference-path `enforceTenantAndRole` now rejects tenant-scoped keys on unresolved hosts (`key_tenant_mismatch` 401) — it completes the P4 admin-path fix of the same `ctxTenantID==""` vulnerability. Any client relying on the old lenient behavior on unresolved hosts must be updated.

**Known tradeoffs (accepted, recorded from ledger):**
- RefreshAll full-map-swap semantics: each tick requires the complete active-tenant list; a transient `tenants.Service.List` error at tick evicts all non-default tenants (spec-mandated `["default"]` fallback + full swap — availability over completeness).
- Each config service still does a single-tenant `Refresh` at factory construction immediately before app-level `RefreshAll` seeding (N+1 store queries, pre-existing).
- ForTenant non-default writes affect live routing only after the next background RefreshAll tick (default interval up to 1h) — the suggested P6 improvement is write-through refresh after ForTenant writes.
