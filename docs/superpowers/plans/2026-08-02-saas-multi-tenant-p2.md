# SmartRouter SaaS 多租户改造 — P2: 认证与两级 Key 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 auth middleware 按租户隔离与角色强制访问:managed key 必须匹配 Host 解析的 tenantID,`is_tenant_admin` key 才能访问 `/admin/*`,master key 在平台 host 是平台管理员、在租户 host 是超级管理员(审计),master key 未配置时平台 host `/admin/*` 返回 503 而非全开放。

**Architecture:** 给 `auth_keys` 表加 `tenant_id` + `is_tenant_admin` 列(三个后端),`AuthKey`/`AuthenticationResult`/`CreateInput` 结构体加对应字段,`Service.Authenticate` 返回新字段,`auth.go` 中间件在现有认证后追加"租户匹配 + 角色路径 + master key host 行为"三层强制。现有 `POST /admin/auth-keys` 端点的请求体接受 `tenant_id`/`is_tenant_admin`,平台管理员(master key)可签发租户管理员 key。Admin handler 拆分(Platform vs Tenant)与租户管理员自动 scope 创建 key 留给 P4。

**Tech Stack:** Go 1.x, Echo v5, `database/sql` (SQLite), `github.com/jackc/pgx/v5/pgxpool` (Postgres), `go.mongodb.org/mongo-driver/v2/mongo` (Mongo), `github.com/stretchr/testify`。

## Global Constraints

- 遵循项目约定:每个源文件配同包同基名 `_test.go`,使用 `testify`(`require`/`assert`)。
- 模型/路径命名为 `smartrouter`(非 `gomodel`);`authkeys.TokenPrefix = "sk_gom_"` 是既有常量,不在 P2 改名。
- P2 **不**改 `authkeys.Store` 接口签名(那是 P3);**不**拆分 admin handler(那是 P4);**不**给 `auth_keys` 以外的业务表加 `tenant_id`(那是 P3);**不**改 provider 配置。
- 迁移遵循 per-store 模式:`CREATE TABLE IF NOT EXISTS` 已存在,用 `ALTER TABLE ADD COLUMN` 容错重复列(参考 `internal/authkeys/store_sqlite.go:45-53`)。现有 `auth_keys` 行回填 `tenant_id='default'`、`is_tenant_admin=0`。
- `base_domain` 为空时(开发模式),TenantResolver no-op(P1 行为),tenant 匹配检查跳过(`ctx.tenantID` 为空时不强制),保证 localhost 与现有测试不中断。
- 租户匹配检查仅在 `ctx.tenantID != ""` 且 `authResult.TenantID != ""` 且两者不同时触发 401;任一为空则跳过(向后兼容 legacy key 与 dev 模式)。
- master key 在租户 host 上有效(超级管理员),但审计日志必须记录 `AuthMethodMasterKey`(现有行为,不改)。
- 现有 `cfg.MasterKey == ""` 时 `/admin/*` 加入 skipPaths 的逻辑(`internal/server/http.go:207-209`)在 P2 **废止**,改为中间件返回 503。

## File Structure

| 文件 | 职责 | 新建/修改 |
|---|---|---|
| `internal/authkeys/types.go` | `AuthKey` 加 `TenantID`/`IsTenantAdmin` 字段;`CreateInput` 加同字段 | 修改 |
| `internal/authkeys/store.go` | `normalizeCreateInput` 校验 `IsTenantAdmin→TenantID 必填` | 修改 |
| `internal/authkeys/store_sqlite.go` | 迁移加列 + INSERT/scan 新字段 | 修改 |
| `internal/authkeys/store_postgresql.go` | INSERT/scan 新字段 | 修改 |
| `internal/authkeys/store_mongodb.go` | InsertOne/Decode 新字段 | 修改 |
| `internal/authkeys/service.go` | `Authenticate` 返回新字段;`Create` 传递新字段到 `AuthKey` | 修改 |
| `internal/server/auth.go` | 中间件追加租户匹配 + 角色路径 + master key host 行为 + 503 | 修改 |
| `internal/server/http.go` | 删除 `MasterKey==""` 的 `/admin/*` skipPaths 逻辑 | 修改 |
| `internal/admin/handler_authkeys.go` | `createAuthKeyRequest` 加 `TenantID`/`IsTenantAdmin` 字段并传递 | 修改 |
| `internal/authkeys/types_test.go`、`store_sqlite_test.go`、`service_test.go`、`internal/server/auth_test.go`、`internal/admin/handler_authkeys_test.go` | 对应测试 | 修改/新建 |

---

## Task 1: auth_keys schema 迁移 + 结构体字段

**Files:**
- Modify: `internal/authkeys/types.go`(`AuthKey` + `CreateInput` 加字段)
- Modify: `internal/authkeys/store.go`(`normalizeCreateInput` 加校验)
- Modify: `internal/authkeys/store_sqlite.go`(加列迁移)
- Test: `internal/authkeys/types_test.go`、`internal/authkeys/store_sqlite_test.go`

**Interfaces:**
- Consumes: P1 的 `tenants` 包(仅用于注释引用,无代码依赖)。
- Produces: `AuthKey.TenantID string`、`AuthKey.IsTenantAdmin bool`、`CreateInput.TenantID string`、`CreateInput.IsTenantAdmin bool`;SQLite `auth_keys` 表新增 `tenant_id TEXT` + `is_tenant_admin INTEGER NOT NULL DEFAULT 0` 列。

- [ ] **Step 1: 写结构体字段的失败测试**

在 `internal/authkeys/types_test.go` 追加(若文件不存在则新建):

```go
package authkeys

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthKey_TenantFields(t *testing.T) {
	now := time.Now().UTC()
	k := AuthKey{
		ID:            "k-1",
		Name:          "n",
		RedactedValue: "sk_gom_...",
		SecretHash:    "h",
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
		TenantID:      "default",
		IsTenantAdmin: true,
	}
	require.Equal(t, "default", k.TenantID)
	require.True(t, k.IsTenantAdmin)
}

func TestCreateInput_TenantFields(t *testing.T) {
	in := CreateInput{Name: "n", TenantID: "default", IsTenantAdmin: true}
	normalized, err := normalizeCreateInput(in)
	require.NoError(t, err)
	require.Equal(t, "default", normalized.TenantID)
	require.True(t, normalized.IsTenantAdmin)
}

func TestNormalizeCreateInput_AdminKeyRequiresTenantID(t *testing.T) {
	_, err := normalizeCreateInput(CreateInput{Name: "n", IsTenantAdmin: true})
	require.Error(t, err)
	require.True(t, IsValidationError(err))
}

func TestNormalizeCreateInput_APIKeyWithoutTenantID(t *testing.T) {
	normalized, err := normalizeCreateInput(CreateInput{Name: "n"})
	require.NoError(t, err)
	require.Equal(t, "", normalized.TenantID)
	require.False(t, normalized.IsTenantAdmin)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/authkeys/... -run "TestAuthKey_TenantFields|TestCreateInput_TenantFields|TestNormalizeCreateInput" -v`
Expected: 编译失败(`TenantID`/`IsTenantAdmin` 字段未定义)。

- [ ] **Step 3: 加字段到 `AuthKey` 与 `CreateInput`**

在 `internal/authkeys/types.go` 的 `AuthKey` 结构体(`UpdatedAt` 字段之后)加:

```go
	// TenantID binds this key to a tenant. Empty means no tenant binding
	// (legacy / dev mode). Post-P2 migration all keys should have a value.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	// IsTenantAdmin allows this key to access /admin/* on its tenant host.
	// Platform admin creates these; tenant admins create regular API keys.
	IsTenantAdmin bool `json:"is_tenant_admin,omitempty" bson:"is_tenant_admin,omitempty"`
```

在 `CreateInput` 结构体(`ExpiresAt` 字段之后)加:

```go
	TenantID      string
	IsTenantAdmin bool
```

- [ ] **Step 4: 加校验到 `normalizeCreateInput`**

在 `internal/authkeys/store.go` 的 `normalizeCreateInput` 函数末尾(`return input, nil` 之前)加:

```go
	if input.IsTenantAdmin && strings.TrimSpace(input.TenantID) == "" {
		return CreateInput{}, newValidationError("tenant_id is required when is_tenant_admin is true", nil)
	}
	input.TenantID = strings.TrimSpace(input.TenantID)
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/authkeys/... -run "TestAuthKey_TenantFields|TestCreateInput_TenantFields|TestNormalizeCreateInput" -v`
Expected: 4 个测试 PASS。

- [ ] **Step 6: 写 SQLite 迁移与 round-trip 失败测试**

在 `internal/authkeys/store_sqlite_test.go` 追加(复用现有 `newTestSQLiteStore` helper,执行前 `grep -n "func newTestSQLiteStore\|func newSQLiteStore" internal/authkeys/*_test.go` 确认 helper 名):

```go
func TestSQLiteStore_CreateWithTenantFields(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	key := AuthKey{
		ID:            "k-tenant-1",
		Name:          "tenant admin",
		RedactedValue: "sk_gom_...",
		SecretHash:    "hash-1",
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
		TenantID:      "default",
		IsTenantAdmin: true,
	}
	require.NoError(t, store.Create(ctx, key))

	list, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "default", list[0].TenantID)
	require.True(t, list[0].IsTenantAdmin)
}
```

- [ ] **Step 7: 运行测试确认失败**

Run: `go test ./internal/authkeys/... -run TestSQLiteStore_CreateWithTenantFields -v`
Expected: 失败——`TenantID` 为空(列不存在,INSERT 未写入,scan 未读取)。

- [ ] **Step 8: 实现 SQLite 迁移 + INSERT/scan 新字段**

在 `internal/authkeys/store_sqlite.go` 的 `NewSQLiteStore` 函数中,现有 `migrations` 切片(`store_sqlite.go:45-48`)追加两条:

```go
	migrations := []string{
		`ALTER TABLE auth_keys ADD COLUMN user_path TEXT`,
		`ALTER TABLE auth_keys ADD COLUMN labels JSON`,
		`ALTER TABLE auth_keys ADD COLUMN tenant_id TEXT`,
		`ALTER TABLE auth_keys ADD COLUMN is_tenant_admin INTEGER NOT NULL DEFAULT 0`,
	}
```

在 `Create` 方法的 INSERT 语句中加 `tenant_id`、`is_tenant_admin` 列与占位符(参考现有 `store_sqlite.go:84-87` 的 INSERT),并在 `ExecContext` 参数中加 `sqlutil.NullableString(key.TenantID)` 与 `boolToSQLite(key.IsTenantAdmin)`。

在 `scanSQLiteAuthKey` 函数中加 `tenantID sql.NullString`、`isTenantAdmin int` 的 Scan 目标,并在末尾赋值 `key.TenantID = sqlutil.StringFromNullable(tenantID)`、`key.IsTenantAdmin = isTenantAdmin != 0`。同步更新 `List` 的 SELECT 列列表(加 `tenant_id, is_tenant_admin`)。

执行前 `grep -n "boolToSQLite\|sqlutil.NullableString\|sqlutil.StringFromNullable" internal/authkeys/store_sqlite.go` 确认 helper 已存在。

- [ ] **Step 9: 运行测试确认通过**

Run: `go test ./internal/authkeys/... -run TestSQLiteStore -v`
Expected: 全部 PASS(含新测试 + 现有 SQLite 测试)。

- [ ] **Step 10: 全量构建 + 提交**

Run: `go build ./...`
Expected: 成功。

```bash
git add internal/authkeys/types.go internal/authkeys/types_test.go internal/authkeys/store.go internal/authkeys/store_sqlite.go internal/authkeys/store_sqlite_test.go
git commit -m "feat(authkeys): add tenant_id and is_tenant_admin fields with SQLite migration"
```

---

## Task 2: Postgres 与 Mongo Store 新字段

**Files:**
- Modify: `internal/authkeys/store_postgresql.go`(INSERT/scan 新字段)
- Modify: `internal/authkeys/store_mongodb.go`(InsertOne/Decode 新字段)
- Test: `internal/authkeys/store_postgresql_test.go`、`internal/authkeys/store_mongodb_test.go`

**Interfaces:**
- Consumes: Task 1 的 `AuthKey.TenantID`/`IsTenantAdmin` 字段。
- Produces: PG/Mongo 后端支持新字段的读写。

- [ ] **Step 1: 写 PG 失败测试(skip-by-default)**

在 `internal/authkeys/store_postgresql_test.go` 追加(复用现有 skip helper 与 pool 构造,执行前 `grep -n "skipIfNoPostgres\|SMARTROUTER_TEST_POSTGRES_URL\|newTestPool" internal/authkeys/store_postgresql_test.go` 确认):

```go
func TestPostgreSQLStore_CreateWithTenantFields(t *testing.T) {
	skipIfNoPostgres(t)
	store := newTestPostgreSQLStore(t) // 复用现有 helper
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, AuthKey{
		ID: "k-pg-tenant", Name: "pg admin", RedactedValue: "sk_gom_...",
		SecretHash: "hash-pg", Enabled: true, CreatedAt: now, UpdatedAt: now,
		TenantID: "default", IsTenantAdmin: true,
	}))
	list, err := store.List(ctx)
	require.NoError(t, err)
	var found *AuthKey
	for i := range list {
		if list[i].ID == "k-pg-tenant" {
			found = &list[i]
			break
		}
	}
	require.NotNil(t, found)
	require.Equal(t, "default", found.TenantID)
	require.True(t, found.IsTenantAdmin)
}
```

**说明**:若 `newTestPostgreSQLStore` 不存在,复用 `store_postgresql_test.go` 中现有的 store 构造逻辑(参考 `store_sqlite_test.go` 的 `newTestSQLiteStore` 模式)。执行前 grep 确认实际 helper 名。

- [ ] **Step 2: 实现 PG INSERT/scan 新字段**

在 `internal/authkeys/store_postgresql.go` 中:
- `Create` 的 INSERT 语句加 `tenant_id, is_tenant_admin` 列与 `$N, $N+1` 占位符,`Exec` 参数加 `sqlutil.NullableString(key.TenantID)` 与 `key.IsTenantAdmin`。
- `scanPGAuthKey` 加 `tenantID sql.NullString`、`isTenantAdmin bool` 的 Scan 目标,赋值 `key.TenantID`、`key.IsTenantAdmin`。
- `List` 的 SELECT 列列表加 `tenant_id, is_tenant_admin`。
- `UpdateLabels` 不涉及新字段,无需改。

执行前 `grep -n "func.*scanPGAuthKey\|INSERT INTO auth_keys\|SELECT.*FROM auth_keys" internal/authkeys/store_postgresql.go` 定位精确行。

- [ ] **Step 3: 运行 PG 测试(默认 skip)**

Run: `go test ./internal/authkeys/... -run TestPostgreSQLStore_CreateWithTenantFields -v`
Expected: SKIP(无 env)或 PASS(有 env)。

- [ ] **Step 4: 写 Mongo 失败测试(skip-by-default)**

在 `internal/authkeys/store_mongodb_test.go` 追加(复用现有 skip helper):

```go
func TestMongoDBStore_CreateWithTenantFields(t *testing.T) {
	skipIfNoMongo(t)
	store := newTestMongoDBStore(t) // 复用现有 helper
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, AuthKey{
		ID: "k-mg-tenant", Name: "mg admin", RedactedValue: "sk_gom_...",
		SecretHash: "hash-mg", Enabled: true, CreatedAt: now, UpdatedAt: now,
		TenantID: "default", IsTenantAdmin: true,
	}))
	list, err := store.List(ctx)
	require.NoError(t, err)
	var found *AuthKey
	for i := range list {
		if list[i].ID == "k-mg-tenant" {
			found = &list[i]
			break
		}
	}
	require.NotNil(t, found)
	require.Equal(t, "default", found.TenantID)
	require.True(t, found.IsTenantAdmin)
}
```

- [ ] **Step 5: 实现 Mongo InsertOne/Decode 新字段**

在 `internal/authkeys/store_mongodb.go` 中:
- 找到 auth key 的 BSON 文档结构(可能是 `authKeyDoc` 或类似,`grep -n "type.*Doc\|bson:" internal/authkeys/store_mongodb.go`)。
- 加 `TenantID string \`bson:"tenant_id,omitempty"\`` 与 `IsTenantAdmin bool \`bson:"is_tenant_admin,omitempty"\`` 字段。
- `Create` 的文档构造加这两字段。
- `scanMongoAuthKey`(或 Decode 目标)加这两字段赋值到 `AuthKey`。

- [ ] **Step 6: 运行 Mongo 测试(默认 skip)**

Run: `go test ./internal/authkeys/... -run TestMongoDBStore_CreateWithTenantFields -v`
Expected: SKIP 或 PASS。

- [ ] **Step 7: 全量构建 + 测试 + 提交**

Run: `go build ./... && go test ./internal/authkeys/... -v`
Expected: 全部 PASS(PG/Mongo skip 不计失败)。

```bash
git add internal/authkeys/store_postgresql.go internal/authkeys/store_postgresql_test.go internal/authkeys/store_mongodb.go internal/authkeys/store_mongodb_test.go
git commit -m "feat(authkeys): persist tenant_id and is_tenant_admin in PostgreSQL and MongoDB backends"
```

---

## Task 3: Service 层传递新字段

**Files:**
- Modify: `internal/authkeys/service.go`(`Authenticate` 返回新字段;`Create` 传递新字段)
- Test: `internal/authkeys/service_test.go`

**Interfaces:**
- Consumes: Task 1 的 `AuthKey.TenantID`/`IsTenantAdmin`、`CreateInput.TenantID`/`IsTenantAdmin`。
- Produces: `AuthenticationResult.TenantID string`、`AuthenticationResult.IsTenantAdmin bool`;`Service.Authenticate` 返回值包含新字段;`Service.Create` 把 `CreateInput` 的新字段写入 `AuthKey`。

- [ ] **Step 1: 写 Service 失败测试**

在 `internal/authkeys/service_test.go` 追加(复用现有 test store/helper,执行前 `grep -n "func newTestService\|type.*Store.*struct\|func.*Authenticate" internal/authkeys/service_test.go` 确认模式):

```go
func TestService_Authenticate_ReturnsTenantFields(t *testing.T) {
	svc := newTestService(t) // 复用现有 helper;若不存在,构造 Service with in-memory stub store
	ctx := context.Background()
	now := time.Now().UTC()
	key := AuthKey{
		ID: "k-auth", Name: "admin", RedactedValue: "sk_gom_...",
		SecretHash: hashSecret("secret-auth"), Enabled: true,
		CreatedAt: now, UpdatedAt: now,
		TenantID: "default", IsTenantAdmin: true,
	}
	require.NoError(t, seedKey(ctx, svc, key)) // 复用现有 seed helper

	result, err := svc.Authenticate(ctx, TokenPrefix+"secret-auth")
	require.NoError(t, err)
	require.Equal(t, "default", result.TenantID)
	require.True(t, result.IsTenantAdmin)
}

func TestService_Create_PersistsTenantFields(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	issued, err := svc.Create(ctx, CreateInput{
		Name:          "new admin",
		TenantID:      "default",
		IsTenantAdmin: true,
	})
	require.NoError(t, err)
	require.Equal(t, "default", issued.TenantID)
	require.True(t, issued.IsTenantAdmin)

	// 验证 Authenticate 能取回
	result, err := svc.Authenticate(ctx, issued.Value)
	require.NoError(t, err)
	require.Equal(t, "default", result.TenantID)
	require.True(t, result.IsTenantAdmin)
}
```

**说明**:执行前 `grep -n "hashSecret\|TokenPrefix\|seedKey\|newTestService" internal/authkeys/service_test.go` 确认 helper 名;若 `newTestService`/`seedKey` 不存在,在测试文件内按现有模式构造。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/authkeys/... -run "TestService_Authenticate_ReturnsTenantFields|TestService_Create_PersistsTenantFields" -v`
Expected: 失败——`AuthenticationResult` 无 `TenantID`/`IsTenantAdmin` 字段。

- [ ] **Step 3: 加字段到 `AuthenticationResult` 并更新 `Authenticate`/`authenticateKey`**

在 `internal/authkeys/service.go` 的 `AuthenticationResult` 结构体(`service.go:32-36`)加:

```go
type AuthenticationResult struct {
	ID            string
	UserPath      string
	Labels        []string
	TenantID      string
	IsTenantAdmin bool
}
```

在 `authenticateKey` 函数(`service.go:322-337`)的返回值中加:

```go
	return AuthenticationResult{
		ID:            key.ID,
		UserPath:      strings.TrimSpace(key.UserPath),
		Labels:        key.Labels,
		TenantID:      strings.TrimSpace(key.TenantID),
		IsTenantAdmin: key.IsTenantAdmin,
	}, nil
```

- [ ] **Step 4: 更新 `Service.Create` 传递新字段**

在 `internal/authkeys/service.go` 的 `Create` 方法中,构造 `AuthKey` 的字面量(`service.go:176-188`)加:

```go
	key := AuthKey{
		ID:            uuid.NewString(),
		Name:          normalized.Name,
		Description:   normalized.Description,
		UserPath:      normalized.UserPath,
		Labels:        normalized.Labels,
		RedactedValue: redactedValue,
		SecretHash:    secretHash,
		Enabled:       true,
		ExpiresAt:     normalized.ExpiresAt,
		CreatedAt:     now,
		UpdatedAt:     now,
		TenantID:      normalized.TenantID,
		IsTenantAdmin: normalized.IsTenantAdmin,
	}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/authkeys/... -run "TestService_Authenticate_ReturnsTenantFields|TestService_Create_PersistsTenantFields" -v`
Expected: 2 个测试 PASS。

- [ ] **Step 6: 跑全包测试 + 提交**

Run: `go test ./internal/authkeys/... -v`
Expected: 全部 PASS。

```bash
git add internal/authkeys/service.go internal/authkeys/service_test.go
git commit -m "feat(authkeys): return tenant fields from Authenticate and persist in Create"
```

---

## Task 4: Auth 中间件强制(租户匹配 + 角色 + master key host 行为 + 503)

**Files:**
- Modify: `internal/server/auth.go`(中间件追加强制逻辑)
- Modify: `internal/server/http.go`(删除 `MasterKey==""` 的 `/admin/*` skipPaths)
- Test: `internal/server/auth_test.go`

**Interfaces:**
- Consumes: Task 3 的 `AuthenticationResult.TenantID`/`IsTenantAdmin`;P1 的 `core.GetTenantID`/`core.GetPlatformHost`。
- Produces: 中间件在认证成功后追加三层强制;`/admin/*` 在平台 host 且 master key 未配置时返回 503。

**强制规则(精确):**
1. **路径分类**:`/admin/*` 前缀 = admin 路径;其余 = 推理路径。
2. **master key 命中**:admin 路径→放行(平台 host 是平台管理员,租户 host 是超级管理员,审计已记录);推理路径→放行。
3. **managed key 命中 + admin 路径**:
   - `!IsTenantAdmin` → 403 `insufficient_role`。
   - `IsTenantAdmin` 且 `core.GetPlatformHost(ctx)` → 401 `key_not_allowed_on_platform_host`。
   - `IsTenantAdmin` 且租户 host 且 `ctx.tenantID != ""` 且 `result.TenantID != ctx.tenantID` → 401 `key_tenant_mismatch`。
   - 否则 → 放行。
4. **managed key 命中 + 推理路径**:
   - `ctx.tenantID != ""` 且 `result.TenantID != ""` 且 `result.TenantID != ctx.tenantID` → 401 `key_tenant_mismatch`。
   - 否则 → 放行。
5. **无认证机制(master key 空 且 authenticator 未启用)**:
   - admin 路径 且 `core.GetPlatformHost(ctx)` → 503 `master_key_not_configured`。
   - 否则 → 放行(开发模式,向后兼容)。

- [ ] **Step 1: 写中间件强制的失败测试**

在 `internal/server/auth_test.go` 追加(复用现有 test helper,执行前 `grep -n "func newTestEcho\|func doAuthReq\|func.*AuthMiddleware" internal/server/auth_test.go` 确认模式)。若无现成 helper,参考 `tenant_resolver_test.go` 的 direct-chain-call 模式构造:

```go
func TestAuthMiddleware_TenantMismatch_OnV1(t *testing.T) {
	// managed key tenant_id="tenant-a", 但 ctx tenantID="tenant-b" → 401
	authenticator := stubAuthenticator{result: authkeys.AuthenticationResult{
		ID: "k-1", TenantID: "tenant-a",
	}}
	mw := AuthMiddlewareWithAuthenticator("", authenticator, nil)
	ctx := core.WithTenantID(context.Background(), "tenant-b")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk_gom_test")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	handler := mw(func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	err := handler(c)
	require.Error(t, err)
	// writeGatewayError 把错误写入 response;断言状态码
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "key_tenant_mismatch")
}

func TestAuthMiddleware_APIKeyOnAdmin_403(t *testing.T) {
	authenticator := stubAuthenticator{result: authkeys.AuthenticationResult{
		ID: "k-2", TenantID: "default", IsTenantAdmin: false,
	}}
	mw := AuthMiddlewareWithAuthenticator("", authenticator, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	req.Header.Set("Authorization", "Bearer sk_gom_test")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	handler := mw(func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	_ = handler(c)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "insufficient_role")
}

func TestAuthMiddleware_TenantAdminOnPlatformHost_401(t *testing.T) {
	authenticator := stubAuthenticator{result: authkeys.AuthenticationResult{
		ID: "k-3", TenantID: "default", IsTenantAdmin: true,
	}}
	mw := AuthMiddlewareWithAuthenticator("", authenticator, nil)
	ctx := core.WithPlatformHost(context.Background(), true)
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	req.Header.Set("Authorization", "Bearer sk_gom_test")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	handler := mw(func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	_ = handler(c)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "key_not_allowed_on_platform_host")
}

func TestAuthMiddleware_TenantAdminOnTenantHost_OK(t *testing.T) {
	authenticator := stubAuthenticator{result: authkeys.AuthenticationResult{
		ID: "k-4", TenantID: "default", IsTenantAdmin: true,
	}}
	mw := AuthMiddlewareWithAuthenticator("", authenticator, nil)
	ctx := core.WithTenantID(context.Background(), "default")
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	req.Header.Set("Authorization", "Bearer sk_gom_test")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	handler := mw(func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	_ = handler(c)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthMiddleware_MasterKeyEmpty_PlatformHostAdmin_503(t *testing.T) {
	mw := AuthMiddlewareWithAuthenticator("", nil, nil) // no master key, no authenticator
	ctx := core.WithPlatformHost(context.Background(), true)
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	handler := mw(func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	_ = handler(c)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "master_key_not_configured")
}

func TestAuthMiddleware_MasterKeyOnTenantHost_OK(t *testing.T) {
	mw := AuthMiddlewareWithAuthenticator("secret-master", nil, nil)
	ctx := core.WithTenantID(context.Background(), "tenant-a")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer secret-master")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	handler := mw(func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	_ = handler(c)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthMiddleware_DevMode_NoPlatformHost_AdminAllowed(t *testing.T) {
	// base_domain 空 → isPlatformHost=false, tenantID="" → dev 模式 /admin/* 仍开放(向后兼容)
	mw := AuthMiddlewareWithAuthenticator("", nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	handler := mw(func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	_ = handler(c)
	require.Equal(t, http.StatusOK, rec.Code)
}
```

在测试文件中定义 `stubAuthenticator`:

```go
type stubAuthenticator struct {
	result authkeys.AuthenticationResult
	err    error
}

func (s stubAuthenticator) Enabled() bool { return true }
func (s stubAuthenticator) Authenticate(_ context.Context, _ string) (authkeys.AuthenticationResult, error) {
	return s.result, s.err
}
```

**说明**:测试用 direct-chain-call 模式(`echo.New().NewContext(req, rec)` + `handler(c)`),参考 `tenant_resolver_test.go`。`writeGatewayError` 的具体行为(是否写 `rec.Code`)执行前 `grep -n "func writeGatewayError" internal/server/*.go` 确认;若它不直接写 HTTP 状态码而是返回 error 给 Echo,测试需调整断言方式(用 `echo.HTTPError` 断言)。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server/... -run TestAuthMiddleware -v`
Expected: 多个测试失败(强制逻辑未实现,全部放行)。

- [ ] **Step 3: 实现中间件强制逻辑**

在 `internal/server/auth.go` 的 `AuthMiddlewareWithAuthenticator` 中:

**3a. 修改"无认证机制"分支**(`auth.go:37-40`),在 `return next(c)` 前加平台 host admin 503 检查:

```go
			if masterKey == "" && (authenticator == nil || !authenticator.Enabled()) {
				if isAdminPath(c.Request().URL.Path) && core.GetPlatformHost(c.Request().Context()) {
					err := writeGatewayError(c, &core.GatewayError{
						Type:       core.ErrorType("master_key_not_configured"),
						Message:    "master key not configured; admin API unavailable on platform host",
						StatusCode: http.StatusServiceUnavailable,
					}.WithCode("master_key_not_configured"))
					return err
				}
				auditlog.EnrichEntryWithAuthMethod(c, auditlog.AuthMethodNoKey)
				return next(c)
			}
```

**3b. 在 master key 命中分支**(`auth.go:73-76`),保持放行但无需改动(master key 全通)。

**3c. 在 managed key 认证成功分支**(`auth.go:81-84`),在 `applyAuthKeyResult` 后、`return next(c)` 前加强制:

```go
			if err == nil {
				applyAuthKeyResult(c, authResult, userPathHeaderName)
				if enforcerErr := enforceTenantAndRole(c, authResult); enforcerErr != nil {
					return writeGatewayError(c, enforcerErr)
				}
				return next(c)
			}
```

**3d. 新增 helper 函数**:

```go
func isAdminPath(path string) bool {
	return strings.HasPrefix(path, "/admin/")
}

// enforceTenantAndRole applies the two-tier key model after successful
// managed-key authentication. Returns nil to allow, or a *core.GatewayError
// to reject.
func enforceTenantAndRole(c *echo.Context, result authkeys.AuthenticationResult) *core.GatewayError {
	ctx := c.Request().Context()
	ctxTenantID := core.GetTenantID(ctx)
	isPlatform := core.GetPlatformHost(ctx)
	path := c.Request().URL.Path

	if isAdminPath(path) {
		if !result.IsTenantAdmin {
			return &core.GatewayError{
				Type:       core.ErrorType("insufficient_role"),
				Message:    "API key does not have admin access",
				StatusCode: http.StatusForbidden,
			}.WithCode("insufficient_role")
		}
		if isPlatform {
			return &core.GatewayError{
				Type:       core.ErrorType("key_not_allowed_on_platform_host"),
				Message:    "tenant admin key not allowed on platform host",
				StatusCode: http.StatusUnauthorized,
			}.WithCode("key_not_allowed_on_platform_host")
		}
		if ctxTenantID != "" && result.TenantID != "" && result.TenantID != ctxTenantID {
			return &core.GatewayError{
				Type:       core.ErrorType("key_tenant_mismatch"),
				Message:    "auth key does not belong to this tenant",
				StatusCode: http.StatusUnauthorized,
			}.WithCode("key_tenant_mismatch")
		}
		return nil
	}
	// 推理路径
	if ctxTenantID != "" && result.TenantID != "" && result.TenantID != ctxTenantID {
		return &core.GatewayError{
			Type:       core.ErrorType("key_tenant_mismatch"),
			Message:    "auth key does not belong to this tenant",
			StatusCode: http.StatusUnauthorized,
		}.WithCode("key_tenant_mismatch")
	}
	return nil
}
```

**说明**:`core.GatewayError` 是结构体,直接 `&core.GatewayError{Type, Message, StatusCode}` 构造,`.WithCode(code)` 设错误码(已确认 `internal/core/errors.go` 的 API)。`writeGatewayError(c, err)` 会用 `gatewayErr.HTTPStatusCode()` 写 HTTP 状态码与 JSON body(已确认 `internal/server/error_support.go`),所以测试断言 `rec.Code` 与 `rec.Body` 有效。

- [ ] **Step 4: 删除 http.go 的 `/admin/*` skipPaths 逻辑**

在 `internal/server/http.go` 找到 `cfg.MasterKey == "" && cfg.AdminEndpointsEnabled` 分支(`http.go:207-209`),删除或注释掉把 `/admin/*` 加入 `authSkipPaths` 的逻辑:

```go
	// P2: master key 未配置时不再把 /admin/* 加入 skipPaths。
	// 中间件会在平台 host 上返回 503,在开发模式(isPlatformHost=false)下放行。
	// if cfg != nil && cfg.MasterKey == "" && cfg.AdminEndpointsEnabled && cfg.AdminHandler != nil {
	// 	authSkipPaths = append(authSkipPaths, "/admin/*")
	// }
```

保留 `DashboardHandler` 的 skipPaths(`/admin/dashboard`、`/admin/static/*`)——dashboard HTML 页面本身无需鉴权,API 调用仍走中间件。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/server/... -run TestAuthMiddleware -v`
Expected: 7 个测试全部 PASS。

- [ ] **Step 6: 跑现有 server 包测试确保无回归**

Run: `go test ./internal/server/... -v`
Expected: 全部 PASS(含现有测试)。若有现有测试依赖"`/admin/*` 无 master key 时 skipPaths 放行"的行为,需更新该测试为"dev 模式(`isPlatformHost=false`)仍放行"——执行前 `grep -rn "MasterKey == \"\"\|admin.*skip\|skipPaths" internal/server/*_test.go` 排查。

- [ ] **Step 7: 全量构建 + 测试 + 提交**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/server/auth.go internal/server/auth_test.go internal/server/http.go
git commit -m "feat(server): enforce tenant match and two-tier key roles in auth middleware"
```

---

## Task 5: Admin auth-keys 端点接受新字段

**Files:**
- Modify: `internal/admin/handler_authkeys.go`(`createAuthKeyRequest` 加字段 + 传递)
- Test: `internal/admin/handler_authkeys_test.go`

**Interfaces:**
- Consumes: Task 1 的 `CreateInput.TenantID`/`IsTenantAdmin`;Task 3 的 `Service.Create`。
- Produces: `POST /admin/auth-keys` 请求体接受 `tenant_id` 与 `is_tenant_admin` 字段。

**设计**:P2 不拆分 admin handler(那是 P4)。平台管理员用 master key 在平台 host 调用此端点,显式指定 `tenant_id` + `is_tenant_admin=true` 签发租户管理员 key。租户管理员在租户 host 调用此端点创建 API key 的"自动 scope 到当前租户"行为留给 P4。

- [ ] **Step 1: 写端点失败测试**

在 `internal/admin/handler_authkeys_test.go` 追加(复用现有 test helper,执行前 `grep -n "func.*CreateAuthKey\|newTestHandler\|mockAuthKeys\|func TestCreate" internal/admin/handler_authkeys_test.go` 确认模式):

```go
func TestCreateAuthKey_WithTenantFields(t *testing.T) {
	h := newTestHandler(t) // 复用现有 helper;若不存在,按现有模式构造带 mock authKeys service
	body := `{"name":"tenant admin","tenant_id":"default","is_tenant_admin":true}`
	req := httptest.NewRequest(http.MethodPost, "/admin/auth-keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.CreateAuthKey(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)

	// 断言 service 收到了 tenant 字段(通过 mock 捕获 CreateInput 或解析响应体)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "default", resp["tenant_id"])
	require.Equal(t, true, resp["is_tenant_admin"])
}
```

**说明**:执行前确认 `newTestHandler` 与 mock authKeys service 的构造方式。若现有测试用真实 `authkeys.Service` + 内存 store,直接断言响应体;若用 mock,断言 mock 收到的 `CreateInput.TenantID`/`IsTenantAdmin`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/admin/... -run TestCreateAuthKey_WithTenantFields -v`
Expected: 失败——请求体的 `tenant_id`/`is_tenant_admin` 未被读取,响应不含这些字段。

- [ ] **Step 3: 实现:加字段到请求结构并传递**

在 `internal/admin/handler_authkeys.go` 的 `createAuthKeyRequest` 结构体(`handler_authkeys.go:17-23`)加:

```go
type createAuthKeyRequest struct {
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	UserPath      string     `json:"user_path,omitempty"`
	Labels        []string   `json:"labels,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	TenantID      string     `json:"tenant_id,omitempty"`
	IsTenantAdmin bool       `json:"is_tenant_admin,omitempty"`
}
```

在 `CreateAuthKey` 的 `CreateInput` 构造(`handler_authkeys.go:52-58`)加:

```go
	issued, err := h.authKeys.Create(c.Request().Context(), authkeys.CreateInput{
		Name:          req.Name,
		Description:   req.Description,
		UserPath:      userPath,
		Labels:        req.Labels,
		ExpiresAt:     req.ExpiresAt,
		TenantID:      req.TenantID,
		IsTenantAdmin: req.IsTenantAdmin,
	})
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/admin/... -run TestCreateAuthKey -v`
Expected: 新测试 + 现有 CreateAuthKey 测试全部 PASS。

- [ ] **Step 5: 跑全包测试 + 提交**

Run: `go test ./internal/admin/... -v`
Expected: 全部 PASS。

```bash
git add internal/admin/handler_authkeys.go internal/admin/handler_authkeys_test.go
git commit -m "feat(admin): accept tenant_id and is_tenant_admin in create-auth-key endpoint"
```

---

## Task 6: 端到端集成测试

**Files:**
- Create: `internal/server/auth_tenant_integration_test.go`
- (无生产代码改动)

**目标**:验证两级 Key 模型在完整 Echo + 真实 authkeys.Service + 真实 tenants.Service 下端到端工作。

- [ ] **Step 1: 写端到端测试**

创建 `internal/server/auth_tenant_integration_test.go`:

```go
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
	"smartrouter/internal/tenants"
)

// TestTwoTierKeyModel_EndToEnd 验证完整的两级 Key 流程:
// 1. 平台管理员(master key)在平台 host 签发租户管理员 key
// 2. 租户管理员 key 在租户 host 访问 /admin/* 成功
// 3. 租户管理员 key 在租户 host 签发租户 API key
// 4. 租户 API key 在租户 host 访问 /v1/* 成功
// 5. 租户 API key 在租户 host 访问 /admin/* → 403
// 6. 租户管理员 key 在平台 host 访问 /admin/* → 401
// 7. 租户 A 的 API key 在租户 B 的 host 访问 /v1/* → 401 key_tenant_mismatch
func TestTwoTierKeyModel_EndToEnd(t *testing.T) {
	// 构造真实 SQLite authkeys store + service
	authStore := newTestAuthKeysSQLiteStore(t) // 复用 authkeys 测试 helper;若不可跨包访问,在测试内构造
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, authSvc.Refresh(ctx))

	// 构造真实 tenants store + service(含两个租户)
	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-a", Subdomain: "a", Name: "A", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-b", Subdomain: "b", Name: "B", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))
	tenantSvc := tenants.NewService(tenantStore, time.Minute)

	masterKey := "master-secret"
	mw := AuthMiddlewareWithAuthenticator(masterKey, authSvc, nil)

	// 1. 平台管理员签发租户 A 的管理员 key
	adminKey, err := authSvc.Create(ctx, authkeys.CreateInput{
		Name: "A admin", TenantID: "tenant-a", IsTenantAdmin: true,
	})
	require.NoError(t, err)

	// 2. 租户管理员 key 在租户 A host 访问 /admin/*
	require.Equal(t, http.StatusOK, doAuthReq(t, mw, "a.smart-router.com", "/admin/auth-keys", adminKey.Value, tenantSvc))

	// 3. 签发租户 A 的 API key(通过 service 直接创建,模拟租户管理员操作)
	apiKey, err := authSvc.Create(ctx, authkeys.CreateInput{
		Name: "A api key", TenantID: "tenant-a",
	})
	require.NoError(t, err)

	// 4. API key 在租户 A host 访问 /v1/*
	require.Equal(t, http.StatusOK, doAuthReq(t, mw, "a.smart-router.com", "/v1/chat/completions", apiKey.Value, tenantSvc))

	// 5. API key 在租户 A host 访问 /admin/* → 403
	require.Equal(t, http.StatusForbidden, doAuthReq(t, mw, "a.smart-router.com", "/admin/auth-keys", apiKey.Value, tenantSvc))

	// 6. 租户管理员 key 在平台 host 访问 /admin/* → 401
	require.Equal(t, http.StatusUnauthorized, doAuthReq(t, mw, "app.smart-router.com", "/admin/auth-keys", adminKey.Value, tenantSvc))

	// 7. 租户 A 的 API key 在租户 B host 访问 /v1/* → 401
	require.Equal(t, http.StatusUnauthorized, doAuthReq(t, mw, "b.smart-router.com", "/v1/chat/completions", apiKey.Value, tenantSvc))
}

// doAuthReq 发起一个带 Bearer token 的请求,先经 TenantResolver(用真实 tenantSvc),
// 再经 AuthMiddleware,返回最终 HTTP 状态码。
func doAuthReq(t *testing.T, authMW echo.MiddlewareFunc, host, path, token string, tenantSvc *tenants.Service) int {
	t.Helper()
	tenantMW := TenantResolver(tenantSvc, "smart-router.com", "app")
	handler := tenantMW(authMW(func(c *echo.Context) error { return c.NoContent(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler(c)
	return rec.Code
}
```

**说明**:
- `newTestAuthKeysSQLiteStore` 与 `newMemoryDB`:执行前 `grep -rn "func newTestAuthKeysSQLiteStore\|func newMemoryDB\|func newTestSQLiteStore" internal/authkeys/*_test.go internal/server/*_test.go` 确认。若跨包不可访问,在测试文件内构造 SQLite authkeys store(参考 `internal/authkeys/store_sqlite_test.go` 的模式,用 `_ "modernc.org/sqlite"` + `sql.Open("sqlite", ":memory:")`)。
- `doAuthReq` 用 direct-chain-call 模式叠加 TenantResolver + AuthMiddleware,与 `tenant_resolver_test.go` 一致。
- 若 `writeGatewayError` 不直接写 `rec.Code`(而是返回 echo.HTTPError),`doAuthReq` 需调整:用 `e.ServeHTTP(rec, req)` 而非 direct-chain-call,让 Echo 的错误处理器写状态码。执行前确认。

- [ ] **Step 2: 运行测试**

Run: `go test ./internal/server/... -run TestTwoTierKeyModel_EndToEnd -v`
Expected: PASS。若有失败,根据失败用例调试(可能是 `writeGatewayError` 写状态码的方式,或 helper 跨包访问问题)。

- [ ] **Step 3: 跑全量测试 + 提交**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/server/auth_tenant_integration_test.go
git commit -m "test(server): add two-tier key model end-to-end integration test"
```

---

## Self-Review

**1. Spec coverage(对照设计文档 §4.1、§6.2、§6.3、§4.3):**
- §4.1 `auth_keys` 加 `tenant_id` + `is_tenant_admin` → Task 1 ✓
- §4.3 两级 Key 模型(管理员 key + API key)→ Task 1(CreateInput)+ Task 3(Service)+ Task 4(中间件)✓
- §6.2 master key 在两种 host 的行为 → Task 4 ✓
- §6.2 managed key tenant 匹配 → Task 4 ✓
- §6.2 `is_tenant_admin` 路径访问 → Task 4 ✓
- §6.3 master key 空时平台 host `/admin/*` → 503 → Task 4 ✓
- §6.3 租户 admin key 在平台 host → 401 → Task 4 ✓
- §6.3 租户 API key 访问 `/admin/*` → 403 → Task 4 ✓
- §4.4 迁移:`auth_keys` 现有行回填 `tenant_id='default'`、`is_tenant_admin=0` → Task 1 的 ALTER COLUMN DEFAULT 处理(`tenant_id TEXT` 默认 NULL,`is_tenant_admin INTEGER NOT NULL DEFAULT 0`)。**注意**:设计文档说回填 `tenant_id='default'`,但 ALTER ADD COLUMN 不能设非空默认值兼容所有后端。P2 接受现有行 `tenant_id` 为 NULL/空,中间件 tenant 匹配检查对空 `TenantID` 跳过(向后兼容)。P3 迁移脚本可补回填。已在 Global Constraints 声明。

**P2 明确排除(归后续阶段):** Store 接口加 tenantID 参数(P3)、admin handler 拆分(P4)、租户管理员创建 key 时自动 scope 到当前租户(P4)、租户创建 admin 端点(P4)、给其它业务表加 tenant_id(P3)。

**2. Placeholder scan:**
- 无 TBD/TODO。多个"执行前 grep 确认 helper"指令——这些是必要的探查(authkeys 测试 helper 名、`core.NewGatewayError` 签名、`writeGatewayError` 行为),实现者必须执行并在不一致时报告。

**3. Type consistency:**
- `AuthKey.TenantID`/`IsTenantAdmin` 在 Task 1 定义,Task 2/3 使用——一致。
- `AuthenticationResult.TenantID`/`IsTenantAdmin` 在 Task 3 定义,Task 4 中间件使用——一致。
- `CreateInput.TenantID`/`IsTenantAdmin` 在 Task 1 定义,Task 3(Service.Create)与 Task 5(admin handler)使用——一致。
- `enforceTenantAndRole(c, authResult)` 在 Task 4 定义并使用——签名一致。

**4. 已知风险与执行注意事项:**
- `core.GatewayError` 直接构造(`&core.GatewayError{Type, Message, StatusCode}.WithCode(code)`),`writeGatewayError(c, err)` 用 `HTTPStatusCode()` 写状态码——已确认 `internal/core/errors.go` 与 `internal/server/error_support.go`,Task 4 代码可直接使用。
- Task 6 的 `doAuthReq` 叠加两个中间件:TenantResolver 必须在 Auth 之前(它设置 ctx.tenantID)。direct-chain-call 顺序 `tenantMW(authMW(handler))` 正确(外层先执行)。`writeGatewayError` 直接写 `rec.Code`,断言有效。
- 删除 http.go 的 `/admin/*` skipPaths 可能影响依赖该行为的现有测试——Task 4 Step 6 指示排查。
- `authkeys.TokenPrefix = "sk_gom_"` 是既有常量(含 `gomodel` 命名),P2 不改名(避免破坏现有 key)。

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-02-saas-multi-tenant-p2.md`. Two execution options:

**1. Subagent-Driven (recommended)** — 每个 Task 派一个 fresh subagent 执行,Task 间做两阶段 review。

**2. Inline Execution** — 在当前会话用 executing-plans 批量执行,带 checkpoint 复核。

选哪种?
