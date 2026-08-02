package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/core"
	"smartrouter/internal/tenants"

	_ "modernc.org/sqlite"
)

// newMemoryDB returns an in-memory SQLite database closed on test cleanup.
func newMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestServerEndToEnd_TenantResolution verifies the TenantResolver middleware
// resolves a tenant from the Host header and injects the tenant ID into the
// request context, visible to downstream handlers on a full Echo instance.
func TestServerEndToEnd_TenantResolution(t *testing.T) {
	store, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, tenants.Tenant{
		ID:        "t-xyz",
		Subdomain: "xyz",
		Name:      "XYZ",
		Status:    tenants.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}))

	svc := tenants.NewService(store, time.Minute)

	e := echo.New()
	e.Use(TenantResolver(svc, "smart-router.com", "app"))
	var gotTenantID string
	e.GET("/v1/chat/completions", func(c *echo.Context) error {
		gotTenantID = core.GetTenantID(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Host = "xyz.smart-router.com"
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "t-xyz", gotTenantID)
}

// TestServerEndToEnd_TenantResolution_PlatformHost verifies the platform host
// subdomain does NOT set a tenant ID (it sets the platform-host flag instead).
func TestServerEndToEnd_TenantResolution_PlatformHost(t *testing.T) {
	store, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	svc := tenants.NewService(store, time.Minute)

	e := echo.New()
	e.Use(TenantResolver(svc, "smart-router.com", "app"))
	var (
		gotTenantID     string
		gotPlatformHost bool
	)
	e.GET("/v1/chat/completions", func(c *echo.Context) error {
		gotTenantID = core.GetTenantID(c.Request().Context())
		gotPlatformHost = core.GetPlatformHost(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Host = "app.smart-router.com"
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "", gotTenantID)
	require.True(t, gotPlatformHost)
}
