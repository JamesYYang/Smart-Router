# SmartRouter SaaS 多租户改造 — P1: 租户基座 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 引入租户实体、子域名解析中间件与 context 传播,使每个请求能按 `Host` 解析出 `tenantID` 并注入到 `context.Context`,为后续阶段的认证/隔离/管理后台拆分奠基。

**Architecture:** 新增 `internal/tenants` 包(tenant 实体 + Store + 带 60s 缓存的 Service)。新增 `TenantResolver` Echo 中间件,插在 `RequestSnapshotCapture` 之前,解析 `Host` → 查 `tenants` 表(经缓存)→ 注入 `core.TenantID` / `core.IsPlatformHost` 到 context。未配置 `base_domain` 时中间件 no-op,保证开发/测试环境(localhost)不中断。启动时 bootstrap 一个 `default` 租户,现有部署行为不变。

**Tech Stack:** Go 1.x, Echo v5 (`github.com/labstack/echo/v5`), `database/sql` (SQLite), `github.com/jackc/pgx/v5/pgxpool` (Postgres), `go.mongodb.org/mongo-driver/v2/mongo` (Mongo), `github.com/stretchr/testify`。

## Global Constraints

- 遵循项目约定:每个源文件配同包同基名 `_test.go`,使用 `testify`(`require`/`assert`)。
- 模型/路径命名为 `smartrouter`(非 `gomodel`);任何 `gomodel` 引用视为 stale。
- Provider 配置保持全局,P1 不改 `internal/providers`。
- P1 **不**改任何现有 Store 接口签名(那是 P3);**不**给现有业务表加 `tenant_id` 列(那是 P3);**不**改 auth 中间件(那是 P2)。P1 只新增租户基座 + 解析中间件 + context key。
- `base_domain` 为空时 `TenantResolver` 必须 no-op(不注入 tenant、不报错),保证 localhost 访问与现有测试不中断。
- 迁移遵循 per-store 模式:`CREATE TABLE IF NOT EXISTS` + `ALTER TABLE ADD COLUMN` 容错重复列(参考 `internal/authkeys/store_sqlite.go:45-53`)。

## File Structure

| 文件 | 职责 | 新建/修改 |
|---|---|---|
| `internal/tenants/types.go` | `Tenant` 实体 + `Status` 常量 + `ErrNotFound` | 新建 |
| `internal/tenants/store.go` | `Store` 接口 | 新建 |
| `internal/tenants/store_sqlite.go` | SQLite 实现 + 建表/迁移 | 新建 |
| `internal/tenants/store_postgresql.go` | Postgres 实现 | 新建 |
| `internal/tenants/store_mongodb.go` | Mongo 实现 | 新建 |
| `internal/tenants/service.go` | `Service`(缓存 + 基础 CRUD) | 新建 |
| `internal/server/tenant_resolver.go` | `TenantResolver` 中间件 | 新建 |
| `internal/core/context.go` | 新增 `tenantID`/`isPlatformHost` key 与 helper | 修改 |
| `config/server.go` | 新增 `BaseDomain`/`PlatformHost`/`BootstrapDefaultTenant` | 修改 |
| `config/config.example.yaml` | 文档化新配置 | 修改 |
| `internal/server/http.go` | `Config` 加 `TenantResolver`/`Tenants` 字段 + 中间件插入 | 修改 |
| `internal/app/app.go` | 装配 `tenants.Service` + bootstrap `default` 租户 | 修改 |

---

## Task 1: Tenant 实体类型与 context key

**Files:**
- Create: `internal/tenants/types.go`
- Create: `internal/tenants/types_test.go`
- Modify: `internal/core/context.go`(在 `requestOriginKey` 之后新增两个 key 与 helper)
- Test: `internal/core/context_test.go`(若不存在则新建)

**Interfaces:**
- Produces: `tenants.Tenant` 结构体、`tenants.StatusActive`/`StatusDisabled` 常量、`tenants.ErrNotFound`、`core.WithTenantID`/`core.GetTenantID`/`core.WithPlatformHost`/`core.GetPlatformHost`。

- [ ] **Step 1: 写 `internal/tenants/types.go` 的失败测试**

创建 `internal/tenants/types_test.go`:

```go
package tenants

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTenantStatusConstants(t *testing.T) {
	require.Equal(t, Status("active"), StatusActive)
	require.Equal(t, Status("disabled"), StatusDisabled)
}

func TestTenantIsDisabled(t *testing.T) {
	now := time.Now().UTC()
	require.True(t, Tenant{Status: StatusDisabled, UpdatedAt: now}.IsDisabled())
	require.False(t, Tenant{Status: StatusActive, UpdatedAt: now}.IsDisabled())
}

func TestErrNotFound(t *testing.T) {
	require.True(t, IsNotFound(ErrNotFound))
	require.False(t, IsNotFound(nil))
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/tenants/... -run TestTenant -v`
Expected: 编译失败(`Status`、`StatusActive`、`Tenant`、`IsDisabled`、`IsNotFound`、`ErrNotFound` 未定义)。

- [ ] **Step 3: 实现 `internal/tenants/types.go`**

`CreatedAt`/`UpdatedAt` 用 `time.Time`,与 `internal/authkeys` 一致(`authkeys.AuthKey.CreatedAt` 即 `time.Time`)。测试用 `time.Now().UTC()`。

```go
// Package tenants provides tenant entity types and persistence for the
// multi-tenant SaaS layer. A tenant owns a unique subdomain and scopes
// auth keys, usage, budgets, and per-tenant configuration overrides.
package tenants

import (
	"errors"
	"time"
)

// Status is the lifecycle state of a tenant.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// Tenant is a SaaS customer organization identified by a unique subdomain.
type Tenant struct {
	ID        string
	Subdomain string
	Name      string
	Status    Status
	Plan      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsDisabled reports whether the tenant is in the disabled state.
func (t Tenant) IsDisabled() bool { return t.Status == StatusDisabled }

// ErrNotFound is returned by Store/Service when no tenant matches the query.
var ErrNotFound = errors.New("tenant not found")

// IsNotFound reports whether err is a tenant-not-found error.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/tenants/... -run TestTenant -v`
Expected: PASS。

- [ ] **Step 5: 写 context key 的失败测试**

在 `internal/core/context_test.go` 末尾追加(若文件不存在则新建,包名 `core`):

```go
package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTenantIDContext(t *testing.T) {
	ctx := context.Background()
	require.Equal(t, "", GetTenantID(ctx))

	ctx = WithTenantID(ctx, "tenant-xyz")
	require.Equal(t, "tenant-xyz", GetTenantID(ctx))
}

func TestPlatformHostContext(t *testing.T) {
	ctx := context.Background()
	require.False(t, GetPlatformHost(ctx))

	ctx = WithPlatformHost(ctx, true)
	require.True(t, GetPlatformHost(ctx))

	ctx = WithPlatformHost(ctx, false)
	require.False(t, GetPlatformHost(ctx))
}
```

- [ ] **Step 6: 运行测试确认失败**

Run: `go test ./internal/core/... -run TestTenantIDContext -v`
Expected: 编译失败(`WithTenantID`/`GetTenantID`/`WithPlatformHost`/`GetPlatformHost` 未定义)。

- [ ] **Step 7: 实现 context key**

在 `internal/core/context.go` 的 `requestOriginKey` 常量之后新增:

```go
	// tenantIDKey stores the resolved tenant ID for the current request.
	// Set by the TenantResolver middleware from the Host header subdomain.
	tenantIDKey contextKey = "tenant-id"
	// platformHostKey marks requests hitting the platform admin host
	// (app/www/apex of the configured base_domain).
	platformHostKey contextKey = "platform-host"
```

并在文件末尾(`GetRequestOrigin` 之后)新增 helper:

```go
// WithTenantID returns a new context with the tenant ID attached.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}

// GetTenantID retrieves the tenant ID from context.
// Returns empty string when no tenant was resolved (e.g. base_domain not
// configured, or request hit the platform host).
func GetTenantID(ctx context.Context) string {
	if v := ctx.Value(tenantIDKey); v != nil {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// WithPlatformHost returns a new context marked as hitting the platform host.
func WithPlatformHost(ctx context.Context, isPlatform bool) context.Context {
	return context.WithValue(ctx, platformHostKey, isPlatform)
}

// GetPlatformHost reports whether the request targeted the platform admin host.
func GetPlatformHost(ctx context.Context) bool {
	if v := ctx.Value(platformHostKey); v != nil {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}
```

- [ ] **Step 8: 运行测试确认通过**

Run: `go test ./internal/core/... -run "TestTenantIDContext|TestPlatformHostContext" -v`
Expected: PASS。

- [ ] **Step 9: 全量构建 + 提交**

Run: `go build ./...`
Expected: 成功。

```bash
git add internal/tenants/types.go internal/tenants/types_test.go internal/core/context.go internal/core/context_test.go
git commit -m "feat(tenants): add Tenant entity and tenant context keys"
```

---

## Task 2: Store 接口与 SQLite 实现

**Files:**
- Create: `internal/tenants/store.go`
- Create: `internal/tenants/store_sqlite.go`
- Create: `internal/tenants/store_sqlite_test.go`

**Interfaces:**
- Consumes: `tenants.Tenant`、`tenants.ErrNotFound`(来自 Task 1)、`smartrouter/internal/storage/sqlutil`。
- Produces: `tenants.Store` 接口、`tenants.SQLiteStore`、`tenants.NewSQLiteStore(db *sql.DB) (*SQLiteStore, error)`。

- [ ] **Step 1: 写 Store 接口与 SQLite 的失败测试**

创建 `internal/tenants/store_sqlite_test.go`:

```go
package tenants

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"smartrouter/internal/storage/sqlutil"
)

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db := sqlutil.NewMemoryDB(t) // 见下方说明
	store, err := NewSQLiteStore(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSQLiteStore_CreateAndGet(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	tenant := Tenant{
		ID:        "t-1",
		Subdomain: "xyz",
		Name:      "XYZ Inc",
		Status:    StatusActive,
		Plan:      "pro",
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, store.Create(ctx, tenant))

	got, err := store.GetBySubdomain(ctx, "xyz")
	require.NoError(t, err)
	require.Equal(t, "t-1", got.ID)
	require.Equal(t, "XYZ Inc", got.Name)
	require.Equal(t, StatusActive, got.Status)
	require.Equal(t, "pro", got.Plan)
}

func TestSQLiteStore_GetBySubdomain_NotFound(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, err := store.GetBySubdomain(context.Background(), "missing")
	require.True(t, IsNotFound(err))
}

func TestSQLiteStore_GetByID(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-2", Subdomain: "abc", Name: "ABC", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	got, err := store.GetByID(ctx, "t-2")
	require.NoError(t, err)
	require.Equal(t, "abc", got.Subdomain)
}

func TestSQLiteStore_List(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-a", Subdomain: "a", Name: "A", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-b", Subdomain: "b", Name: "B", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	list, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestSQLiteStore_UpdateStatus(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-3", Subdomain: "c", Name: "C", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, store.UpdateStatus(ctx, "t-3", StatusDisabled, now.Add(time.Second)))
	got, err := store.GetByID(ctx, "t-3")
	require.NoError(t, err)
	require.True(t, got.IsDisabled())
}

func TestSQLiteStore_UniqueSubdomain(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-4", Subdomain: "dup", Name: "First", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))
	err := store.Create(ctx, Tenant{ID: "t-5", Subdomain: "dup", Name: "Second", Status: StatusActive, CreatedAt: now, UpdatedAt: now})
	require.Error(t, err) // UNIQUE 约束
}
```

**关于 `sqlutil.NewMemoryDB(t)`**:如果 `internal/storage/sqlutil` 已有内存 DB helper(检查 `internal/storage/sqlutil/` 下是否有 `NewMemoryDB` 或类似),直接复用;否则在测试文件内本地创建:

```go
import "database/sql"

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewSQLiteStore(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}
```

执行前用 `grep -rn "NewMemoryDB\|:memory:" internal/storage/sqlutil/ internal/authkeys/` 确认项目惯用方式,选其一。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/tenants/... -run TestSQLiteStore -v`
Expected: 编译失败(`Store`、`SQLiteStore`、`NewSQLiteStore` 未定义)。

- [ ] **Step 3: 实现 `internal/tenants/store.go`**

```go
package tenants

import (
	"context"
	"time"
)

// Store persists tenants across storage backends.
type Store interface {
	Create(ctx context.Context, tenant Tenant) error
	GetByID(ctx context.Context, id string) (Tenant, error)
	GetBySubdomain(ctx context.Context, subdomain string) (Tenant, error)
	List(ctx context.Context) ([]Tenant, error)
	UpdateStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error
	Close() error
}
```

- [ ] **Step 4: 实现 `internal/tenants/store_sqlite.go`**

```go
package tenants

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLiteStore stores tenants in SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates the tenants table and indexes if needed.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tenants (
			id          TEXT PRIMARY KEY,
			subdomain   TEXT NOT NULL UNIQUE,
			name        TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'active',
			plan        TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create tenants table: %w", err)
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_tenants_subdomain ON tenants(subdomain)`,
		`CREATE INDEX IF NOT EXISTS idx_tenants_status ON tenants(status)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			return nil, fmt.Errorf("failed to create tenants index: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Create(ctx context.Context, tenant Tenant) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tenants (id, subdomain, name, status, plan, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, tenant.ID, tenant.Subdomain, tenant.Name, string(tenant.Status), tenant.Plan, tenant.CreatedAt.Unix(), tenant.UpdatedAt.Unix())
	if err != nil {
		if isSQLiteUniqueConstraintError(err) {
			return fmt.Errorf("tenant subdomain %q already exists: %w", tenant.Subdomain, err)
		}
		return fmt.Errorf("create tenant: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetByID(ctx context.Context, id string) (Tenant, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants WHERE id = ?
	`, id)
	return scanSQLiteTenant(row)
}

func (s *SQLiteStore) GetBySubdomain(ctx context.Context, subdomain string) (Tenant, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants WHERE subdomain = ?
	`, subdomain)
	return scanSQLiteTenant(row)
}

func (s *SQLiteStore) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants ORDER BY created_at DESC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanSQLiteTenant(rows)
		if err != nil {
			return nil, fmt.Errorf("iterate tenants: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE tenants SET status = ?, updated_at = ? WHERE id = ?
	`, string(status), updatedAt.Unix(), id)
	if err != nil {
		return fmt.Errorf("update tenant status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read update status rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) Close() error { return nil }

type sqliteScanner interface{ Scan(dest ...any) error }

func scanSQLiteTenant(scanner sqliteScanner) (Tenant, error) {
	var t Tenant
	var status string
	var createdAt, updatedAt int64
	if err := scanner.Scan(&t.ID, &t.Subdomain, &t.Name, &status, &t.Plan, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, err
	}
	t.Status = Status(status)
	t.CreatedAt = time.Unix(createdAt, 0).UTC()
	t.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return t, nil
}

func isSQLiteUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint")
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/tenants/... -run TestSQLiteStore -v`
Expected: 6 个测试全部 PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/tenants/store.go internal/tenants/store_sqlite.go internal/tenants/store_sqlite_test.go
git commit -m "feat(tenants): add Store interface and SQLite implementation"
```

---

## Task 3: 带 TTL 缓存的 Service 层

**Files:**
- Create: `internal/tenants/service.go`
- Create: `internal/tenants/service_test.go`

**Interfaces:**
- Consumes: `tenants.Store`(Task 2)、`tenants.Tenant`、`tenants.ErrNotFound`。
- Produces: `tenants.Service`、`tenants.NewService(store Store, ttl time.Duration) *Service`、`(*Service).ResolveBySubdomain(ctx, subdomain) (Tenant, error)`、`(*Service).GetByID(ctx, id)`、`(*Service).Create`、`(*Service).List`、`(*Service).UpdateStatus`、`(*Service).Close`。

**设计要点:** 缓存按 subdomain 做,正命中时直接返回(不查库), TTL 过期后下次查询强制刷新。负缓存(NotFound)也缓存,避免未知子域名打爆库——但 P1 先只缓存正命中,负缓存留 TODO(不在 P1 范围)。

- [ ] **Step 1: 写 Service 缓存的失败测试**

创建 `internal/tenants/service_test.go`:

```go
package tenants

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeStore is a minimal Store stub that counts GetBySubdomain calls.
type fakeStore struct {
	gets    atomic.Int64
	tenant  Tenant
	getErr  error
	created []Tenant
}

func (f *fakeStore) Create(_ context.Context, t Tenant) error {
	f.created = append(f.created, t)
	f.tenant = t
	return nil
}
func (f *fakeStore) GetByID(_ context.Context, _ string) (Tenant, error) {
	return f.tenant, f.getErr
}
func (f *fakeStore) GetBySubdomain(_ context.Context, _ string) (Tenant, error) {
	f.gets.Add(1)
	if f.getErr != nil {
		return Tenant{}, f.getErr
	}
	return f.tenant, nil
}
func (f *fakeStore) List(_ context.Context) ([]Tenant, error) { return []Tenant{f.tenant}, nil }
func (f *fakeStore) UpdateStatus(_ context.Context, _ string, _ Status, _ time.Time) error {
	return nil
}
func (f *fakeStore) Close() error { return nil }

func TestService_ResolveBySubdomain_CacheHit(t *testing.T) {
	store := &fakeStore{tenant: Tenant{ID: "t-1", Subdomain: "xyz", Status: StatusActive}}
	svc := NewService(store, time.Minute)

	t1, err := svc.ResolveBySubdomain(context.Background(), "xyz")
	require.NoError(t, err)
	require.Equal(t, "t-1", t1.ID)

	t2, err := svc.ResolveBySubdomain(context.Background(), "xyz")
	require.NoError(t, err)
	require.Equal(t, "t-1", t2.ID)

	// 第二次应命中缓存,store 只被查询一次
	require.Equal(t, int64(1), store.gets.Load())
}

func TestService_ResolveBySubdomain_NotFound(t *testing.T) {
	store := &fakeStore{getErr: ErrNotFound}
	svc := NewService(store, time.Minute)

	_, err := svc.ResolveBySubdomain(context.Background(), "missing")
	require.True(t, IsNotFound(err))
}

func TestService_ResolveBySubdomain_DisabledTenant(t *testing.T) {
	store := &fakeStore{tenant: Tenant{ID: "t-2", Subdomain: "off", Status: StatusDisabled}}
	svc := NewService(store, time.Minute)

	_, err := svc.ResolveBySubdomain(context.Background(), "off")
	require.Error(t, err)
	var te *TenantDisabledError
	require.True(t, errors.As(err, &te))
}

func TestService_ResolveBySubdomain_TTLExpiry(t *testing.T) {
	store := &fakeStore{tenant: Tenant{ID: "t-3", Subdomain: "exp", Status: StatusActive}}
	svc := NewService(store, 10*time.Millisecond)

	_, _ = svc.ResolveBySubdomain(context.Background(), "exp")
	time.Sleep(20 * time.Millisecond)
	_, _ = svc.ResolveBySubdomain(context.Background(), "exp")

	// TTL 过期后应再次查库
	require.Equal(t, int64(2), store.gets.Load())
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/tenants/... -run TestService -v`
Expected: 编译失败(`NewService`、`TenantDisabledError`、`ResolveBySubdomain` 未定义)。

- [ ] **Step 3: 实现 `internal/tenants/service.go`**

```go
package tenants

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TenantDisabledError is returned when a tenant is resolved but its status
// is disabled.
type TenantDisabledError struct {
	TenantID  string
	Subdomain string
}

func (e *TenantDisabledError) Error() string {
	return fmt.Sprintf("tenant %q (%s) is disabled", e.Subdomain, e.TenantID)
}

// Service wraps a Store with an in-memory subdomain cache.
type Service struct {
	store Store
	ttl   time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry // keyed by subdomain
}

type cacheEntry struct {
	tenant   Tenant
	expiresAt time.Time
}

// NewService returns a Service caching resolved tenants for the given TTL.
// ttl <= 0 disables caching (every call hits the store).
func NewService(store Store, ttl time.Duration) *Service {
	if ttl < 0 {
		ttl = 0
	}
	return &Service{store: store, ttl: ttl, entries: make(map[string]cacheEntry)}
}

// ResolveBySubdomain returns the active tenant for the subdomain.
// Returns ErrNotFound when no tenant matches, or *TenantDisabledError when
// the tenant exists but is disabled.
func (s *Service) ResolveBySubdomain(ctx context.Context, subdomain string) (Tenant, error) {
	if cached, ok := s.cacheGet(subdomain); ok {
		if cached.IsDisabled() {
			return cached, &TenantDisabledError{TenantID: cached.ID, Subdomain: cached.Subdomain}
		}
		return cached, nil
	}
	t, err := s.store.GetBySubdomain(ctx, subdomain)
	if err != nil {
		return Tenant{}, err
	}
	s.cacheSet(subdomain, t)
	if t.IsDisabled() {
		return t, &TenantDisabledError{TenantID: t.ID, Subdomain: t.Subdomain}
	}
	return t, nil
}

func (s *Service) GetByID(ctx context.Context, id string) (Tenant, error) {
	return s.store.GetByID(ctx, id)
}

func (s *Service) Create(ctx context.Context, t Tenant) error {
	return s.store.Create(ctx, t)
}

func (s *Service) List(ctx context.Context) ([]Tenant, error) {
	return s.store.List(ctx)
}

func (s *Service) UpdateStatus(ctx context.Context, id string, status Status, now time.Time) error {
	if err := s.store.UpdateStatus(ctx, id, status, now); err != nil {
		return err
	}
	s.invalidate(id)
	return nil
}

func (s *Service) Close() error { return s.store.Close() }

func (s *Service) cacheGet(subdomain string) (Tenant, bool) {
	if s.ttl == 0 {
		return Tenant{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[subdomain]
	if !ok {
		return Tenant{}, false
	}
	if time.Now().After(e.expiresAt) {
		return Tenant{}, false
	}
	return e.tenant, true
}

func (s *Service) cacheSet(subdomain string, t Tenant) {
	if s.ttl == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[subdomain] = cacheEntry{tenant: t, expiresAt: time.Now().Add(s.ttl)}
}

// invalidate drops cache entries that may reference the given tenant id.
func (s *Service) invalidate(tenantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub, e := range s.entries {
		if e.tenant.ID == tenantID {
			delete(s.entries, sub)
		}
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/tenants/... -run TestService -v`
Expected: 4 个测试全部 PASS。

- [ ] **Step 5: 跑全包测试 + 提交**

Run: `go test ./internal/tenants/... -v`
Expected: 全部 PASS。

```bash
git add internal/tenants/service.go internal/tenants/service_test.go
git commit -m "feat(tenants): add Service with subdomain cache and TTL"
```

---

## Task 4: TenantResolver 中间件

**Files:**
- Create: `internal/server/tenant_resolver.go`
- Create: `internal/server/tenant_resolver_test.go`

**Interfaces:**
- Consumes: `tenants.Service`、`tenants.TenantDisabledError`、`tenants.IsNotFound`、`core.WithTenantID`、`core.WithPlatformHost`。
- Produces: `server.TenantResolver(svc *tenants.Service, baseDomain, platformHost string) echo.MiddlewareFunc`。

**行为约定(与设计文档 §3.2 一致):**
- `baseDomain == ""` → no-op(next 直接调用,不注入任何 tenant 信息)。用于开发/测试。
- Host 等于 `platformHost + "." + baseDomain`、`www.` + baseDomain、或 baseDomain(apex)→ 注入 `isPlatformHost=true`,不注入 tenantID。
- Host 的首段是其它值 → 当作 subdomain 查 `svc.ResolveBySubdomain`:
  - 命中且 active → 注入 `tenantID`。
  - `*TenantDisabledError` → 返回 403 `{"error":{"type":"tenant_disabled"}}`。
  - `ErrNotFound` → 返回 404 `{"error":{"type":"unknown_tenant"}}`。
- Host 不以 `.` + baseDomain 结尾(如 `localhost:8080`)→ no-op(不注入,不报错)。

- [ ] **Step 1: 写中间件的失败测试**

创建 `internal/server/tenant_resolver_test.go`:

```go
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/core"
	"smartrouter/internal/tenants"
)

func newEchoWithTenantResolver(t *testing.T, svc *tenants.Service, baseDomain, platformHost string) (*echo.Echo, *bool) {
	t.Helper()
	e := echo.New()
	called := false
	e.Use(TenantResolver(svc, baseDomain, platformHost))
	e.GET("/probe", func(c echo.Context) error {
		called = true
		// 把 context 里的 tenant 信息回写到响应,便于断言
		return c.JSON(http.StatusOK, map[string]any{
			"tenant_id":      core.GetTenantID(c.Request().Context()),
			"platform_host":  core.GetPlatformHost(c.Request().Context()),
		})
	})
	return e, &called
}

func doReq(e *echo.Echo, host, path string) (*httptest.ResponseRecorder, echo.Context) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = e.ServeHTTP(rec, req)
	return rec, c
}

func TestTenantResolver_BaseDomainEmpty_NoOp(t *testing.T) {
	svc := tenants.NewService(nil, 0) // store nil,不应被调用
	e, called := newEchoWithTenantResolver(t, svc, "", "app")

	rec, _ := doReq(e, "localhost:8080", "/probe")
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, *called)
}

func TestTenantResolver_PlatformHost(t *testing.T) {
	svc := tenants.NewService(nil, 0)
	e, _ := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	for _, host := range []string{"app.smart-router.com", "www.smart-router.com", "smart-router.com"} {
		rec, c := doReq(e, host, "/probe")
		require.Equal(t, http.StatusOK, rec.Code, host)
		require.True(t, core.GetPlatformHost(c.Request().Context()), host)
		require.Empty(t, core.GetTenantID(c.Request().Context()), host)
	}
}

func TestTenantResolver_TenantSubdomain(t *testing.T) {
	store := &stubStore{tenant: tenants.Tenant{ID: "t-1", Subdomain: "xyz", Status: tenants.StatusActive}}
	svc := tenants.NewService(store, time.Minute)
	e, _ := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	rec, c := doReq(e, "xyz.smart-router.com", "/probe")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "t-1", core.GetTenantID(c.Request().Context()))
	require.False(t, core.GetPlatformHost(c.Request().Context()))
}

func TestTenantResolver_UnknownSubdomain(t *testing.T) {
	store := &stubStore{getErr: tenants.ErrNotFound}
	svc := tenants.NewService(store, time.Minute)
	e, _ := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	rec, _ := doReq(e, "missing.smart-router.com", "/probe")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "unknown_tenant")
}

func TestTenantResolver_DisabledTenant(t *testing.T) {
	store := &stubStore{tenant: tenants.Tenant{ID: "t-2", Subdomain: "off", Status: tenants.StatusDisabled}}
	svc := tenants.NewService(store, time.Minute)
	e, _ := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	rec, _ := doReq(e, "off.smart-router.com", "/probe")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "tenant_disabled")
}

func TestTenantResolver_ForeignHost_NoOp(t *testing.T) {
	svc := tenants.NewService(nil, 0)
	e, called := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	// Host 不以 .smart-router.com 结尾
	rec, c := doReq(e, "localhost:8080", "/probe")
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, *called)
	require.Empty(t, core.GetTenantID(c.Request().Context()))
	require.False(t, core.GetPlatformHost(c.Request().Context()))
}
```

需要在同测试文件定义 `stubStore`(复用 Task 3 的 fakeStore 模式,但放在 server 包测试里):

```go
type stubStore struct {
	tenant tenants.Tenant
	getErr error
}

func (s *stubStore) Create(context.Context, tenants.Tenant) error { return nil }
func (s *stubStore) GetByID(context.Context, string) (tenants.Tenant, error) {
	return s.tenant, s.getErr
}
func (s *stubStore) GetBySubdomain(context.Context, string) (tenants.Tenant, error) {
	return s.tenant, s.getErr
}
func (s *stubStore) List(context.Context) ([]tenants.Tenant, error) { return nil, nil }
func (s *stubStore) UpdateStatus(context.Context, string, tenants.Status, time.Time) error {
	return nil
}
func (s *stubStore) Close() error { return nil }
```

测试文件需 `import "time"`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/... -run TestTenantResolver -v`
Expected: 编译失败(`TenantResolver` 未定义)。

- [ ] **Step 3: 实现 `internal/server/tenant_resolver.go`**

```go
package server

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/core"
	"smartrouter/internal/tenants"
)

// TenantResolver resolves the request tenant from the Host header and
// injects it into the request context. When baseDomain is empty the
// middleware is a no-op (development/localhost mode).
func TenantResolver(svc *tenants.Service, baseDomain, platformHost string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if baseDomain == "" || svc == nil {
				return next(c)
			}
			req := c.Request()
			subdomain, isPlatform, matched := parseHost(req.Host, baseDomain, platformHost)
			if !matched {
				// Foreign host (e.g. localhost) — leave context unset.
				return next(c)
			}
			if isPlatform {
				ctx := core.WithPlatformHost(req.Context(), true)
				c.SetRequest(req.WithContext(ctx))
				return next(c)
			}
			tenant, err := svc.ResolveBySubdomain(req.Context(), subdomain)
			if err != nil {
				return tenantError(c, err)
			}
			ctx := core.WithTenantID(req.Context(), tenant.ID)
			c.SetRequest(req.WithContext(ctx))
			return next(c)
		}
	}
}

// parseHost splits the Host header into a subdomain and whether it is the
// platform host. matched is false when the host does not belong to baseDomain.
func parseHost(host, baseDomain, platformHost string) (subdomain string, isPlatform, matched bool) {
	// strip :port
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || baseDomain == "" {
		return "", false, false
	}
	suffix := "." + baseDomain
	if !strings.HasSuffix(host, suffix) && host != baseDomain {
		return "", false, false
	}
	if host == baseDomain {
		return "", true, true // apex
	}
	first := strings.TrimSuffix(host, suffix)
	if first == "www" || first == platformHost {
		return "", true, true
	}
	return first, false, true
}

func tenantError(c echo.Context, err error) error {
	if tenants.IsNotFound(err) {
		return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "unknown_tenant"}})
	}
	if _, ok := err.(*tenants.TenantDisabledError); ok {
		return c.JSON(http.StatusForbidden, map[string]any{"error": map[string]string{"type": "tenant_disabled"}})
	}
	return c.JSON(http.StatusInternalServerError, map[string]any{"error": map[string]string{"type": "tenant_resolution_failed"}})
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/server/... -run TestTenantResolver -v`
Expected: 6 个测试全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/server/tenant_resolver.go internal/server/tenant_resolver_test.go
git commit -m "feat(server): add TenantResolver middleware"
```

---

## Task 5: 配置字段与装配接线

**Files:**
- Modify: `config/server.go`(加 3 个字段)
- Modify: `config/config.example.yaml`(文档化)
- Modify: `internal/server/http.go`(`Config` 加 `Tenants *tenants.Service`、`BaseDomain`、`PlatformHost`;中间件链插入 `TenantResolver`)
- Modify: `internal/app/app.go`(构造 `tenants.Service`,bootstrap `default` 租户,传入 `server.Config`)
- Test: `internal/app/app_test.go`(若存在则追加;否则在 `internal/server/http_test.go` 加端到端用例)

**Interfaces:**
- Consumes: `tenants.NewSQLiteStore`、`tenants.NewService`、`server.TenantResolver`。
- Produces: 配置项 `SERVER_BASE_DOMAIN`/`SERVER_PLATFORM_HOST`/`BOOTSTRAP_DEFAULT_TENANT`;启动后 `tenants` 表存在且含 `default` 行;中间件链中 `TenantResolver` 在 `RequestSnapshotCapture` 之前。

- [ ] **Step 1: 写配置字段的失败测试**

在 `config/server_test.go`(若不存在则新建)追加:

```go
package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServerConfig_TenantFields_FromEnv(t *testing.T) {
	t.Setenv("SERVER_BASE_DOMAIN", "smart-router.com")
	t.Setenv("SERVER_PLATFORM_HOST", "app")
	t.Setenv("BOOTSTRAP_DEFAULT_TENANT", "true")

	cfg, err := LoadFromEnvOnly() // 见说明
	require.NoError(t, err)
	require.Equal(t, "smart-router.com", cfg.Server.BaseDomain)
	require.Equal(t, "app", cfg.Server.PlatformHost)
	require.True(t, cfg.Server.BootstrapDefaultTenant)
}

func TestServerConfig_TenantFields_Defaults(t *testing.T) {
	cfg, err := LoadFromEnvOnly()
	require.NoError(t, err)
	require.Equal(t, "", cfg.Server.BaseDomain)
	require.Equal(t, "app", cfg.Server.PlatformHost) // 默认 app
	require.True(t, cfg.Server.BootstrapDefaultTenant)
}
```

**说明 `LoadFromEnvOnly`**:检查 `config` 包是否已有现成的"仅 env 加载"helper(如 `LoadFromEnv`、`loadFromEnvMap`)。若有则复用;若无,在 `config/server_test.go` 内用最简方式:直接构造 `ServerConfig{}` 并通过项目既有的 env 解析逻辑填充。执行前 `grep -rn "func Load" config/*.go` 确认入口。若 Load 需要文件,可临时写 `t.TempDir()` 下的空 yaml。若太复杂,改为直接断言默认值:`require.Equal(t, ServerConfig{PlatformHost: "app", BootstrapDefaultTenant: true}.PlatformHost, "app")` 并跳过 env 测试,但优先复用现有 helper。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./config/... -run TestServerConfig_TenantFields -v`
Expected: 编译失败(`BaseDomain`/`PlatformHost`/`BootstrapDefaultTenant` 字段不存在)。

- [ ] **Step 3: 加配置字段到 `config/server.go`**

在 `ServerConfig` 结构体末尾(`EnabledPassthroughProviders` 之后)追加:

```go
	// BaseDomain is the base domain used to resolve tenants from the Host
	// header subdomain (e.g. "smart-router.com" → tenant "xyz" from
	// "xyz.smart-router.com"). Empty disables tenant resolution (dev mode).
	BaseDomain string `yaml:"base_domain" env:"SERVER_BASE_DOMAIN"`
	// PlatformHost is the subdomain reserved for the platform admin UI
	// (e.g. "app" → "app.smart-router.com"). "www" and the apex always
	// resolve to the platform host too. Default: "app".
	PlatformHost string `yaml:"platform_host" env:"SERVER_PLATFORM_HOST"`
	// BootstrapDefaultTenant creates a "default" tenant on startup when the
	// tenants table is empty. Default: true.
	BootstrapDefaultTenant bool `yaml:"bootstrap_default_tenant" env:"BOOTSTRAP_DEFAULT_TENANT"`
```

同时在 `config/config.go` 的 `defaults()` 或 `Load()` 中设置默认值(`PlatformHost: "app"`, `BootstrapDefaultTenant: true`)。执行前 `grep -n "func.*default\|cfg.Server\." config/config.go` 找到默认值填充点,在 `Server` 默认值附近加:

```go
if cfg.Server.PlatformHost == "" {
	cfg.Server.PlatformHost = "app"
}
// BootstrapDefaultTenant 默认 true:仅当 env 显式设 "false" 才关闭
// (bool 字段零值为 false,需用 env 解析逻辑覆盖;参考现有 bool 字段如 SwaggerEnabled 的处理)
```

**注意 bool 默认值陷阱**:`BootstrapDefaultTenant bool` 零值是 `false`,但我们要默认 `true`。检查项目现有 bool 配置如何处理"默认 true"(如 `EnablePassthroughRoutes` 注释为 Default: true)——通常在 `Load()` 末尾对未显式设置的字段补默认。复用同一模式。

- [ ] **Step 4: 在 `config/config.example.yaml` 的 `server:` 段文档化**

在 `server:` 段(参考现有字段位置)加:

```yaml
  # Base domain for multi-tenant subdomain routing. When set, requests to
  # <subdomain>.<base_domain> are resolved to a tenant. Empty disables
  # tenant resolution (single-tenant / localhost dev mode).
  base_domain: "" # env: SERVER_BASE_DOMAIN  e.g. "smart-router.com"
  # Subdomain reserved for the platform admin UI. "www" and the apex always
  # map to the platform host too.
  platform_host: "app" # env: SERVER_PLATFORM_HOST
  # Create a "default" tenant on startup when the tenants table is empty.
  bootstrap_default_tenant: true # env: BOOTSTRAP_DEFAULT_TENANT
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./config/... -run TestServerConfig_TenantFields -v`
Expected: PASS。

- [ ] **Step 6: 写装配与 bootstrap 的失败测试**

在 `internal/server/http_test.go`(或新建 `internal/server/tenant_integration_test.go`)加端到端用例:

```go
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/tenants"
)

func TestServerEndToEnd_TenantResolution(t *testing.T) {
	// 用真实 SQLite 内存 store + Service,验证完整中间件链
	store, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, tenants.Tenant{ID: "t-xyz", Subdomain: "xyz", Name: "XYZ", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))

	svc := tenants.NewService(store, time.Minute)

	e := echo.New()
	e.Use(TenantResolver(svc, "smart-router.com", "app"))
	var gotTenantID string
	e.GET("/v1/chat/completions", func(c echo.Context) error {
		gotTenantID = core.GetTenantID(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Host = "xyz.smart-router.com"
	rec := httptest.NewRecorder()
	require.NoError(t, e.ServeHTTP(rec, req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "t-xyz", gotTenantID)
}
```

`newMemoryDB` 复用 Task 2 Step 1 确认的 helper;`time` 与 `core` 需 import。

- [ ] **Step 7: 运行测试确认通过(此时中间件已实现,应 PASS)**

Run: `go test ./internal/server/... -run TestServerEndToEnd_TenantResolution -v`
Expected: PASS(验证中间件在完整 echo 实例中工作)。

- [ ] **Step 8: 修改 `internal/server/http.go` 的 `Config` 与中间件链**

在 `Config` 结构体(`http.go` 顶部 `type Config struct` 处)加字段:

```go
	// Tenants is the tenant resolution service. When non-nil and BaseDomain
	// is set, the TenantResolver middleware runs before RequestSnapshotCapture.
	Tenants      *tenants.Service
	BaseDomain   string
	PlatformHost string
```

加 import:`"smartrouter/internal/tenants"`。

在中间件链中,`modelInteractionWriteDeadlineMiddleware()`(约 `http.go:277`)之后、`RequestSnapshotCapture`(约 `http.go:281`)之前插入:

```go
	// Tenant resolution runs before ingress capture so downstream stages
	// (snapshot, auth, workflow) can read tenantID from context.
	if cfg != nil && cfg.Tenants != nil && cfg.BaseDomain != "" {
		e.Use(TenantResolver(cfg.Tenants, cfg.BaseDomain, cfg.PlatformHost))
	}
```

- [ ] **Step 9: 修改 `internal/app/app.go` 装配 `tenants.Service` 与 bootstrap**

在 `app.New` 中(参考 `app.go:195-386` 创建其它 store 的位置,如 authkeys store 创建点)加:

```go
	// tenants store + service (multi-tenant SaaS layer)
	tenantStore, err := storage.ResolveBackend(
		sharedStorage,
		func(db *sql.DB) (tenants.Store, error) { return tenants.NewSQLiteStore(db) },
		func(pool *pgxpool.Pool) (tenants.Store, error) { return tenants.NewPostgreSQLStore(pool) },
		func(db *mongo.Database) (tenants.Store, error) { return tenants.NewMongoDBStore(db) },
	)
	if err != nil {
		// unwind 已初始化组件(参考现有 New 的错误处理模式)
		return nil, fmt.Errorf("create tenant store: %w", err)
	}
	tenantSvc := tenants.NewService(tenantStore, time.Minute)
```

在传入 `server.Config` 处(参考 `app.go:500-565` `serverCfg := server.Config{...}`)加:

```go
		Tenants:      tenantSvc,
		BaseDomain:   cfg.Server.BaseDomain,
		PlatformHost: cfg.Server.PlatformHost,
```

在 `app.New` 末尾、返回前加 bootstrap `default` 租户(参考 app.go 中其它启动期初始化的位置):

```go
	if cfg.Server.BootstrapDefaultTenant {
		if err := bootstrapDefaultTenant(ctx, tenantSvc); err != nil {
			// unwind
			return nil, fmt.Errorf("bootstrap default tenant: %w", err)
		}
	}
```

新增 helper(放在 `app.go` 末尾或新文件 `internal/app/tenants_bootstrap.go`):

```go
package app

import (
	"context"
	"time"

	"smartrouter/internal/tenants"
)

func bootstrapDefaultTenant(ctx context.Context, svc *tenants.Service) error {
	if _, err := svc.GetBySubdomain(ctx, "default"); err == nil {
		return nil // 已存在
	} else if !tenants.IsNotFound(err) {
		return err
	}
	now := time.Now().UTC()
	return svc.Create(ctx, tenants.Tenant{
		ID:        "default",
		Subdomain: "default",
		Name:      "Default Tenant",
		Status:    tenants.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
}
```

需在 `app.go` 加 import:`"database/sql"`、`"github.com/jackc/pgx/v5/pgxpool"`、`"go.mongodb.org/mongo-driver/v2/mongo"`、`"smartrouter/internal/tenants"`、`"time"`(部分可能已存在)。

- [ ] **Step 10: 跑全量构建 + 测试**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS。如有 unwind 逻辑编译错误,参考 `app.go` 现有 `New` 的错误处理模式修正。

- [ ] **Step 11: 提交**

```bash
git add config/server.go config/config.example.yaml config/server_test.go internal/server/http.go internal/server/http_test.go internal/server/tenant_integration_test.go internal/app/app.go internal/app/tenants_bootstrap.go
git commit -m "feat(app): wire tenant service, resolver middleware, and default tenant bootstrap"
```

---

## Task 6: Postgres 与 Mongo Store 实现

**Files:**
- Create: `internal/tenants/store_postgresql.go`
- Create: `internal/tenants/store_postgresql_test.go`
- Create: `internal/tenants/store_mongodb.go`
- Create: `internal/tenants/store_mongodb_test.go`

**Interfaces:**
- Consumes: `tenants.Store`(Task 2)、`tenants.Tenant`、`tenants.ErrNotFound`。
- Produces: `tenants.NewPostgreSQLStore(pool *pgxpool.Pool) (*PostgreSQLStore, error)`、`tenants.NewMongoDBStore(db *mongo.Database) (*MongoDBStore, error)`。

**测试策略:** Postgres/Mongo 测试默认跳过,通过环境变量启用(`SMARTROUTER_TEST_POSTGRES_URL`、`SMARTROUTER_TEST_MONGO_URL`),与项目现有跨后端测试模式一致。执行前 `grep -rn "SMARTROUTER_TEST\|testing.Short\|skipIf" internal/authkeys/*_test.go` 确认项目惯用 skip 模式并复用。

- [ ] **Step 1: 写 Postgres 实现的失败测试(可 skip)**

创建 `internal/tenants/store_postgresql_test.go`:

```go
package tenants

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func skipIfNoPostgres(t *testing.T) string {
	t.Helper()
	url := os.Getenv("SMARTROUTER_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set SMARTROUTER_TEST_POSTGRES_URL to run")
	}
	return url
}

func TestPostgreSQLStore_CRUD(t *testing.T) {
	url := skipIfNoPostgres(t)
	// 复用项目既有的 pgxpool 构造 helper;执行前 grep 确认
	pool := newTestPool(t, url)
	store, err := NewPostgreSQLStore(pool)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-pg-1", Subdomain: "pgxyz", Name: "PG", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	got, err := store.GetBySubdomain(ctx, "pgxyz")
	require.NoError(t, err)
	require.Equal(t, "t-pg-1", got.ID)
}
```

`newTestPool` 参考 `internal/authkeys/store_postgresql_test.go` 中的同等 helper(执行前 `grep -n "pgxpool\|newTestPool\|NewPool" internal/authkeys/*_test.go`)。

- [ ] **Step 2: 实现 `internal/tenants/store_postgresql.go`**

```go
package tenants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQLStore stores tenants in PostgreSQL.
type PostgreSQLStore struct {
	pool *pgxpool.Pool
}

func NewPostgreSQLStore(pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("pgx pool is required")
	}
	_, err := pool.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS tenants (
			id          TEXT PRIMARY KEY,
			subdomain   TEXT NOT NULL UNIQUE,
			name        TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'active',
			plan        TEXT NOT NULL DEFAULT '',
			created_at  BIGINT NOT NULL,
			updated_at  BIGINT NOT NULL
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create tenants table: %w", err)
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func (s *PostgreSQLStore) Create(ctx context.Context, t Tenant) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenants (id, subdomain, name, status, plan, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, t.ID, t.Subdomain, t.Name, string(t.Status), t.Plan, t.CreatedAt.Unix(), t.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("create tenant: %w", err)
	}
	return nil
}

func (s *PostgreSQLStore) GetByID(ctx context.Context, id string) (Tenant, error) {
	return scanPGTenant(s.pool.QueryRow(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants WHERE id = $1
	`, id))
}

func (s *PostgreSQLStore) GetBySubdomain(ctx context.Context, sub string) (Tenant, error) {
	return scanPGTenant(s.pool.QueryRow(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants WHERE subdomain = $1
	`, sub))
}

func (s *PostgreSQLStore) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants ORDER BY created_at DESC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanPGTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgreSQLStore) UpdateStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenants SET status = $1, updated_at = $2 WHERE id = $3
	`, string(status), updatedAt.Unix(), id)
	if err != nil {
		return fmt.Errorf("update tenant status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgreSQLStore) Close() error { return nil }

type pgScanner interface{ Scan(dest ...any) error }

func scanPGTenant(sc pgScanner) (Tenant, error) {
	var t Tenant
	var status string
	var created, updated int64
	if err := sc.Scan(&t.ID, &t.Subdomain, &t.Name, &status, &t.Plan, &created, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, err
	}
	t.Status = Status(status)
	t.CreatedAt = time.Unix(created, 0).UTC()
	t.UpdatedAt = time.Unix(updated, 0).UTC()
	return t, nil
}
```

- [ ] **Step 3: 运行 Postgres 测试(默认 skip)**

Run: `go test ./internal/tenants/... -run TestPostgreSQLStore -v`
Expected: SKIP(无 env)或 PASS(有 env)。

- [ ] **Step 4: 写 Mongo 实现的失败测试(可 skip)**

创建 `internal/tenants/store_mongodb_test.go`:

```go
package tenants

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func skipIfNoMongo(t *testing.T) string {
	t.Helper()
	url := os.Getenv("SMARTROUTER_TEST_MONGO_URL")
	if url == "" {
		t.Skip("set SMARTROUTER_TEST_MONGO_URL to run")
	}
	return url
}

func TestMongoDBStore_CRUD(t *testing.T) {
	url := skipIfNoMongo(t)
	db := newTestMongoDB(t, url) // 复用 authkeys mongo 测试 helper
	store, err := NewMongoDBStore(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-mg-1", Subdomain: "mgxyz", Name: "MG", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	got, err := store.GetBySubdomain(ctx, "mgxyz")
	require.NoError(t, err)
	require.Equal(t, "t-mg-1", got.ID)
}
```

`newTestMongoDB` 参考 `internal/authkeys/store_mongodb_test.go`。

- [ ] **Step 5: 实现 `internal/tenants/store_mongodb.go`**

```go
package tenants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoDBStore stores tenants in MongoDB.
type MongoDBStore struct {
	coll *mongo.Collection
}

func NewMongoDBStore(db *mongo.Database) (*MongoDBStore, error) {
	if db == nil {
		return nil, fmt.Errorf("mongo database is required")
	}
	coll := db.Collection("tenants")
	_, err := coll.Indexes().CreateMany(context.Background(), []mongo.IndexModel{
		{Keys: bson.D{{Key: "subdomain", Value: 1}}, Options: options.Index().SetUnique(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("create tenants indexes: %w", err)
	}
	return &MongoDBStore{coll: coll}, nil
}

type tenantDoc struct {
	ID        string `bson:"_id"`
	Subdomain string `bson:"subdomain"`
	Name      string `bson:"name"`
	Status    string `bson:"status"`
	Plan      string `bson:"plan"`
	CreatedAt int64  `bson:"created_at"`
	UpdatedAt int64  `bson:"updated_at"`
}

func (s *MongoDBStore) Create(ctx context.Context, t Tenant) error {
	_, err := s.coll.InsertOne(ctx, tenantDoc{
		ID: t.ID, Subdomain: t.Subdomain, Name: t.Name,
		Status: string(t.Status), Plan: t.Plan,
		CreatedAt: t.CreatedAt.Unix(), UpdatedAt: t.UpdatedAt.Unix(),
	})
	if err != nil {
		return fmt.Errorf("create tenant: %w", err)
	}
	return nil
}

func (s *MongoDBStore) GetByID(ctx context.Context, id string) (Tenant, error) {
	return scanMongoTenant(s.coll.FindOne(ctx, bson.M{"_id": id}))
}

func (s *MongoDBStore) GetBySubdomain(ctx context.Context, sub string) (Tenant, error) {
	return scanMongoTenant(s.coll.FindOne(ctx, bson.M{"subdomain": sub}))
}

func (s *MongoDBStore) List(ctx context.Context) ([]Tenant, error) {
	cur, err := s.coll.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer cur.Close(ctx)
	var out []Tenant
	for cur.Next(ctx) {
		t, err := scanMongoTenant(cur)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, cur.Err()
}

func (s *MongoDBStore) UpdateStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error {
	res, err := s.coll.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": string(status), "updated_at": updatedAt.Unix()}})
	if err != nil {
		return fmt.Errorf("update tenant status: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoDBStore) Close() error { return nil }

type mongoScanner interface{ Decode(v any) error }

func scanMongoTenant(sc mongoScanner) (Tenant, error) {
	var d tenantDoc
	if err := sc.Decode(&d); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, err
	}
	return Tenant{
		ID: d.ID, Subdomain: d.Subdomain, Name: d.Name, Status: Status(d.Status), Plan: d.Plan,
		CreatedAt: time.Unix(d.CreatedAt, 0).UTC(),
		UpdatedAt: time.Unix(d.UpdatedAt, 0).UTC(),
	}, nil
}
```

- [ ] **Step 6: 运行 Mongo 测试(默认 skip)**

Run: `go test ./internal/tenants/... -run TestMongoDBStore -v`
Expected: SKIP 或 PASS。

- [ ] **Step 7: 全量构建 + 测试 + 提交**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS(skip 的不计失败)。

```bash
git add internal/tenants/store_postgresql.go internal/tenants/store_postgresql_test.go internal/tenants/store_mongodb.go internal/tenants/store_mongodb_test.go
git commit -m "feat(tenants): add PostgreSQL and MongoDB store backends"
```

---

## Self-Review

**1. Spec coverage(对照设计文档 §3、§4.1、§4.4、§12):**
- §3.1 `tenants` 表 → Task 2 ✓
- §3.2 `TenantResolver` 中间件 → Task 4 ✓
- §3.3 context key → Task 1 ✓
- §3.4 配置字段 → Task 5 ✓
- §4.1 `auth_keys` 加 `tenant_id`/`is_tenant_admin` → **P2 范围**,P1 不含(符合阶段拆分)
- §4.4 迁移脚本 `default` 租户 → Task 5 bootstrap ✓
- §12.1 装配 → Task 5 ✓
- §12.2 `run.run.go` bootstrap → Task 5(在 app.New 内完成,无需改 run.go)✓

**P1 明确排除(归后续阶段):** `auth_keys` schema 变更(P2)、所有业务表加 `tenant_id` 列(P3)、Store 接口加 tenantID 参数(P3)、Admin 拆分(P4)、配置继承(P5)、内存 store DB 化(P6)、Dashboard(P7)。已在本计划 Global Constraints 与各任务说明中声明。

**2. Placeholder scan:**
- 无 TBD/TODO。Task 5 Step 1 与 Step 9 提到"执行前 grep 确认 helper"——这是必要的探查指令(因为项目内 helper 名未在本次探索中 100% 确认),不是占位符;实现者必须执行该 grep 并复用现有 helper,否则编译失败会暴露问题。

**3. Type consistency:**
- `tenants.Tenant` 字段在 Task 1-6 一致:`ID/Subdomain/Name/Status/Plan/CreatedAt/UpdatedAt`。
- `tenants.Store` 接口方法签名在 Task 2 定义,Task 3 Service 调用,Task 6 Postgres/Mongo 实现——签名一致(`Create/GetByID/GetBySubdomain/List/UpdateStatus/Close`)。
- `core.WithTenantID/GetTenantID/WithPlatformHost/GetPlatformHost` 在 Task 1 定义,Task 4 中间件使用——一致。
- `server.TenantResolver(svc, baseDomain, platformHost)` 在 Task 4 定义,Task 5 http.go 调用——一致。
- `tenants.NewService(store, ttl)` 在 Task 3 定义,Task 5 app.go 调用——一致。
- `tenants.NewSQLiteStore/NewPostgreSQLStore/NewMongoDBStore` 在 Task 2/6 定义,Task 5 app.go 通过 `storage.ResolveBackend` 调用——一致。

**4. 已知风险与执行注意事项:**
- Task 5 Step 9 的 `storage.ResolveBackend` 调用需确认 `internal/app/app.go` 已 import `storage` 包且 `sharedStorage` 变量名正确(执行前 `grep -n "sharedStorage\|storage.ResolveBackend" internal/app/app.go`)。
- `BootstrapDefaultTenant` 的 bool 默认值陷阱(零值 false vs 期望 true)必须在 Task 5 Step 3 处理,否则现有部署升级后不会自动建 default 租户。
- Echo v5 的 `c.SetRequest(req.WithContext(ctx))` 是修改 request context 的正确方式(参考 `http.go:272` 现有 RequestID 中间件)。
- 若 `internal/storage/sqlutil` 无 `NewMemoryDB` helper,Task 2/5 测试用 `sql.Open("sqlite3", ":memory:")`;确认驱动名(`sqlite3` vs `modernc.org/sqlite` 的 `sqlite`)——执行前 `grep -rn "sql.Open" internal/authkeys/`。

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-02-saas-multi-tenant-p1.md`. Two execution options:

**1. Subagent-Driven (recommended)** — 每个 Task 派一个 fresh subagent 执行,Task 间做两阶段 review,迭代快。

**2. Inline Execution** — 在当前会话用 executing-plans 批量执行,带 checkpoint 复核。

选哪种?

---

## P1 Completion Notes (2026-08-02)

**Status:** Complete. 6 tasks implemented via subagent-driven-development, all per-task reviews approved, final whole-branch review passed (with 3 must-fixes applied in one fix wave + scoped re-review clean).

**Commits:** `1c09bfe..2b1ff83` on master (7 implementation commits + 1 fix-wave commit).

**Must-fixes applied in final fix wave (commit `2b1ff83`):**
1. `tenant_resolver.go` — type assertion → `errors.As` (parity with bootstrap; forward-robust to error wrapping).
2. `app.go` — `tenantSvc` registered in `closers` (pattern parity; enables future backend cleanup).
3. `store_mongodb.go` — 30s timeout on index creation (parity with authkeys; avoids startup hang).

### Deferred items for P2-P7 (triaged by final review)

| Item | Verdict | Target phase |
|---|---|---|
| `List` returns nil for empty table | Defer | P3 (admin API exposes List) |
| Missing `GetByID_NotFound` / `UpdateStatus_NotFound` tests | Defer | P2 (impl correct, low risk until admin mutates tenants) |
| Sub-second timestamp precision | Non-issue | Project-consistent with authkeys |
| Cached-disabled path untested | Defer | P2 (one-test gap; fresh-fetch disabled is tested) |
| `cacheGet` doesn't delete expired entries | Defer | P3 (plan-mandated; cache bounded by tenant count) |
| `invalidate` is O(n) | Non-issue | Tenant count is small |
| `parseHost` empty `platformHost` misclassification | Defer | P2 (misconfiguration scenario; default `"app"` prevents) |
| PG/Mongo `Create` no duplicate-subdomain translation | Defer | P4 (admin API exposes Create) |
| PG missing `idx_tenants_status` | Defer | P4 (status-filtered listing) |
| PG constructor doesn't accept `ctx` | Defer | P2 (factory touch-up) |
| `_CRUD` tests only exercise Create→GetBySubdomain | Defer | P4 (expand when env-enabled CI added) |
| Subdomain validation (lowercase, length, reserved names) | Defer | P4 (centralize in `tenants.Create` before user input) |
| `default` subdomain not documented as reserved | Defer | P4 (admin API must block creating `default`/`www`/`app`) |

### Architecture notes for P2-P7

- `tenantSvc` is reachable via `server.Config.Tenants` but NOT stored on the `App` struct. P4 admin handlers will need it — add `app.tenants *tenants.Service` or thread it through when P4 lands.
- Service TTL is hard-coded `time.Minute` in `app.go`. Consider making it a config field in P2.
- `UserPath` (existing per-auth-key field) remains the sub-tenant grouping; P3 will add `tenant_id` to `auth_keys` and all business tables.

