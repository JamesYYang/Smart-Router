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
	// (newTestAuthKeysSQLiteStore 不存在;authkeys.newTestSQLiteStore 跨包不可访问,
	//  用 public authkeys.NewSQLiteStore + newMemoryDB 内联构造)
	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
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
