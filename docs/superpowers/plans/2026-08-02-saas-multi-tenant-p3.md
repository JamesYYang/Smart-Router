# P3 — Store 隔离 (Store Isolation) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every persistent store in SmartRouter tenant-aware: each store interface takes an explicit `tenantID` parameter, every query filters by `tenant_id`, config-type stores gain a `ListEffective` merge (platform `default` tenant + tenant override, tenant wins), and isolation tests prove no cross-tenant leakage.

**Architecture:** Shared-DB + `tenant_id` column model (design §1/§4). Store interfaces take `tenantID` as an explicit parameter (NOT context-scoping — design §5). Data stores scope rows by `tenant_id = ?`; config stores (virtual_models, failover_rules, guardrails, workflows, pricing_overrides, tagging_rules) store platform defaults under the reserved `default` tenant and tenant overrides under the tenant id, merged by a `ListEffective(ctx, tenantID)` method with tenant priority. MongoDB backends add the `tenant_id` field and filter by it. The in-memory conversation store keys by `(tenantID, id)`; the response cache mixes `tenantID` into its cache/vector key hash.

**Tech Stack:** Go, `database/sql` (SQLite via `modernc.org/sqlite`, PostgreSQL via `pgxpool`), MongoDB driver, `testify` (`require`/`assert`), in-memory `:memory:` SQLite for tests.

## Global Constraints

- **Execute directly on `master`** (user decision for P1–P7: no feature branch / worktree; keep local, do not push unless asked).
- **Explicit `tenantID` parameter** on every Store method (design §5) — never read tenant from context inside a store. Context only carries it to callers.
- **Sentinel deviation (documented):** config-type platform defaults are stored with `tenant_id = 'default'` (the reserved bootstrap tenant), NOT `NULL`. Rationale: PostgreSQL PK columns cannot be NULL, so the spec's `NULL = platform default` is not portable; `default` sentinel preserves identical `ListEffective` semantics cross-DB. Data stores also use `tenant_id TEXT NOT NULL DEFAULT 'default'`.
- **Backends are three:** SQLite (`modernc.org/sqlite`), PostgreSQL (`pgxpool`), MongoDB. Every interface change MUST be reflected in all three backend implementations or the build breaks.
- **PostgreSQL placeholders are positional `$1..$N`.** Adding `tenant_id` as a parameter SHIFTS every subsequent `$n`. Re-number all placeholders in PG backends when editing a query.
- **Migration pattern (copy authkeys):** each store's constructor runs idempotent `ALTER TABLE ... ADD COLUMN tenant_id TEXT DEFAULT 'default'` (SQLite) / `ADD COLUMN IF NOT EXISTS tenant_id TEXT DEFAULT 'default'` (PG). For Mongo, no DDL — filter on the field. Use each store's existing duplicate-column guard (e.g. `isSQLiteDuplicateColumnError`) so re-running is safe.
- **Primary key changes:** config stores change PK to `(tenant_id, <businessKey>)` (e.g. `(tenant_id, source)`, `(tenant_id, selector)`, `(tenant_id, key)`) so a platform-default row (`default`, x) and a tenant row (`t1`, x) can co-exist. Data stores with a composite business key (budgets `(user_path, period_seconds)`) extend it to `(tenant_id, user_path, period_seconds)`.
- **Isolation test contract (design §11.1):** for every store, an isolation test inserts rows for tenant `A` and tenant `B`, then asserts `List`/`Get`/reader calls scoped to `A` return only `A`'s rows (and vice-versa). This is the core cross-tenant-leak defense — every store task MUST add at least one such test.
- **Test conventions:** blank import `_ "modernc.org/sqlite"`; `sql.Open("sqlite", ":memory:")` + `db.SetMaxOpenConns(1)` (storage serializes on one conn); `t.Cleanup`; `testify/require` in new files; one `_test.go` per source file, same package, same base name.
- **Do not push.** Commit to local master only.
- **Seed/idempotency:** config seed paths (`app.go` `UpsertDefinitions`, `seedConfiguredBudgets`, `SetConfigModels`, `ConfigRules`, `EnsureManagedDefaultGlobal`) must stamp `tenant_id = 'default'` so platform defaults land under the `default` tenant.
- **`default` tenant is reserved** (bootstrapped in P1) — no real tenant may use id `default`.

---

## Scope / File Structure

Stores touched (12). Each gets its own task:

| Task | Package | Kind | Backends |
|---|---|---|---|
| 1 | `internal/authkeys` | data (cols exist, interface+filter new) | sqlite/pg/mongo |
| 2 | `internal/usage` | data | sqlite/pg/mongo |
| 3 | `internal/auditlog` | data (2 tables) | sqlite/pg/mongo |
| 4 | `internal/budget` | data (cross-reads usage) | sqlite/pg/mongo |
| 5 | `internal/conversationstore` | memory (keyed by tenant) | memory |
| 6 | `internal/responsecache` | key-based (hash tenant in) | n/a (unit test) |
| 7 | `internal/virtualmodels` | config (ListEffective) | sqlite/pg/mongo |
| 8 | `internal/failover` | config (ListEffective) | sqlite/pg/mongo |
| 9 | `internal/guardrails` | config (ListEffective) | sqlite/pg/mongo |
| 10 | `internal/workflows` | config (ListEffective + managed_default) | sqlite/pg/mongo |
| 11 | `internal/pricingoverrides` | config (ListEffective) | sqlite/pg/mongo |
| 12 | `internal/tagging` | config (ListEffective, KV table) | sqlite/pg/mongo |
| 13 | (final) | build/integration verification + completion notes | n/a |

Cross-cutting helpers reused: `internal/storage/sqlutil.BuildWhereClause` (joins conditions with ` AND `), `ClampLimitOffset`. The `core.TenantID` context helper (`internal/core/context.go`) already exists (P1) and is the contract callers use to obtain `tenantID` before calling stores.

---

### Task 1: `internal/authkeys` Store isolation

**Files:**
- Modify: `internal/authkeys/store.go:36-43` (interface), `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `service.go`
- Test: `internal/authkeys/store_sqlite_test.go`, `store_postgresql_test.go`, `store_mongodb_test.go`, `service_test.go`

**Interfaces (new signatures — explicit `tenantID` first arg for scoped methods):**
```go
type Store interface {
    List(ctx context.Context, tenantID string) ([]AuthKey, error)
    Create(ctx context.Context, tenantID string, key AuthKey) error
    UpdateLabels(ctx context.Context, tenantID, id string, labels []string, now time.Time) error
    Deactivate(ctx context.Context, tenantID, id string, now time.Time) error
    Close() error
}
```
(Columns `tenant_id`/`is_tenant_admin` already exist from P2 — do NOT re-add. Only add `WHERE tenant_id = ?` to `List`/`Get`-style queries and bind `tenantID` on writes.)

- [ ] **Step 1: Write failing isolation test**
```go
func TestSQLiteStore_Isolation(t *testing.T) {
    s := newTestSQLiteStore(t)
    require.NoError(t, s.Create(context.Background(), "A", AuthKey{ID: "k1", KeyHash: "h1", TenantID: "A"}))
    require.NoError(t, s.Create(context.Background(), "B", AuthKey{ID: "k2", KeyHash: "h2", TenantID: "B"}))
    gotA, err := s.List(context.Background(), "A")
    require.NoError(t, err)
    require.Len(t, gotA, 1)
    require.Equal(t, "k1", gotA[0].ID)
    gotB, err := s.List(context.Background(), "B")
    require.NoError(t, err)
    require.Len(t, gotB, 1)
    require.Equal(t, "k2", gotB[0].ID)
}
```
- [ ] **Step 2: Run test — FAIL** (`List` has wrong arity / returns both). `go test ./internal/authkeys/ -run TestSQLiteStore_Isolation -v`
- [ ] **Step 3: Implement** — change interface, update `List`/`Create`/`UpdateLabels`/`Deactivate` in all three backends to accept `tenantID` and bind `WHERE tenant_id = ?` / `tenant_id = $1` / `bson.M{"tenant_id": tenantID}`. Update `service.go` callers (it has `tenantID` via `AuthKey.TenantID` / context). Update `factory.go` and any `createStore` wiring only if signatures changed (they didn't for construction).
- [ ] **Step 4: Run tests** `go test ./internal/authkeys/...` — PASS. Also build whole module to catch callers: `go build ./...`
- [ ] **Step 5: Commit** `git commit -m "feat(authkeys): scope Store by explicit tenantID; add isolation test"`

---

### Task 2: `internal/usage` Store isolation

**Files:**
- Modify: `internal/usage/usage.go:12` (`UsageStore`), `reader.go:348` (`UsageReader`), `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `reader_sqlite.go`, `reader_postgresql.go`, `reader_mongodb.go`, `recalculate_pricing_*.go`
- Test: `internal/usage/store_sqlite_test.go` (new) + extend `reader_sqlite_boundary_test.go`

**Interfaces:**
```go
type UsageStore interface {
    WriteBatch(ctx context.Context, tenantID string, entries []*UsageEntry) error
    Flush(ctx context.Context) error
    Close() error
}
// UsageReader: every read method gains `tenantID string` as first param, e.g.
GetSummary(ctx context.Context, tenantID string, params UsageQueryParams) (*UsageSummary, error)
// (repeat for all 10 reader methods)
```
Add `TenantID string` to `UsageEntry` (`usage.go:38`).

- [ ] **Step 1: Write failing isolation test** (in new `store_sqlite_test.go`)
```go
func TestUsageSQLite_Isolation(t *testing.T) {
    db := openTestSQLite(t)
    s, _ := NewSQLiteStore(db, 0)
    e := &UsageEntry{TenantID: "A", Model: "m", RequestID: "r1"}
    require.NoError(t, s.WriteBatch(context.Background(), "A", []*UsageEntry{e}))
    params := UsageQueryParams{ /* empty */ }
    sum, err := s.GetSummary(context.Background(), "B", params)
    require.NoError(t, err)
    require.Zero(t, sum.TotalRequests) // B sees nothing
}
```
- [ ] **Step 2: Run — FAIL** (arity / cross-tenant returns A's row).
- [ ] **Step 3: Implement** — add `tenant_id TEXT DEFAULT 'default'` to `usage` CREATE/ALTER (sqlite `store_sqlite.go:66-76`; pg `store_postgresql.go:58`); bind `tenantID` on `WriteBatch` insert and on every reader `WHERE` via `BuildWhereClause` (append `"tenant_id = ?"`/`$N`). Update Mongo `NewMongoDBStore` writes/filters `tenant_id`. Update `recalculate_pricing_*` to scope by tenant. Update all callers in `internal/app`, `internal/server`, `internal/usage` service.
- [ ] **Step 4: Run** `go test ./internal/usage/...` and `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(usage): scope UsageStore/Reader by tenantID; isolation test"`

---

### Task 3: `internal/auditlog` Store isolation

**Files:**
- Modify: `internal/auditlog/auditlog.go:19` (`LogStore`), `reader.go:45` (`Reader`), `store_sqlite.go` (two tables `audit_logs`,`audit_log_attempts`), `store_postgresql.go`, `store_mongodb.go`, `reader_sqlite.go`, `reader_postgresql.go`, `reader_mongodb.go`
- Test: `internal/auditlog/store_sqlite_test.go` (extend) + isolation test

**Interfaces:**
```go
type LogStore interface {
    WriteBatch(ctx context.Context, tenantID string, entries []*LogEntry) error
    Flush(ctx context.Context) error
    Close() error
}
// Reader: GetLogs/GetLogByID/GetConversation gain `tenantID string` first param.
```
Add `TenantID string` to `LogEntry`.

- [ ] **Step 1: Write failing isolation test** — insert A and B log entries, assert `GetLogs(ctx,"A",...)` returns only A, and `GetLogByID` for B's id under tenant A returns not-found/empty.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — add `tenant_id TEXT DEFAULT 'default'` to both tables (sqlite ALTER slice `store_sqlite.go:98-117`; pg `store_postgresql.go:55,83`); bind on write + every reader `WHERE`; Mongo filter. Update `reader_factory.go` and callers (`internal/server`, `internal/admin`).
- [ ] **Step 4: Run** `go test ./internal/auditlog/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(auditlog): scope LogStore/Reader by tenantID; isolation test"`

---

### Task 4: `internal/budget` Store isolation

**Files:**
- Modify: `internal/budget/store.go:14-25` (`Store`), `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `service.go`
- Test: `internal/budget/store_sqlite_test.go` (extend with isolation)

**Interface (every method gains `tenantID` first arg):**
```go
type Store interface {
    ListBudgets(ctx context.Context, tenantID string) ([]Budget, error)
    UpsertBudgets(ctx context.Context, tenantID string, budgets []Budget) error
    DeleteBudget(ctx context.Context, tenantID, userPath string, periodSeconds int64) error
    ReplaceConfigBudgets(ctx context.Context, tenantID string, budgets []Budget) error
    GetSettings(ctx context.Context, tenantID string) (Settings, error)
    SaveSettings(ctx context.Context, tenantID string, settings Settings) (Settings, error)
    ResetBudget(ctx context.Context, tenantID, userPath string, periodSeconds int64, at time.Time) error
    ResetAllBudgets(ctx context.Context, tenantID string, at time.Time) error
    SumUsageCost(ctx context.Context, tenantID, userPath string, start, end time.Time) (float64, bool, error)
    Close() error
}
```
- [ ] **Step 1: Write failing isolation test** — `UpsertBudgets(ctx,"A",...)`, `ListBudgets(ctx,"B")` returns empty.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — change PK `(user_path, period_seconds)` → `(tenant_id, user_path, period_seconds)` in sqlite CREATE `store_sqlite.go:34-43` and pg `store_postgresql.go:25`; add `tenant_id` column + index; bind `tenantID` on all queries. CRITICAL: `SumUsageCost` reads the `usage` table cross-package — add `AND tenant_id = ?`/`$N` matching the `usage` table's tenant_id (keep in lockstep with Task 2's usage schema). Update `budget_settings` table too (add `tenant_id`, scope by it). Update `service.go`, `factory.go` `seedConfiguredBudgets` (stamp `tenant_id='default'`).
- [ ] **Step 4: Run** `go test ./internal/budget/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(budget): scope Store by tenantID (incl. usage cross-read); isolation test"`

---

### Task 5: `internal/conversationstore` tenant scoping (memory)

**Files:**
- Modify: `internal/conversationstore/store.go:22-42` (struct + interface), `store_memory.go`
- Test: `internal/conversationstore/store_memory_test.go`

**Interface + struct:**
```go
type StoredConversation struct {
    Conversation interface{} `json:"conversation"`
    Items        []json.RawMessage `json:"items"`
    UserPath     string `json:"user_path"`
    RequestID    string `json:"request_id"`
    TenantID     string `json:"tenant_id"` // NEW
    StoredAt     time.Time `json:"stored_at"`
    ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}
type Store interface {
    Create(ctx context.Context, tenantID string, c *StoredConversation) error
    Get(ctx context.Context, tenantID, id string) (*StoredConversation, error)
    Update(ctx context.Context, tenantID string, c *StoredConversation) error
    AppendItems(ctx context.Context, tenantID, id string, items []json.RawMessage) error
    Delete(ctx context.Context, tenantID, id string) error
    Close() error
}
```
(Deferred: SQL/Mongo backend — memory keying satisfies isolation for single-instance MVP; record as deferred minor in Completion Notes. SQL-ification tracked for later phase.)

- [ ] **Step 1: Write failing isolation test** — `Create(ctx,"A", &StoredConversation{TenantID:"A", ...})`; `Get(ctx,"B", id)` returns nil/error; `Get(ctx,"A", id)` returns it.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — in `store_memory.go` change the internal map to `map[string]map[string]*StoredConversation` keyed by `(tenantID, id)` (or a `tenantID+"/"+id` composite key). Every method takes `tenantID` and scopes the map. Update `internal/server/handlers.go:109` construction call to pass `core.GetTenantID(ctx)` at call sites (server passes tenant from context into these calls).
- [ ] **Step 4: Run** `go test ./internal/conversationstore/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(conversationstore): scope memory store by tenantID"`

---

### Task 6: `internal/responsecache` tenant keying

**Files:**
- Modify: `internal/responsecache/simple.go:196` (`hashRequest`), `semantic.go:145,192` (paramsHash / cacheKey), `stream_cache.go:29`
- Test: `internal/responsecache/exact_cache_test.go` (extend) + new `tenant_key_test.go`

**Approach:** no interface change (key-value). Mix `core.GetTenantID(ctx)` into the cache key hash so two tenants with identical request bodies get separate cache entries (prevents cross-tenant response leakage).

- [ ] **Step 1: Write failing test** — build two contexts with different `tenantID` via `core.WithTenantID`, call `hashRequest`/key builder, assert hashes differ; assert `core.GetTenantID(ctx) == ""` case still hashes (back-compat, single default).
- [ ] **Step 2: Run — FAIL** (hashes identical).
- [ ] **Step 3: Implement** — in `hashRequest`, prepend/append `core.GetTenantID(ctx)` (or `"default"` when empty) to the hashed bytes before SHA-256. Same for `semantic.go` paramsHash and vec cacheKey and `stream_cache.go` key. Guard: when `tenantID == ""` use `"default"` so existing single-tenant behavior unchanged.
- [ ] **Step 4: Run** `go test ./internal/responsecache/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(responsecache): isolate cache/vector keys by tenantID"`

---

### Task 7: `internal/virtualmodels` Store isolation + ListEffective

**Files:**
- Modify: `internal/virtualmodels/store.go:16-22`, `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `service.go`
- Test: `internal/virtualmodels/store_test.go` (extend) + `internal/virtualmodels/config_overlay_test.go` (extend for ListEffective)

**Interface:**
```go
type Store interface {
    List(ctx context.Context, tenantID string) ([]VirtualModel, error)
    ListEffective(ctx context.Context, tenantID string) ([]VirtualModel, error) // NEW: default + tenant merge
    Get(ctx context.Context, tenantID, source string) (*VirtualModel, error)
    Upsert(ctx context.Context, tenantID string, vm VirtualModel) error
    Delete(ctx context.Context, tenantID, source string) error
    Close() error
}
```
- [ ] **Step 1: Write failing tests** — (a) isolation: upsert `source="s"` under A and B, `List(ctx,"A")` returns only A; (b) ListEffective: seed platform default `("default","s")` + tenant `("A","s")`; `ListEffective(ctx,"A")` returns A's row (tenant wins); `ListEffective(ctx,"B")` returns default's row.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — PK `(source)` → `(tenant_id, source)` in sqlite CREATE `store_sqlite.go:24-35` + pg `store_postgresql.go:29`; add `tenant_id TEXT DEFAULT 'default'` column + ALTER slice (none existed — add one). Bind `tenantID` on all queries. Implement `ListEffective`: query `WHERE tenant_id IN ('default', ?)` ordered so tenant rows override platform rows of the same `source` (collect into map keyed by `source`, tenant value wins). Mongo: filter `tenant_id` in `$in`. Update `service.go` `refreshLocked`/`SetConfigModels` to stamp `tenant_id='default'` on config models and call `ListEffective`. Update `factory.go`.
- [ ] **Step 4: Run** `go test ./internal/virtualmodels/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(virtualmodels): tenantID scope + ListEffective merge"`

---

### Task 8: `internal/failover` Store isolation + ListEffective

**Files:**
- Modify: `internal/failover/store.go:16-23`, `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `service.go`
- Test: `internal/failover/store_sqlite_test.go` (extend) + `service_test.go`

**Interface:** same shape as virtualmodels — `List(ctx, tenantID)`, `ListEffective(ctx, tenantID)`, `Get(ctx, tenantID, source)`, `Upsert(ctx, tenantID, rule)`, `Delete(ctx, tenantID, source)`, `DeleteAll(ctx, tenantID)`, `Close()`.

- [ ] **Step 1: Write failing isolation + ListEffective tests** (mirror Task 7: A/B isolation; default+tenant merge with tenant priority).
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — PK `(primary_model)` → `(tenant_id, primary_model)` in sqlite CREATE `store_sqlite.go:21-28` (note existing `migrateSQLiteFailoverRules` table-rebuild at `:46` — extend it to add `tenant_id`); pg `store_postgresql.go:26`; add column + ALTER. Keep `managed_source` semantics (config rows stamped `tenant_id='default'`). Implement `ListEffective` (tenant overrides platform default by `primary_model`). Update `service.go` `mergeConfig`/seed to stamp `tenant_id='default'`; `factory.go`.
- [ ] **Step 4: Run** `go test ./internal/failover/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(failover): tenantID scope + ListEffective merge"`

---

### Task 9: `internal/guardrails` Store isolation + ListEffective

**Files:**
- Modify: `internal/guardrails/store.go:28-35`, `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `service.go`
- Test: `internal/guardrails/store_sqlite_test.go` (extend) + `service_test.go`

**Interface:** `List(ctx, tenantID)`, `ListEffective(ctx, tenantID)`, `Get(ctx, tenantID, name)`, `Upsert(ctx, tenantID, def)`, `UpsertMany(ctx, tenantID, defs)`, `Delete(ctx, tenantID, name)`, `Close()`.

- [ ] **Step 1: Write failing isolation + ListEffective tests** (guardrails has NO managed flag today — so "platform default" = rows seeded at startup; seed path `app.go:351 UpsertDefinitions` must stamp `tenant_id='default'`). Test: seed default `("default","g")` + tenant `("A","g")`; `ListEffective(ctx,"A")` returns A's.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — PK `(name)` → `(tenant_id, name)` in sqlite CREATE `store_sqlite.go:26-39` (generalize the duplicate-column guard at `:42-44` so a second ALTER for `tenant_id` is tolerated); pg `store_postgresql.go:29`; add `tenant_id` column + ALTER. Implement `ListEffective`. Update `service.go` `UpsertDefinitions` to stamp `tenant_id='default'` (config seed) and read via `ListEffective`. Update `factory.go`.
- [ ] **Step 4: Run** `go test ./internal/guardrails/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(guardrails): tenantID scope + ListEffective merge"`

---

### Task 10: `internal/workflows` Store isolation + ListEffective

**Files:**
- Modify: `internal/workflows/store.go:26-33`, `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `service.go`
- Test: `internal/workflows/store_sqlite_test.go` (extend) + `service_test.go`

**Interface:** `ListActive(ctx, tenantID)`, `ListEffective(ctx, tenantID)`, `Get(ctx, tenantID, id)`, `Create(ctx, tenantID, input)`, `EnsureManagedDefaultGlobal(ctx, tenantID, input, hash)`, `Deactivate(ctx, tenantID, id)`, `Close()`.

- [ ] **Step 1: Write failing isolation + ListEffective tests** — note `managed_default` column (`store_sqlite.go:36`); the global default must become `tenant_id='default'` + `managed_default=1`. Test: `EnsureManagedDefaultGlobal` seeds under `default`; a tenant override `("A", name)` wins in `ListEffective`.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — add `tenant_id TEXT DEFAULT 'default'` column; extend the two UNIQUE indexes (`store_sqlite.go:44,46`) to include `tenant_id` (and PK concept); pg `store_postgresql.go:33`. Implement `ListEffective` (tenant overrides default by name/scope). Update `service.go` `EnsureDefaultGlobal` to stamp `tenant_id='default'`; `factory.go` + `app.go:375`.
- [ ] **Step 4: Run** `go test ./internal/workflows/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(workflows): tenantID scope + ListEffective merge"`

---

### Task 11: `internal/pricingoverrides` Store isolation + ListEffective

**Files:**
- Modify: `internal/pricingoverrides/store.go:30-35`, `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `service.go`
- Test: `internal/pricingoverrides/store_sqlite_test.go` (extend) + `service_test.go`

**Interface:** `List(ctx, tenantID)`, `ListEffective(ctx, tenantID)`, `Upsert(ctx, tenantID, override)`, `Delete(ctx, tenantID, selector)`, `Close()`.

- [ ] **Step 1: Write failing isolation + ListEffective tests** — `(tenant_id, selector)` PK; default `("default", sel)` + tenant `("A", sel)`; `ListEffective(ctx,"A")` returns A's; `List(ctx,"B")` returns only B.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — PK `(selector)` → `(tenant_id, selector)` in sqlite CREATE `store_sqlite.go:25-32`; change `ON CONFLICT(selector)` → `ON CONFLICT(tenant_id, selector)` in `Upsert` (`:79`); three index statements (`:37,40,43`) include `tenant_id`; pg `store_postgresql.go:29`; add `tenant_id` column + ALTER slice (none existed — add). Implement `ListEffective`. Update `service.go` snapshot read to use `ListEffective`; `factory.go` + `app.go:314`.
- [ ] **Step 4: Run** `go test ./internal/pricingoverrides/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(pricingoverrides): tenantID scope + ListEffective merge"`

---

### Task 12: `internal/tagging` Store isolation + ListEffective

**Files:**
- Modify: `internal/tagging/store.go:7-16`, `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`, `service.go`
- Test: `internal/tagging/store_sqlite_test.go` (NEW — none existed) + `tagging_test.go` (extend)

**Interface:**
```go
type Store interface {
    GetRules(ctx context.Context, tenantID string) ([]Rule, error)
    SaveRules(ctx context.Context, tenantID string, rules []Rule) error
    ListEffectiveRules(ctx context.Context, tenantID string) ([]Rule, error) // NEW
    Close() error
}
```
Table `tagging_settings` PK `(key)` → `(tenant_id, key)`; whole-rule-set JSON blob stored per `(tenant_id, key)`.

- [ ] **Step 1: Write failing isolation + ListEffective tests** — SaveRules under A and B with different rule sets; GetRules(ctx,"A") returns only A's; ListEffectiveRules(ctx,"B") merges `default` config rules + B's stored rules (tenant stored wins by header key).
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — change CREATE `store_sqlite.go:24-28` PK to `(tenant_id, key)`, add `tenant_id` column, update `ON CONFLICT(key)` → `ON CONFLICT(tenant_id, key)` in `SaveRules` (`:52`); pg `store_postgresql.go:24`; Mongo `tagging_settings` keyed by `(tenant_id, key)`. Implement `ListEffectiveRules`: merge config rules (already in `service.go` `configRules`, stamped `tenant_id='default'`) with stored tenant rules. Update `service.go` `Refresh`/`Rules`/`SaveRules` to scope by `tenantID`; `factory.go` `ConfigRules` stamps `tenant_id='default'`.
- [ ] **Step 4: Run** `go test ./internal/tagging/...` + `go build ./...` — PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(tagging): tenantID scope + ListEffectiveRules merge"`

---

### Task 13: Verification + Completion Notes

**Files:**
- Modify: none (verification only)
- Create: append `Completion Notes` + `Deferred items for P4-P7` section to this plan file.

- [ ] **Step 1: Full build** `go build ./...` — must pass with no leftover callers missing `tenantID`.
- [ ] **Step 2: Full test suite** `go test ./...` — all pass. (Note any panics from `:memory:` SQLite concurrency; ensure `SetMaxOpenConns(1)` in new tests.)
- [ ] **Step 3: `go vet ./...`** — clean.
- [ ] **Step 4: Dashboard JS tests** `node --test internal/admin/dashboard/static/js/modules/*.test.cjs` — pass (no store contract broken).
- [ ] **Step 5: Append Completion Notes** to this plan: commit range, must-fixes applied, deferred items list (conversationstore SQL backend; any PG/Mongo test gaps; config seed idempotency under tenant), and a triage table pointing which deferred items belong to P4 (admin handler split) / P5+ (routing/host guards) / later.
- [ ] **Step 6: Commit** `git commit -m "docs(plan): append P3 completion notes and deferred-items triage for P4-P7"` (amend notes file only).

---

## Self-Review (against spec §4, §5, §9, §11)

- [x] Every business table gains `tenant_id` (usage/audit/budget/conversations/response_cache/virtual_models/failover/guardrails/workflows/pricing_overrides/tagging/auth_keys). ✓ mapped to Tasks 1–12.
- [x] Store interfaces take explicit `tenantID` (design §5). ✓ every task.
- [x] config stores get `ListEffective` platform-default + tenant-override merge. ✓ Tasks 7–12.
- [x] Isolation tests per store (design §11.1). ✓ every task.
- [x] In-memory conversation store keyed by tenant (design §9 acceptable single-instance). ✓ Task 5 (SQL deferred, noted).
- [x] response cache key isolation (design §9 cross-tenant leak). ✓ Task 6.
- [x] Mongo backends updated (build must pass). ✓ each task.
- [ ] PostgreSQL `$N` renumbering — implementer MUST verify per backend.
- [ ] `default` sentinel deviation from spec `NULL` — documented in Global Constraints; `ListEffective` semantics unchanged.

---

## P3 Completion Notes (2026-08-02)

**Status:** Complete. 12/12 stores transformed + full verification.

**Commits:** `d080768..acc3cc0` on master (19 commits).

**Verification:**
- `go build ./...` — PASS
- `go test ./...` — ALL 60+ packages PASS
- `go vet ./...` — only 3 pre-existing dup json tag warnings in core/

**Design deviations (documented):**
1. `default` sentinel instead of NULL for config stores
2. `tenantID == ""` = unscoped for data stores (platform admin cross-tenant reads)

**Deferred items for P4-P7:**
| Item | Target |
|------|--------|
| MongoDB ListEffective sort (a-* tenant IDs < "default") | P5+ |
| MongoDB tagging doc schema migration | P4 |
| Per-tenant Service wiring (budget/guardrails caches) | P4 |
| PG/Mongo isolation tests (need DB URLs) | P5 |
| authkeys.ListViews() cross-tenant | P4 |
| conversationstore SQL/MongoDB backends | P6+ |
| RecalculatePricing tenant scoping | P4 |
