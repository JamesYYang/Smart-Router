package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"smartrouter/internal/admin"
	"smartrouter/internal/core"
)

// The admin API is split by host kind: /admin/tenants (and the platform
// default config surface) is served only on the platform host, while the
// tenant-scoped auth-keys/config surfaces are served only on tenant hosts.
// These tests exercise the host-aware mounting in http.go end to end through
// the real echo router.

func TestAdminRouting_PlatformHost_TenantRoutesNotFound(t *testing.T) {
	cfg := &Config{
		AdminEndpointsEnabled: true,
		PlatformAdminHandler:  &admin.PlatformAdminHandler{},
		TenantAdminHandler:    &admin.TenantAdminHandler{},
	}
	e := New(&mockProvider{}, cfg)

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
	cfg := &Config{
		AdminEndpointsEnabled: true,
		PlatformAdminHandler:  &admin.PlatformAdminHandler{Default: shared},
		TenantAdminHandler:    &admin.TenantAdminHandler{Config: shared},
	}
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
	cfg := &Config{
		AdminEndpointsEnabled: true,
		PlatformAdminHandler:  &admin.PlatformAdminHandler{Default: shared},
		TenantAdminHandler:    &admin.TenantAdminHandler{Config: shared},
	}
	e := New(&mockProvider{}, cfg)

	for _, path := range []string{"/admin/runtime/config", "/admin/models", "/admin/providers/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNotFound, rec.Code, "platform-infra path %s must 404 on tenant host", path)
	}
}
