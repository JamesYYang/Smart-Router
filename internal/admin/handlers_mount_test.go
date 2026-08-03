package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
)

// TestPlatformAdminHandler_RegisterRoutes_MountsWithoutPanic verifies that
// PlatformAdminHandler.RegisterRoutes can be mounted on a real echo router
// without panicking. The platform host must serve tenants CRUD plus the FULL
// existing admin API (delegated via Default), with no duplicate method+path
// registration.
func TestPlatformAdminHandler_RegisterRoutes_MountsWithoutPanic(t *testing.T) {
	h := &PlatformAdminHandler{
		Tenants:  newTestTenantsService(t),
		AuthKeys: newTestAuthKeysService(t),
		Default:  &Handler{},
	}
	e := echo.New()
	h.RegisterRoutes(e.Group("/admin"))

	// tenants CRUD resolves (real service).
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// An existing admin endpoint still resolves through the Default delegation.
	// The zero-value Handler reports feature-unavailable (503) rather than 404,
	// proving the route is registered.
	req = httptest.NewRequest(http.MethodGet, "/admin/virtual-models", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusNotFound, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusNotFound, rec.Code)
}

// TestTenantAdminHandler_RegisterRoutes_MountsWithoutPanic verifies that
// TenantAdminHandler.RegisterRoutes can be mounted on a real echo router
// without panicking. The tenant host serves auto-scoped auth-keys + the 6
// per-tenant config resources + usage/audit/budgets (delegated via Config),
// with no duplicate method+path registration.
func TestTenantAdminHandler_RegisterRoutes_MountsWithoutPanic(t *testing.T) {
	h := &TenantAdminHandler{
		AuthKeys: newTestAuthKeysService(t),
		Config:   &Handler{},
	}
	e := echo.New()
	h.RegisterRoutes(e.Group("/admin"))

	// auth-keys list resolves, auto-scoped to the ctx tenant.
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// A per-tenant config resource resolves (nil service → 503, not a panic).
	req = httptest.NewRequest(http.MethodGet, "/admin/virtual-models", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// usage resolves through the Config delegation (zero-value → 503, not 404).
	req = httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusNotFound, rec.Code)

	// budgets resolves through the Config delegation.
	req = httptest.NewRequest(http.MethodGet, "/admin/budgets", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusNotFound, rec.Code)

	// runtime/config is NOT exposed on the tenant host (platform infra only).
	req = httptest.NewRequest(http.MethodGet, "/admin/runtime/config", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandler_ListAuthKeys_ScopesByTenantQueryParam verifies the platform
// host's cross-tenant/per-tenant auth-keys listing: the existing
// Handler.ListAuthKeys honors ?tenant_id= (empty = all tenants).
func TestHandler_ListAuthKeys_ScopesByTenantQueryParam(t *testing.T) {
	authSvc := newTestAuthKeysService(t)
	_, err := authSvc.Create(context.Background(), authkeys.CreateInput{Name: "a", TenantID: "tenant-a"})
	require.NoError(t, err)
	_, err = authSvc.Create(context.Background(), authkeys.CreateInput{Name: "b", TenantID: "tenant-b"})
	require.NoError(t, err)

	h := &Handler{authKeys: authSvc}

	// ?tenant_id=tenant-a → only tenant-a keys.
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys?tenant_id=tenant-a", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	require.NoError(t, h.ListAuthKeys(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"a"`)
	require.NotContains(t, rec.Body.String(), `"b"`)

	// No tenant_id → all tenants (current behavior).
	req = httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	rec = httptest.NewRecorder()
	c = echo.New().NewContext(req, rec)
	require.NoError(t, h.ListAuthKeys(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"a"`)
	require.Contains(t, rec.Body.String(), `"b"`)
}
