package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/core"
)

func TestHostGuard_Platform_Allows(t *testing.T) {
	e := echo.New()
	e.Use(hostGuard("platform"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(core.WithPlatformHost(req.Context(), true))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestHostGuard_Platform_RejectsTenantHost(t *testing.T) {
	e := echo.New()
	e.Use(hostGuard("platform"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHostGuard_Tenant_Allows(t *testing.T) {
	e := echo.New()
	e.Use(hostGuard("tenant"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestHostGuard_Tenant_RejectsPlatformHost(t *testing.T) {
	e := echo.New()
	e.Use(hostGuard("tenant"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(core.WithPlatformHost(req.Context(), true))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHostGuard_Tenant_AllowsDevMode(t *testing.T) {
	// base_domain 未配置:isPlatformHost 恒为 false,tenant 分组放行
	e := echo.New()
	e.Use(hostGuard("tenant"))
	e.GET("/probe", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
