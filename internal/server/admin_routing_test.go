package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"smartrouter/internal/admin"
	"smartrouter/internal/core"
	"smartrouter/internal/tenants"
)

// The admin API is split by host kind: /admin/tenants (and the platform
// default config surface) is served only on the platform host, while the
// tenant-scoped auth-keys/config surfaces are served only on tenant hosts.
// These tests exercise the host-aware mounting in http.go end to end through
// the real echo router.
//
// Host-aware mounting only activates when a base_domain is configured AND a
// tenant service is wired (the same condition that activates the
// TenantResolver middleware). Without those, mountAdminRoutesByHost falls back
// to the legacy single-tenant surface (full platform routes, no hostGuard).
// The host-aware tests below therefore set both, and keep requests on a
// foreign Host (the httptest default "example.com") so the TenantResolver
// middleware leaves the manually-injected context alone.

// hostAwareAdminConfig returns a server.Config with the multi-tenant admin
// split active (base_domain + tenant service), so the host-aware mounting arm
// runs. Requests must use a Host that does not match baseDomain so the
// TenantResolver middleware no-ops and the test's manually-set context sticks.
func hostAwareAdminConfig(t *testing.T, platform *admin.PlatformAdminHandler, tenant *admin.TenantAdminHandler) *Config {
	t.Helper()
	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	return &Config{
		AdminEndpointsEnabled: true,
		BaseDomain:            "smart-router.com",
		Tenants:               tenants.NewService(tenantStore, time.Minute, ""),
		PlatformAdminHandler:  platform,
		TenantAdminHandler:    tenant,
	}
}

func TestAdminRouting_PlatformHost_TenantRoutesNotFound(t *testing.T) {
	cfg := hostAwareAdminConfig(t,
		&admin.PlatformAdminHandler{},
		&admin.TenantAdminHandler{},
	)
	e := New(&mockProvider{}, cfg)

	req := httptest.NewRequest(http.MethodPost, "/admin/auth-keys", nil)
	req = req.WithContext(core.WithPlatformHost(req.Context(), true))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "tenant-only route (auth-keys create) must 404 on platform host")
}

func TestAdminRouting_TenantHost_PlatformRoutesNotFound(t *testing.T) {
	cfg := hostAwareAdminConfig(t,
		&admin.PlatformAdminHandler{},
		&admin.TenantAdminHandler{},
	)
	e := New(&mockProvider{}, cfg)

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "platform-only route (tenant create) must 404 on tenant host")
}

// The config resources, auth-keys, and usage/audit/budgets paths are served by
// BOTH handlers. They must be registered exactly once (the echo router rejects
// duplicate method+path) and dispatched to the platform or tenant handler
// based on the request host — so the same path resolves on both hosts.
func TestAdminRouting_SharedPathsServeBothHosts(t *testing.T) {
	shared := admin.NewHandler(nil, nil)
	cfg := hostAwareAdminConfig(t,
		&admin.PlatformAdminHandler{Default: shared},
		&admin.TenantAdminHandler{Config: shared},
	)
	e := New(&mockProvider{}, cfg)

	for _, path := range []string{"/admin/auth-keys", "/admin/workflows/guardrails", "/admin/usage/summary"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(core.WithPlatformHost(req.Context(), true))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.NotEqual(t, http.StatusNotFound, rec.Code, "shared path %s must resolve on platform host", path)
		require.NotEqual(t, http.StatusMethodNotAllowed, rec.Code, "shared path %s must resolve on platform host", path)

		req = httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.NotEqual(t, http.StatusNotFound, rec.Code, "shared path %s must resolve on tenant host", path)
		require.NotEqual(t, http.StatusMethodNotAllowed, rec.Code, "shared path %s must resolve on tenant host", path)
	}
}

// Platform infra (runtime/config, models, providers/status) must NOT resolve
// on a tenant host — it is registered under the platform group only.
func TestAdminRouting_PlatformInfraNotFoundOnTenantHost(t *testing.T) {
	shared := admin.NewHandler(nil, nil)
	cfg := hostAwareAdminConfig(t,
		&admin.PlatformAdminHandler{Default: shared},
		&admin.TenantAdminHandler{Config: shared},
	)
	e := New(&mockProvider{}, cfg)

	for _, path := range []string{"/admin/runtime/config", "/admin/models", "/admin/providers/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNotFound, rec.Code, "platform-infra path %s must 404 on tenant host", path)
	}
}

// TestAdminRouting_LegacyMode_NoBaseDomain covers B2: with NO base_domain
// configured (and no tenant service), host resolution never runs, so the
// host-aware mounting arm is skipped entirely. The full platform surface is
// mounted WITHOUT hostGuard and tenant routes are NOT mounted — pre-P4
// single-tenant behavior. Consequently:
//   - platform-infra routes like /admin/runtime/config resolve (not 404);
//   - a request carrying a fake tenant context still reaches the platform
//     surface (no hostGuard to reject it) and shared paths dispatch to the
//     platform handler, not the tenant handler.
func TestAdminRouting_LegacyMode_NoBaseDomain(t *testing.T) {
	shared := admin.NewHandler(nil, nil)
	cfg := &Config{
		AdminEndpointsEnabled: true,
		PlatformAdminHandler:  &admin.PlatformAdminHandler{Default: shared},
		TenantAdminHandler:    &admin.TenantAdminHandler{Config: shared},
	}
	e := New(&mockProvider{}, cfg)

	// Platform-only infra resolves on any host in legacy mode (no hostGuard).
	for _, path := range []string{"/admin/runtime/config", "/admin/models", "/admin/providers/status"} {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.NotEqual(t, http.StatusNotFound, rec.Code, "platform-infra path %s must resolve in legacy mode", path)
		require.NotEqual(t, http.StatusMethodNotAllowed, rec.Code, "platform-infra path %s must resolve in legacy mode", path)
	}

	// Platform-only infra also resolves with a fake tenant context injected —
	// there is no hostGuard to reject it in legacy mode.
	req := httptest.NewRequest(http.MethodGet, "/admin/runtime/config", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusNotFound, rec.Code, "platform-infra path must resolve in legacy mode even with a tenant ctx")

	// Legacy alias under /admin/api/v1/* resolves in legacy mode (hostGuard is
	// not attached when base_domain is unset).
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/v1/runtime/config", nil))
	require.NotEqual(t, http.StatusNotFound, rec.Code, "legacy alias must resolve in legacy mode")
	require.Equal(t, "true", rec.Header().Get("Deprecation"))
}
