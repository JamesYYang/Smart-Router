package server

// P7: cross-tenant end-to-end integration. Assembles the real chain
// (TenantResolver → AuthMiddlewareWithAuthenticator → mountAdminRoutesByHost
// → /v1 group with hostGuard("tenant")) with real SQLite stores + services,
// mirroring internal/http.go's production wiring, and drives requests with
// real Host headers. Extends the P4/P5 integration tests with the remaining
// P7 matrix: auth-key tenant isolation, disabled-tenant 403, and the
// platform/tenant hostGuard 404s on both admin and /v1 planes.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/admin"
	"smartrouter/internal/authkeys"
	"smartrouter/internal/providers"
	"smartrouter/internal/tenants"

	_ "modernc.org/sqlite"
)

// p7Echo builds the full production-shaped chain: TenantResolver → auth →
// mountAdminRoutesByHost → /v1 group carrying hostGuard("tenant") with a stub
// inference handler. This mirrors http.go (unlike adminSplitEcho, whose /v1
// stub is NOT host-guarded). baseDomainConfigured=true always.
func p7Echo(t *testing.T, authSvc *authkeys.Service, tenantSvc *tenants.Service, platform *admin.PlatformAdminHandler, tenant *admin.TenantAdminHandler) *echo.Echo {
	t.Helper()
	e := echo.New()
	e.Use(TenantResolver(tenantSvc, adminSplitBaseDomain, adminSplitPlatformHost))
	e.Use(AuthMiddlewareWithAuthenticator(adminSplitMasterKey, authSvc, nil))
	mountAdminRoutesByHost(e.Group("/admin"), platform, tenant, true)
	v1 := e.Group("/v1", hostGuard("tenant"))
	v1.POST("/chat/completions", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	return e
}

// TestP7CrossTenantAuthKeyIsolation verifies two tenants' admin surfaces do not
// leak auth keys into each other, and that hostGuard keeps platform-only admin
// routes off tenant hosts and tenant-only inference routes off the platform host.
func TestP7CrossTenantAuthKeyIsolation(t *testing.T) {
	ctx := context.Background()

	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	tenantSvc := tenants.NewService(tenantStore, time.Minute, adminSplitPlatformHost)
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-a", Subdomain: "a", Name: "Tenant A", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-b", Subdomain: "b", Name: "Tenant B", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))

	// Full production-style wiring (mirrors app.go initAdmin + split).
	adminDefault := admin.NewHandler(nil, providers.NewModelRegistry(),
		admin.WithAuthKeys(authSvc),
	)
	platformHandler := &admin.PlatformAdminHandler{Tenants: tenantSvc, AuthKeys: authSvc, Default: adminDefault}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc, Config: adminDefault}
	e := p7Echo(t, authSvc, tenantSvc, platformHandler, tenantHandler)

	// Issue a tenant-admin key for A and B via the platform admin API.
	platformHost := adminSplitPlatformHost + "." + adminSplitBaseDomain
	hostA := "a." + adminSplitBaseDomain
	hostB := "b." + adminSplitBaseDomain

	rec := adminSplitReq(t, e, http.MethodPost, platformHost, "/admin/tenants/tenant-a/admin-keys", adminSplitMasterKey,
		map[string]any{"name": "A admin"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	adminKeyA := decodeJSON[adminSplitIssuedKey](t, rec)

	rec = adminSplitReq(t, e, http.MethodPost, platformHost, "/admin/tenants/tenant-b/admin-keys", adminSplitMasterKey,
		map[string]any{"name": "B admin"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	adminKeyB := decodeJSON[adminSplitIssuedKey](t, rec)

	// Tenant A creates a regular API key; tenant B must not see it.
	rec = adminSplitReq(t, e, http.MethodPost, hostA, "/admin/auth-keys", adminKeyA.Value,
		map[string]any{"name": "A secret key"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = adminSplitReq(t, e, http.MethodGet, hostB, "/admin/auth-keys", adminKeyB.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "A secret key", "tenant B must not see tenant A's auth key")
	require.Contains(t, rec.Body.String(), "B admin", "tenant B must see its own admin key")

	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/auth-keys", adminKeyA.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "A secret key", "tenant A must see its own auth key")

	// Platform-only route 404s on a tenant host even for the master key.
	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/tenants", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "platform-only /admin/tenants must 404 on a tenant host")

	// Tenant-only /admin/auth-keys DELETE 404s on the platform host.
	rec = adminSplitReq(t, e, http.MethodDelete, platformHost, "/admin/auth-keys/any-id", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "tenant-only route must 404 on the platform host")

	// Tenant A's tenant-admin key is rejected on the platform host.
	rec = adminSplitReq(t, e, http.MethodGet, platformHost, "/admin/auth-keys", adminKeyA.Value, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "tenant admin key on platform host must 401")
	require.Contains(t, rec.Body.String(), "key_not_allowed_on_platform_host")
}

// TestP7DisabledTenantBlocked verifies a disabled tenant's host returns 403
// from the TenantResolver before any auth happens, on both admin and /v1 paths.
func TestP7DisabledTenantBlocked(t *testing.T) {
	ctx := context.Background()

	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	tenantSvc := tenants.NewService(tenantStore, time.Minute, adminSplitPlatformHost)
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-disabled", Subdomain: "dead", Name: "Dead Tenant", Status: tenants.StatusDisabled, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-active", Subdomain: "live", Name: "Live Tenant", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))

	platformHandler := &admin.PlatformAdminHandler{Tenants: tenantSvc, AuthKeys: authSvc}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc}
	e := p7Echo(t, authSvc, tenantSvc, platformHandler, tenantHandler)

	// Disabled tenant: /v1 inference is 403 (tenant_disabled) even with the master key.
	rec := adminSplitReq(t, e, http.MethodPost, "dead."+adminSplitBaseDomain, "/v1/chat/completions", adminSplitMasterKey, map[string]any{"model": "gpt-4o"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "tenant_disabled")

	// Disabled tenant: admin path also 403 from the resolver.
	rec = adminSplitReq(t, e, http.MethodGet, "dead."+adminSplitBaseDomain, "/admin/auth-keys", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "tenant_disabled")

	// Active tenant admin path works.
	rec = adminSplitReq(t, e, http.MethodGet, "live."+adminSplitBaseDomain, "/admin/auth-keys", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestP7HostGuardSeparation verifies the route-level host guard keeps the
// inference /v1/* surface off the platform host and the admin surface split
// correct on both hosts.
func TestP7HostGuardSeparation(t *testing.T) {
	ctx := context.Background()

	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	tenantSvc := tenants.NewService(tenantStore, time.Minute, adminSplitPlatformHost)
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-a", Subdomain: "a", Name: "Tenant A", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))
	keyA, err := authSvc.Create(ctx, authkeys.CreateInput{Name: "A key", TenantID: "tenant-a"})
	require.NoError(t, err)

	platformHandler := &admin.PlatformAdminHandler{Tenants: tenantSvc, AuthKeys: authSvc}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc}
	e := p7Echo(t, authSvc, tenantSvc, platformHandler, tenantHandler)

	platformHost := adminSplitPlatformHost + "." + adminSplitBaseDomain
	hostA := "a." + adminSplitBaseDomain

	// /v1 inference is tenant-only: platform host 404s (empty body from hostGuard).
	rec := adminSplitReq(t, e, http.MethodPost, platformHost, "/v1/chat/completions", adminSplitMasterKey, map[string]any{"model": "gpt-4o"})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Empty(t, rec.Body.String(), "hostGuard 404 must be empty body")

	// Tenant host /v1 works with the tenant's API key.
	rec = adminSplitReq(t, e, http.MethodPost, hostA, "/v1/chat/completions", keyA.Value, map[string]any{"model": "gpt-4o"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// /admin/tenants (platform-only) 404s on the tenant host.
	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/tenants", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
