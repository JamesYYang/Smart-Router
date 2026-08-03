package server

// End-to-end integration test for the platform/tenant admin split (P4).
//
// The echo chain is assembled the same way internal/server/http.go does:
// TenantResolver → AuthMiddlewareWithAuthenticator → mountAdminRoutesByHost
// (which attaches the per-route hostGuard middleware). Requests are driven
// with real Host headers through the real TenantResolver, so core.GetPlatformHost
// / core.GetTenantID are populated exactly as in production. Real SQLite stores
// and real services are used throughout; no production code is touched.
//
// The brief's six assertions map to TestAdminSplit. TestAdminSplit_SharedPathsDispatch
// additionally exercises the shared-path arm of mountAdminRoutesByHost
// (hostAwareAdminHandler) with the platform handler's Default and the tenant
// handler's Config wired the way internal/app does.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/admin"
	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
	"smartrouter/internal/providers"
	"smartrouter/internal/tenants"
	"smartrouter/internal/virtualmodels"

	_ "modernc.org/sqlite"
)

const (
	adminSplitBaseDomain   = "smart-router.com"
	adminSplitPlatformHost = "app"
	adminSplitMasterKey    = "master-secret"
)

// adminSplitEcho builds the real chain:
//   - TenantResolver (only active when baseDomain is configured — this is why
//     a.smart-router.com / app.smart-router.com resolve here, exactly as in
//     production with a configured base_domain)
//   - AuthMiddlewareWithAuthenticator (master key + managed auth keys)
//   - mountAdminRoutesByHost (real mount; hostGuard attached per route)
//   - a stub /v1/chat/completions inference route so tenant-key host-mismatch
//     (assertion 4) is exercised end to end against a real inference path.
func adminSplitEcho(t *testing.T, authSvc *authkeys.Service, tenantSvc *tenants.Service, platform *admin.PlatformAdminHandler, tenant *admin.TenantAdminHandler) *echo.Echo {
	t.Helper()
	e := echo.New()
	e.Use(TenantResolver(tenantSvc, adminSplitBaseDomain, adminSplitPlatformHost))
	e.Use(AuthMiddlewareWithAuthenticator(adminSplitMasterKey, authSvc, nil))
	// baseDomainConfigured=true: host-aware mounting arm runs.
	mountAdminRoutesByHost(e.Group("/admin"), platform, tenant, true)
	e.POST("/v1/chat/completions", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	return e
}

// adminSplitReq sends one request with a real Host header (through the real
// TenantResolver), an optional Bearer token, and an optional JSON body.
func adminSplitReq(t *testing.T, e *echo.Echo, method, host, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, r)
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// adminSplitTenant is the subset of the POST /admin/tenants response we need.
type adminSplitTenant struct {
	ID        string `json:"id"`
	Subdomain string `json:"subdomain"`
	Name      string `json:"name"`
}

// adminSplitIssuedKey is the subset of an auth-key create response we need
// (created via POST /admin/tenants/:id/admin-keys or POST /admin/auth-keys).
type adminSplitIssuedKey struct {
	ID            string `json:"id"`
	Value         string `json:"value"`
	TenantID      string `json:"tenant_id"`
	IsTenantAdmin bool   `json:"is_tenant_admin"`
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// TestAdminSplit covers the brief's six assertions end to end:
//
//  1. Platform admin (master key, platform host) creates tenants A and B and
//     issues each a tenant-admin key.
//  2. A virtual-model override created by tenant A on a.smart-router.com is
//     invisible to tenant B on b.smart-router.com (and vice versa).
//  3. A tenant admin's API key is auto-scoped to its tenant even when the body
//     attempts to smuggle tenant_id=someone-else / is_tenant_admin=true.
//  4. Tenant A's API key on b.smart-router.com /v1/* → 401 (P2 behavior,
//     re-verified end to end).
//  5. Platform admin key (master key) on a tenant host hitting /admin/tenants
//     → 404 (hostGuard("platform") — route-level isolation, not 401/403).
//  6. /admin/auth-keys is a tenant-only endpoint (hostGuard("tenant")). A
//     tenant-admin key on the platform host is rejected by the auth layer
//     first (401 key_not_allowed_on_platform_host, the P2 behavior), so the
//     route-level 404 is observed with a credential that passes auth on the
//     platform host (the master key).
func TestAdminSplit(t *testing.T) {
	// Real SQLite stores + services (mirrors auth_tenant_integration_test.go).
	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	// platformHost-aware service: "app" can never be claimed as a subdomain.
	tenantSvc := tenants.NewService(tenantStore, time.Minute, adminSplitPlatformHost)

	vmStore, err := virtualmodels.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = vmStore.Close() })
	vmSvc, err := virtualmodels.NewService(vmStore, providers.NewModelRegistry(), true)
	require.NoError(t, err)

	// Platform handler owns tenant CRUD + admin-key issuance only (no Default
	// admin surface), so /admin/auth-keys and /admin/virtual-models are
	// tenant-only endpoints — the surface the brief's assertions target.
	platformHandler := &admin.PlatformAdminHandler{Tenants: tenantSvc, AuthKeys: authSvc}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc, VirtualModels: vmSvc}

	e := adminSplitEcho(t, authSvc, tenantSvc, platformHandler, tenantHandler)

	platformHost := adminSplitPlatformHost + "." + adminSplitBaseDomain

	// ---- Assertion 1: platform admin creates tenants A/B and issues each a
	// tenant-admin key (both via the real admin HTTP API on the platform host).
	rec := adminSplitReq(t, e, http.MethodPost, platformHost, "/admin/tenants", adminSplitMasterKey,
		map[string]any{"subdomain": "a", "name": "Tenant A"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	tenantA := decodeJSON[adminSplitTenant](t, rec)

	rec = adminSplitReq(t, e, http.MethodPost, platformHost, "/admin/tenants", adminSplitMasterKey,
		map[string]any{"subdomain": "b", "name": "Tenant B"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	tenantB := decodeJSON[adminSplitTenant](t, rec)

	rec = adminSplitReq(t, e, http.MethodPost, platformHost, "/admin/tenants/"+tenantA.ID+"/admin-keys", adminSplitMasterKey,
		map[string]any{"name": "A admin"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	adminKeyA := decodeJSON[adminSplitIssuedKey](t, rec)
	require.True(t, adminKeyA.IsTenantAdmin)
	require.Equal(t, tenantA.ID, adminKeyA.TenantID)

	rec = adminSplitReq(t, e, http.MethodPost, platformHost, "/admin/tenants/"+tenantB.ID+"/admin-keys", adminSplitMasterKey,
		map[string]any{"name": "B admin"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	adminKeyB := decodeJSON[adminSplitIssuedKey](t, rec)
	require.True(t, adminKeyB.IsTenantAdmin)
	require.Equal(t, tenantB.ID, adminKeyB.TenantID)

	hostA := tenantA.Subdomain + "." + adminSplitBaseDomain
	hostB := tenantB.Subdomain + "." + adminSplitBaseDomain

	// ---- Assertion 2: virtual-model overrides are tenant-scoped.
	const vmSource = "openai/gpt-4o-mini"
	rec = adminSplitReq(t, e, http.MethodPut, hostA, "/admin/virtual-models", adminKeyA.Value,
		map[string]any{"source": vmSource, "targets": []map[string]any{{"model": "openai/gpt-4o"}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = adminSplitReq(t, e, http.MethodGet, hostB, "/admin/virtual-models", adminKeyB.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), vmSource, "tenant B must not see tenant A's virtual-model override")

	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/virtual-models", adminKeyA.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), vmSource, "tenant A must see its own virtual-model override")

	// ---- Assertion 3: tenant admin's API key is auto-scoped to its tenant;
	// tenant_id / is_tenant_admin in the body are ignored.
	rec = adminSplitReq(t, e, http.MethodPost, hostA, "/admin/auth-keys", adminKeyA.Value,
		map[string]any{"name": "A API key", "tenant_id": "tenant-b", "is_tenant_admin": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	apiKeyA := decodeJSON[adminSplitIssuedKey](t, rec)
	require.Equal(t, tenantA.ID, apiKeyA.TenantID, "tenant_id in the body must be ignored")
	require.False(t, apiKeyA.IsTenantAdmin, "is_tenant_admin=true in the body must be ignored")

	// ---- Assertion 4: tenant A's API key on tenant B's host /v1/* → 401.
	rec = adminSplitReq(t, e, http.MethodPost, hostB, "/v1/chat/completions", apiKeyA.Value, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "key_tenant_mismatch")

	// ---- Assertion 5: platform admin key (master key) on a tenant host
	// hitting /admin/tenants → 404 (hostGuard("platform"), route-level — not
	// 401/403).
	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/tenants", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "platform-only route must 404 on a tenant host")

	// ---- Assertion 6: /admin/auth-keys is a tenant-only endpoint
	// (hostGuard("tenant")). A tenant-admin key on the platform host is
	// rejected by the auth layer first (401 key_not_allowed_on_platform_host);
	// the route-level 404 shows for a credential that passes auth on the
	// platform host (the master key).
	rec = adminSplitReq(t, e, http.MethodGet, platformHost, "/admin/auth-keys", adminKeyA.Value, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "key_not_allowed_on_platform_host")

	rec = adminSplitReq(t, e, http.MethodGet, platformHost, "/admin/auth-keys", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "tenant-only route must 404 on the platform host (hostGuard(tenant))")
}

// TestAdminSplit_SharedPathsDispatch exercises the shared-path arm of
// mountAdminRoutesByHost. With the platform handler's Default and the tenant
// handler's Config wired (as internal/app does), /admin/auth-keys is registered
// once and dispatched per host kind (hostAwareAdminHandler): platform host →
// platform handler, tenant host → tenant handler. Single-owner paths still
// carry hostGuard and 404 on the wrong host.
func TestAdminSplit_SharedPathsDispatch(t *testing.T) {
	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	tenantSvc := tenants.NewService(tenantStore, time.Minute, adminSplitPlatformHost)
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-a", Subdomain: "a", Name: "A", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))

	vmStore, err := virtualmodels.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = vmStore.Close() })
	vmSvc, err := virtualmodels.NewService(vmStore, providers.NewModelRegistry(), true)
	require.NoError(t, err)

	// Full production-style wiring: the platform handler's Default and the
	// tenant handler's Config share one *admin.Handler instance, exactly like
	// internal/app's initAdmin + split.
	adminDefault := admin.NewHandler(nil, providers.NewModelRegistry(),
		admin.WithAuthKeys(authSvc),
		admin.WithVirtualModels(vmSvc),
	)
	platformHandler := &admin.PlatformAdminHandler{Tenants: tenantSvc, AuthKeys: authSvc, Default: adminDefault}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc, VirtualModels: vmSvc, Config: adminDefault}
	e := adminSplitEcho(t, authSvc, tenantSvc, platformHandler, tenantHandler)

	adminKeyA, err := authSvc.Create(ctx, authkeys.CreateInput{Name: "A admin", TenantID: "tenant-a", IsTenantAdmin: true})
	require.NoError(t, err)

	platformHost := adminSplitPlatformHost + "." + adminSplitBaseDomain
	hostA := "a." + adminSplitBaseDomain

	// Shared GET /admin/auth-keys dispatches to the platform handler on the
	// platform host (cross-tenant view) and to the tenant handler on a tenant
	// host (tenant-scoped view).
	rec := adminSplitReq(t, e, http.MethodGet, platformHost, "/admin/auth-keys", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/auth-keys", adminKeyA.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A tenant-admin key is still rejected at the auth layer on the platform
	// host before the shared dispatcher can run.
	rec = adminSplitReq(t, e, http.MethodGet, platformHost, "/admin/auth-keys", adminKeyA.Value, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "key_not_allowed_on_platform_host")

	// Single-owner routes keep hostGuard: the tenant-only DELETE
	// /admin/auth-keys/:id 404s on the platform host even for the master key…
	rec = adminSplitReq(t, e, http.MethodDelete, platformHost, "/admin/auth-keys/any-id", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// …and the platform-only GET /admin/tenants 404s on a tenant host.
	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/tenants", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestAdminSplit_LegacyMode_NoBaseDomain covers the B2 regression: when NO
// base_domain is configured (and no tenant service), TenantResolver is a no-op
// — every request has GetPlatformHost=false and GetTenantID="". The legacy
// single-tenant surface must be mounted instead of the host-aware split:
//   - shared paths (e.g. /admin/virtual-models) write to the platform-default
//     tenant (tenant_id="") and are visible through the platform handler —
//     pre-P4 the single Handler served everything on "default" with live cache
//     refresh;
//   - platform-only infra (/admin/runtime/config) returns 200, not 404;
//   - no tenant routes are mounted, so tenant-host behavior does not apply.
func TestAdminSplit_LegacyMode_NoBaseDomain(t *testing.T) {
	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, authSvc.Refresh(ctx))

	vmStore, err := virtualmodels.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = vmStore.Close() })
	// Register the target model so the platform handler's redirect validation
	// (which checks the catalog) accepts the upsert — in production the
	// catalog is warm from provider discovery.
	vmRegistry := providers.NewModelRegistry()
	vmRegistry.RegisterProviderWithType(&mockProvider{
		modelsResponse: &core.ModelsResponse{
			Object: "list",
			Data:   []core.Model{{ID: "gpt-4o", Object: "model", OwnedBy: "openai"}},
		},
	}, "openai")
	require.NoError(t, vmRegistry.Initialize(ctx))
	vmSvc, err := virtualmodels.NewService(vmStore, vmRegistry, true)
	require.NoError(t, err)

	// Full production-style wiring: platform Default and tenant Config share
	// one *admin.Handler, but mountAdminRoutesByHost runs in legacy mode so
	// only the platform surface is registered (no hostGuard).
	adminDefault := admin.NewHandler(nil, providers.NewModelRegistry(),
		admin.WithAuthKeys(authSvc),
		admin.WithVirtualModels(vmSvc),
	)
	platformHandler := &admin.PlatformAdminHandler{Tenants: nil, AuthKeys: authSvc, Default: adminDefault}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc, VirtualModels: vmSvc, Config: adminDefault}

	e := echo.New()
	// NO TenantResolver middleware — exactly the no-base_domain production shape.
	e.Use(AuthMiddlewareWithAuthenticator(adminSplitMasterKey, authSvc, nil))
	mountAdminRoutesByHost(e.Group("/admin"), platformHandler, tenantHandler, false)

	// Shared path: PUT /admin/virtual-models with the master key writes to the
	// default tenant and is visible through the platform handler (live cache).
	const vmSource = "openai/gpt-4o-mini"
	rec := adminSplitReq(t, e, http.MethodPut, "127.0.0.1:8080", "/admin/virtual-models", adminSplitMasterKey,
		map[string]any{"source": vmSource, "targets": []map[string]any{{"model": "openai/gpt-4o"}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = adminSplitReq(t, e, http.MethodGet, "127.0.0.1:8080", "/admin/virtual-models", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), vmSource, "platform handler must see virtual model written to the default tenant")

	// Platform-only infra returns 200 in legacy mode (was 404 for everyone
	// with the host-aware split and no resolution).
	rec = adminSplitReq(t, e, http.MethodGet, "127.0.0.1:8080", "/admin/runtime/config", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Tenant routes are NOT mounted in legacy mode: the tenant-only DELETE
	// /admin/auth-keys/:id 404s even though no host resolution happens.
	rec = adminSplitReq(t, e, http.MethodDelete, "127.0.0.1:8080", "/admin/auth-keys/any-id", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// A Host header that would be a tenant subdomain in multi-tenant mode does
	// nothing here — no resolution, no tenant routes.
	rec = adminSplitReq(t, e, http.MethodGet, "a.smart-router.com", "/admin/runtime/config", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
