# P6: 内存 Store DB 化 + Quota Middleware

> **For agentic workers:** Use subagent-driven-development to implement this plan task-by-task.
> Checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement SQLite/PostgreSQL/MongoDB backends for conversationstore and responsestore (with tenant isolation), and add per-tenant budget quota middleware.

**Architecture:** Follow the three-backend pattern established by `internal/budget/store_sqlite.go` / `store_postgresql.go` / `store_mongodb.go`, using `storage.ResolveBackend` for dispatch and a `factory.go` file for wiring.

**Tech Stack:** Go, SQLite (modernc.org/sqlite), PostgreSQL (pgx/v5), MongoDB (mongo-driver/v2), echo web framework.

## Global Constraints

- Follow existing store patterns: composite PK `(tenant_id, id)`, Unix epoch timestamps for SQL, native `time.Time` for MongoDB, `bson`+`json` struct tags.
- `conversationstore.Store` already has `tenantID` on all methods (P3). `responsestore.Store` needs `tenantID` added to methods and `TenantID` added to `StoredResponse`.
- Uses `storage.ResolveBackend` dispatch — each module gets its own `factory.go` or wired via `app.go`.
- All new store files follow the naming convention: `store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`.
- Main data is JSON BLOB (core.Conversation/Items and core.ResponsesResponse/InputItems serialized via `goccy/go-json`).

---

### Task 1: conversationstore SQL stores (SQLite + PostgreSQL)

**Files:**
- Create: `internal/conversationstore/store_sqlite.go`
- Create: `internal/conversationstore/store_postgresql.go`
- Modify: `internal/conversationstore/store_memory_test.go` (tenant isolation test for MemoryStore)

**Interfaces:**
- Consumes: `conversationstore.Store` interface (already defined in `store.go:33-40`), `StoredConversation` struct (already has `TenantID` field)
- Produces: `SQLiteStore` (with `*sql.DB`), `PostgreSQLStore` (with `*pgxpool.Pool`), both implementing `Store`

**Data model (SQL):**
```sql
CREATE TABLE IF NOT EXISTS conversations (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    id TEXT NOT NULL,
    conversation JSONB NOT NULL,         -- serialized core.Conversation
    items JSONB NOT NULL DEFAULT '[]',   -- serialized []json.RawMessage
    user_path TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    stored_at INTEGER NOT NULL,          -- Unix epoch seconds
    expires_at INTEGER NOT NULL,         -- Unix epoch seconds (0 = no expiry)
    PRIMARY KEY (tenant_id, id)
);
```

**Notes:**
- SQLite uses `TEXT` for JSON (no native JSONB), PostgreSQL uses `JSONB`.
- `AppendItems` reads existing items → JSON decode → append → JSON encode → UPDATE. Use a SELECT+UPDATE transaction for atomicity.
- `Update` replaces all fields (conversation, items, user_path, request_id, stored_at, expires_at) but preserves `stored_at` if zero and `expires_at` if zero (matching MemoryStore semantics in `store_memory.go:161-166`).
- Timestamps: store as Unix epoch seconds (`time.Unix(ns, 0)`) — consistent with budget/store_sqlite.go.
- `Close()` for SQL stores simply closes the underlying `*sql.DB` / `*pgxpool.Pool` (acquired ownership on construction).
- Include `ALTER TABLE ADD COLUMN IF NOT EXISTS tenant_id` migration for idempotent column addition.

- [ ] **Step 1: Write SQLite store**

Create `internal/conversationstore/store_sqlite.go`:
- `SQLiteStore` struct wrapping `*sql.DB`
- `NewSQLiteStore(db *sql.DB) (*SQLiteStore, error)` — runs CREATE TABLE IF NOT EXISTS + ALTER TABLE ADD COLUMN migrations
- Implement all 6 methods (`Create`, `Get`, `Update`, `AppendItems`, `Delete`, `Close`)
- `AppendItems` uses SELECT+JSON unmarshal/marshal+UPDATE in a transaction

- [ ] **Step 2: Write PostgreSQL store**

Create `internal/conversationstore/store_postgresql.go`:
- `PostgreSQLStore` struct wrapping `*pgxpool.Pool`
- `NewPostgreSQLStore(ctx context.Context, pool *pgxpool.Pool) (*PostgreSQLStore, error)`
- Uses `$n` placeholders instead of `?`
- PostgreSQL native `JSONB` type; timestamps still stored as `BIGINT` (Unix epoch seconds)
- Same semantics as SQLite store

- [ ] **Step 3: Add tenant isolation unit tests for MemoryStore**

Add to `internal/conversationstore/store_memory_test.go`:
- `TestMemoryStoreTenantIsolation` — create for tenant-a, Get for tenant-b returns ErrNotFound
- `TestMemoryStoreAppendItemsTenantIsolation` — same for AppendItems
- `TestMemoryStoreUpdateTenantIsolation` — Update for tenant-b on tenant-a's conversation returns ErrNotFound
- `TestMemoryStoreDeleteTenantIsolation` — Delete for tenant-b on tenant-a's conversation returns ErrNotFound

- [ ] **Step 4: Run tests for conversationstore**

Run: `go test ./internal/conversationstore/... -v`
Expected: PASS (MemoryStore tests pass; SQL stores won't be tested yet — those need integration tests)

---

### Task 2: conversationstore MongoDB store

**Files:**
- Create: `internal/conversationstore/store_mongodb.go`

**Interfaces:**
- Consumes: `conversationstore.Store` interface
- Produces: `MongoDBStore` (with `*mongo.Collection`), implementing `Store`

**Data model (MongoDB):**
- Collection: `conversations`
- Document: `{ tenant_id, id, conversation: {...}, items: [...], user_path, request_id, stored_at: ISODate, expires_at: ISODate }`
- Unique compound index on `{ tenant_id: 1, id: 1 }`

**Notes:**
- MongoDB stores Go `time.Time` natively (not Unix epoch) — consistent with budget/store_mongodb.go.
- `AppendItems` uses `$push` operator on the items array (no read-modify-write needed for simple append).
- `Update` uses `$set` to replace all top-level fields.
- Fallback to standalone (no transaction) using `isMongoTransactionCapabilityError()` pattern from budget/store_mongodb.go.

- [ ] **Step 1: Write MongoDB store**

Create `internal/conversationstore/store_mongodb.go`:
- `MongoDBStore` struct wrapping `*mongo.Collection`
- `NewMongoDBStore(ctx context.Context, db *mongo.Database) (*MongoDBStore, error)` — creates unique compound index on `{tenant_id, id}`
- `Create`: `InsertOne` with duplicate-key → "already exists" error
- `Get`: `FindOne` with `{ tenant_id, id }`
- `Update`: `UpdateOne` with `$set` on all fields
- `AppendItems`: `UpdateOne` with `$push: { items: { $each: newItems } }`
- `Delete`: `DeleteOne`
- timestamps stored as native `time.Time` (add `bson` tags if not using shared struct)

- [ ] **Step 2: Verify build**

Run: `go build ./internal/conversationstore/...`
Expected: PASS

---

### Task 3: conversationstore wiring (factory.go + app.go)

**Files:**
- Create: `internal/conversationstore/factory.go`
- Modify: `internal/app/app.go` (wire conversationstore)

**Notes:**
- The conversationstore is currently wired at the server layer (`server/http.go`), not in `app.go`. The SQL stores need `storage.Storage` which is available in `app.go`. We add wiring in `app.go` to optionally create a DB-backed store and pass it into `serverCfg`.
- `factory.go` uses `storage.ResolveBackend[Store]` dispatch pattern (identical to budget/factory.go:98-105).
- Only create the DB-backed store when a shared storage is configured AND the feature is enabled.
- The existing MemoryStore remains as the default fallback (created in `handlers.go`). The `app.go` wiring simply overrides it when a DB store is available.

- [ ] **Step 1: Write factory.go**

Create `internal/conversationstore/factory.go`:
```go
package conversationstore

import (
    "context"
    "database/sql"
    "github.com/jackc/pgx/v5/pgxpool"
    "go.mongodb.org/mongo-driver/v2/mongo"
    "smartrouter/internal/storage"
)

func NewStore(ctx context.Context, store storage.Storage) (Store, error) {
    return storage.ResolveBackend[Store](
        store,
        func(db *sql.DB) (Store, error) { return NewSQLiteStore(db) },
        func(pool *pgxpool.Pool) (Store, error) { return NewPostgreSQLStore(ctx, pool) },
        func(db *mongo.Database) (Store, error) { return NewMongoDBStore(ctx, db) },
    )
}
```

- [ ] **Step 2: Wire in app.go**

In `internal/app/app.go`, after the response cache setup and before server config assembly:
- If `sharedStorage != nil` and `appCfg.Conversations.Enabled` (or always when using DB storage), call `conversationstore.NewStore(ctx, sharedStorage)`
- Set the result on `serverCfg.ConversationStore` (add this field to server config)

- [ ] **Step 3: Add ConversationStore to server.Config**

In `internal/server/http.go`, add `ConversationStore conversationstore.Store` to the Config struct and wire it like ResponseStore in `New()` (lines 180-181 pattern).

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: PASS (ensure no compilation errors from new wiring)

---

### Task 4: responsestore tenantID + SQL stores (interface change first)

**Files:**
- Modify: `internal/responsestore/store.go` (add TenantID to StoredResponse, add tenantID to Store interface)
- Modify: `internal/responsestore/store_memory.go` (update MemoryStore to match new interface, tenant-scoped map key)
- Modify: `internal/responsestore/store_memory_test.go` (add tenant isolation tests)
- Create: `internal/responsestore/store_sqlite.go`
- Create: `internal/responsestore/store_postgresql.go`
- Create: `internal/responsestore/store_mongodb.go`

**Interfaces:**
- Consumes: `core.ResponsesResponse` (from `internal/core`)
- Produces: `Store` interface with tenantID params, `memoryStoreKey(tenantID, id)` for MemoryStore key generation

**Changes to Store interface:**
```go
type Store interface {
    Create(ctx context.Context, tenantID string, response *StoredResponse) error
    Get(ctx context.Context, tenantID, id string) (*StoredResponse, error)
    Update(ctx context.Context, tenantID string, response *StoredResponse) error
    Delete(ctx context.Context, tenantID, id string) error
    Close() error
}
```

**Changes to StoredResponse:**
- Add `TenantID string `json:"tenant_id,omitempty"`` field

**Data model (SQL):**
```sql
CREATE TABLE IF NOT EXISTS stored_responses (
    tenant_id TEXT NOT NULL DEFAULT 'default',
    id TEXT NOT NULL,
    response JSONB NOT NULL,
    input_items JSONB NOT NULL DEFAULT '[]',
    provider TEXT NOT NULL DEFAULT '',
    provider_name TEXT NOT NULL DEFAULT '',
    provider_response_id TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    user_path TEXT NOT NULL DEFAULT '',
    workflow_version_id TEXT NOT NULL DEFAULT '',
    stored_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
```

**Notes:**
- Unlike conversationstore, responsestore has no `AppendItems` method — simpler CRUD.
- Callers that currently use `responseStore.Create(ctx, stored)` will need to add `core.GetTenantID(ctx)` as the second argument.
- Follow the same pattern as conversationstore for SQLite/PostgreSQL/MongoDB implementation.

- [ ] **Step 1: Update Store interface and StoredResponse**

- Add `TenantID string` field to `StoredResponse` struct
- Add `tenantID string` parameter to all 4 Store methods (Create, Get, Update, Delete)
- Update `cloneResponse` to preserve TenantID
- Update `normalizeStoredResponse` to handle TenantID

- [ ] **Step 2: Update MemoryStore**

- Change key function to `memoryStoreKey(tenantID, id)` = `tenantID + "/" + id` (same as conversationstore)
- Update all 4 methods to accept and use tenantID
- Set `c.TenantID = tenantID` in Create

- [ ] **Step 3: Update all callers**

Files to update (search for `responseStore.` calls):
- `internal/server/native_response_service.go`: add `core.GetTenantID(ctx)` to all 4 calls (Create, Get, Update, Delete)
- `internal/server/translated_inference_service.go`: add `core.GetTenantID(ctx)` to Create/Update calls
- `internal/server/handlers_test.go`: update `failingResponseStore` mock to match new interface, update test setup calls
- `internal/server/native_response_service_test.go`: update test setup calls

- [ ] **Step 4: Write SQLite store**

Create `internal/responsestore/store_sqlite.go`:
- `SQLiteStore` struct wrapping `*sql.DB`
- `NewSQLiteStore(db *sql.DB) (*SQLiteStore, error)`
- CREATE TABLE with all columns + ALTER TABLE migrations for tenant_id
- Simple CRUD with `ON CONFLICT(tenant_id, id)` for upsert or explicit Create/Update

- [ ] **Step 5: Write PostgreSQL store**

Create `internal/responsestore/store_postgresql.go`:
- Same structure as SQLite but using `$n` placeholders and `*pgxpool.Pool`
- `JSONB` type for response/input_items columns; `BIGINT` for timestamps

- [ ] **Step 6: Write MongoDB store**

Create `internal/responsestore/store_mongodb.go`:
- `MongoDBStore` wrapping `*mongo.Collection`
- Unique compound index on `{ tenant_id: 1, id: 1 }`
- Standard CRUD with BSON filters
- Native time.Time storage

- [ ] **Step 7: Add tenant isolation tests for MemoryStore**

Add to `internal/responsestore/store_memory_test.go`:
- `TestMemoryStoreTenantIsolation` — Create for tenant-a, Get for tenant-b returns ErrNotFound
- `TestMemoryStoreUpdateTenantIsolation` — Update for tenant-b on tenant-a's response returns ErrNotFound
- `TestMemoryStoreDeleteTenantIsolation` — Delete for tenant-b on tenant-a's response returns ErrNotFound

- [ ] **Step 8: Run build and tests**

Run: `go build ./...` then `go test ./internal/responsestore/... -v`
Expected: PASS

---

### Task 5: responsestore wiring (factory.go + app.go)

**Files:**
- Create: `internal/responsestore/factory.go`
- Modify: `internal/app/app.go` (wire responsestore)
- Modify: `internal/server/http.go` (confirm ResponseStore config field pattern; already exists at line 74)

**Notes:**
- ResponseStore is already wired in `http.go` via `config.ResponseStore` and `handler.SetResponseStore()`.
- The factory and app.go wiring follows the same pattern as conversationstore.
- Add `ResponseStore` config field to `app.go`'s server config assembly (if not already present).

- [ ] **Step 1: Write factory.go**

```go
package responsestore

import (
    "context"
    "database/sql"
    "github.com/jackc/pgx/v5/pgxpool"
    "go.mongodb.org/mongo-driver/v2/mongo"
    "smartrouter/internal/storage"
)

func NewStore(ctx context.Context, store storage.Storage) (Store, error) {
    return storage.ResolveBackend[Store](
        store,
        func(db *sql.DB) (Store, error) { return NewSQLiteStore(db) },
        func(pool *pgxpool.Pool) (Store, error) { return NewPostgreSQLStore(ctx, pool) },
        func(db *mongo.Database) (Store, error) { return NewMongoDBStore(ctx, db) },
    )
}
```

- [ ] **Step 2: Wire in app.go**

In `internal/app/app.go`: if sharedStorage != nil, create `responsestore.NewStore(ctx, sharedStorage)` and set it on `serverCfg.ResponseStore`.

- [ ] **Step 3: Verify build**

Run: `go build ./...`
Expected: PASS

---

### Task 6: Quota Middleware (tenant-level budget enforcement)

**Files:**
- Create: `internal/server/tenant_quota.go`
- Modify: `internal/server/http.go` (add to middleware chain)
- Modify: `internal/budget/service.go` (add `CheckTenant` method)

**Interfaces:**
- Consumes: `budget.Service` (from `internal/budget`), `core.GetTenantID(ctx)`
- Produces: `echo.MiddlewareFunc` for mounting in echo middleware chain

**Design:**
- `budget.Service.Check(ctx, userPath, now)` already checks per-user-path budgets for the current tenant (derived from context via `tenantID(ctx)`).
- For a per-tenant aggregate budget, we use a **convention-based userPath `"*"`** (asterisk). When a budget is created with `user_path = "*"`, it represents a tenant-wide budget.
- The existing `SumUsageCost` already sums by `userPath` — with userPath `"*"` we need to handle it as "all user paths". We add a special case in `SumUsageCost`: when `userPath == "*"`, the query omits the userPath filter, summing ALL usage for that tenant.
- The `CheckTenant` method just calls `Check(ctx, "*", now)` — simple delegate.

**TenantQuotaMiddleware:**
- Factory: `func TenantQuotaMiddleware(svc *budget.Service) echo.MiddlewareFunc`
- Skip if `core.GetTenantID(ctx) == ""` (platform host has no budget)
- Call `svc.Check(ctx, "*", time.Now().UTC())`
- On `budget.ExceededError`: return 402 JSON with `{"error":{"type":"tenant_budget_exceeded","details":...}}`
- On other errors: log + allow (don't break inference for transient budget-check failures)

**Placement in middleware chain:** After auth, before inference handler. Insert between `WorkflowResolution` and the route handlers (or as the first Global middleware after auth in the echo group chain).

- [ ] **Step 1: Add CheckTenant to budget.Service**

In `internal/budget/service.go`:
```go
// CheckTenant checks if the current tenant has exceeded any budget.
// It uses a tenant-level budget identified by userPath = "*".
func (s *Service) CheckTenant(ctx context.Context, now time.Time) error {
    return s.Check(ctx, "*", now)
}
```

- [ ] **Step 2: Handle "*" userPath in SumUsageCost**

In all three budget store backends (`store_sqlite.go`, `store_postgresql.go`, `store_mongodb.go`):
- When `userPath == "*"`, skip the userPath filter clause — sum ALL usage for this tenant regardless of userPath.
- Add a comment: `// "*" means tenant-aggregate: sum usage across all user paths`

- [ ] **Step 3: Create tenant_quota.go**

```go
// internal/server/tenant_quota.go
package server

import (
    "net/http"
    "time"

    "github.com/goccy/go-json"
    "github.com/labstack/echo/v4"

    "smartrouter/internal/budget"
    "smartrouter/internal/core"
)

func TenantQuotaMiddleware(svc *budget.Service) echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            tenantID := core.GetTenantID(c.Request().Context())
            if tenantID == "" {
                return next(c) // platform host — no budget
            }
            if err := svc.CheckTenant(c.Request().Context(), time.Now().UTC()); err != nil {
                var exceeded budget.ExceededError
                if errors.As(err, &exceeded) {
                    return c.JSON(http.StatusPaymentRequired, map[string]interface{}{
                        "error": map[string]interface{}{
                            "type":    "tenant_budget_exceeded",
                            "message": err.Error(),
                        },
                    })
                }
                // Transient error — log but allow request through
            }
            return next(c)
        }
    }
}
```

- [ ] **Step 4: Add to middleware chain in http.go**

In `internal/server/http.go`, in the `newServer` function, after the auth middleware is registered and before route handlers:
```go
if cfg.BudgetChecker != nil {
    e.Use(TenantQuotaMiddleware(cfg.BudgetChecker))
}
```

Position: after `WorkflowResolution` middleware, inside the `e.Use()` chain or as a group-level middleware on `v1`.

Actually, for precision: add it AFTER the auth middleware (line 319-321) so we have the tenant context, and BEFORE request handling. Add as a `e.Use()` in the global chain, NOT as a group middleware — it should apply to all inference endpoints.

- [ ] **Step 5: Run build and tests**

Run: `go build ./...` then `go test ./internal/budget/... -v`
Expected: PASS

---

### Task 7: Final verification

- [ ] **Step 1: Run full build**

Run: `go build ./...`
Expected: PASS (zero warnings)

- [ ] **Step 2: Run full tests**

Run: `go test ./...`
Expected: PASS (61+ packages, zero failures)

- [ ] **Step 3: Run vet**

Run: `go vet ./...`
Expected: PASS (accept pre-existing warnings that are not in P6 files)

- [ ] **Step 4: Dashboard JS tests**

Run: `node --test internal/admin/dashboard/static/js/modules/*.test.cjs`
Expected: PASS (no P6 changes, but ensure dashboard still works)

---

## P6 Completion Notes (2026-08-04)

P6 complete on master: 22 commits (`45530ee..3bed4a5`), subagent-driven with per-task review + final whole-branch review.

### Delivered

1. **conversationstore DB-ized** — SQLite/PostgreSQL/MongoDB backends (`store_sqlite.go`/`store_postgresql.go`/`store_mongodb.go`), `factory.go` with `storage.ResolveBackend` dispatch, wired in `app.go` via shared storage when available (MemoryStore remains the fallback). Composite PK `(tenant_id, id)` on all backends.
2. **responsestore tenant-scoped + DB-ized** — `StoredResponse` gained `TenantID`; Store interface gained `tenantID` on all 4 methods; MemoryStore key becomes `tenantID + "/" + id`; all callers pass `core.GetTenantID(ctx)`; three DB backends + factory + app wiring (same pattern as conversationstore).
3. **Quota middleware** — `internal/server/tenant_quota.go`: per-tenant aggregate budget enforcement mounted on the `/v1` group. Convention: budget with `user_path="*"` (normalizes to `"/*"`) = tenant-wide budget; `SumUsageCost` drops the user-path filter for `"*"`/`"/*"`. Returns 402 `tenant_budget_exceeded` via `writeGatewayError`; skips when no tenant resolved; logs-and-allows on transient failures. `budget.Service.CheckTenant` added; `server.BudgetChecker` interface extended.
4. **17 new SQLite-backed store tests** (conversationstore 9 + responsestore 8) covering roundtrip, tenant isolation, timestamp normalization, update-preserve, append, delete, create→update fallback. PG aggregate-branch test (env-gated `SMARTROUTER_TEST_POSTGRES_URL`), Mongo helper tests.

### Key decisions / deviations from plan text

- **`Close()` is no-op for all DB stores** — shared storage owns DB lifecycle (matches budget/filestore). Plan text said "close underlying DB"; user approved the change to avoid double-close on server shutdown.
- **Middleware takes `server.BudgetChecker` interface** (not `*budget.Service`) — `cfg.BudgetChecker` is the interface; `CheckTenant` added to it.
- **Mounted on `v1` group, not global chain** — global would lock tenant admins out of `/admin` when over budget.
- **`"*"` normalizes to `"/*"`** — `NormalizeUserPath("*")` prepends a slash, so aggregate detection handles both forms.
- **AppendItems race-free** — SQLite uses a single atomic chained `json_insert` UPDATE (modernc driver ignores `TxOptions.Isolation`, so BEGIN IMMEDIATE unavailable); PG uses `SELECT ... FOR UPDATE`. Mongo already atomic via `$push`.
- **DB stores normalize timestamps on Create** — zero `StoredAt`→now, zero `ExpiresAt`→`StoredAt+DefaultMemoryStoreTTL`; Mongo Update preserves persisted timestamps when incoming is zero.

### Deferred to P7+ (pre-existing P5/P4 minors, unchanged)

- ForTenant write-through refresh latency (config services, up to 1h tick staleness)
- `ResolveRequestModel` hardcodes `context.Background()` (test-only callers)
- nil-ctx guards on `snapshotFor`/`PipelineForWorkflow` (unreachable)
- `RefreshAll` full-map-swap semantics on transient tenant-list errors
- Dashboard JS: 3 timezone-environmental test failures pre-exist at P5 base (399/402 pass)
- `go vet ./...` 3 pre-existing warnings in `internal/core` (duplicate json tags)
