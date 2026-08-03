package admin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/tenants"

	_ "modernc.org/sqlite"
)

// newMemoryDB opens an in-memory SQLite database for admin handler tests.
func newMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTestTenantsService builds a tenants.Service backed by a real SQLite store.
func newTestTenantsService(t *testing.T) *tenants.Service {
	t.Helper()
	store, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return tenants.NewService(store, 0, "app")
}

// newTestAuthKeysService builds an authkeys.Service backed by a real SQLite store.
func newTestAuthKeysService(t *testing.T) *authkeys.Service {
	t.Helper()
	store, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	svc, err := authkeys.NewService(store)
	require.NoError(t, err)
	return svc
}

func TestPlatformAdminHandler_CreateTenant(t *testing.T) {
	h := &PlatformAdminHandler{Tenants: newTestTenantsService(t)}
	body := `{"subdomain":"xyz","name":"XYZ Inc","plan":"pro"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.CreateTenant(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)
}

func TestPlatformAdminHandler_CreateTenant_RejectsReservedSubdomain(t *testing.T) {
	h := &PlatformAdminHandler{Tenants: newTestTenantsService(t)}
	body := `{"subdomain":"default","name":"Nope"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.CreateTenant(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPlatformAdminHandler_DeleteTenant_SoftDisables(t *testing.T) {
	svc := newTestTenantsService(t)
	require.NoError(t, svc.Create(context.Background(), tenants.Tenant{ID: "t-1", Subdomain: "xyz", Name: "XYZ", Status: tenants.StatusActive}))
	h := &PlatformAdminHandler{Tenants: svc}

	e := echo.New()
	g := e.Group("/admin")
	g.DELETE("/tenants/:id", h.DeleteTenant)

	req := httptest.NewRequest(http.MethodDelete, "/admin/tenants/t-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	got, err := svc.GetByID(context.Background(), "t-1")
	require.NoError(t, err)
	require.True(t, got.IsDisabled())
}

func TestPlatformAdminHandler_IssueTenantAdminKey(t *testing.T) {
	tenantsSvc := newTestTenantsService(t)
	require.NoError(t, tenantsSvc.Create(context.Background(), tenants.Tenant{ID: "t-1", Subdomain: "xyz", Name: "XYZ", Status: tenants.StatusActive}))
	authSvc := newTestAuthKeysService(t)
	h := &PlatformAdminHandler{Tenants: tenantsSvc, AuthKeys: authSvc}

	e := echo.New()
	g := e.Group("/admin")
	g.POST("/tenants/:id/admin-keys", h.IssueTenantAdminKey)

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants/t-1/admin-keys", strings.NewReader(`{"name":"xyz admin"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)
}
