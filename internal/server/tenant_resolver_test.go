package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/core"
	"smartrouter/internal/tenants"
)

type stubStore struct {
	tenant tenants.Tenant
	getErr error
}

func (s *stubStore) Create(context.Context, tenants.Tenant) error { return nil }
func (s *stubStore) GetByID(context.Context, string) (tenants.Tenant, error) {
	return s.tenant, s.getErr
}
func (s *stubStore) GetBySubdomain(context.Context, string) (tenants.Tenant, error) {
	return s.tenant, s.getErr
}
func (s *stubStore) List(context.Context) ([]tenants.Tenant, error) { return nil, nil }
func (s *stubStore) UpdateStatus(context.Context, string, tenants.Status, time.Time) error {
	return nil
}
func (s *stubStore) Close() error { return nil }

// newEchoWithTenantResolver builds an Echo instance plus a handler chain that
// wraps a probe handler with the TenantResolver middleware. The chain is
// invoked directly (rather than via e.ServeHTTP) so the test's context is
// the one mutated by the middleware — needed for context assertions.
func newEchoWithTenantResolver(t *testing.T, svc *tenants.Service, baseDomain, platformHost string) (*echo.Echo, echo.HandlerFunc, *bool) {
	t.Helper()
	e := echo.New()
	called := false
	probe := func(c *echo.Context) error {
		called = true
		// 把 context 里的 tenant 信息回写到响应,便于断言
		return c.JSON(http.StatusOK, map[string]any{
			"tenant_id":     core.GetTenantID(c.Request().Context()),
			"platform_host": core.GetPlatformHost(c.Request().Context()),
		})
	}
	return e, TenantResolver(svc, baseDomain, platformHost)(probe), &called
}

func doReq(e *echo.Echo, handler echo.HandlerFunc, host, path string) (*httptest.ResponseRecorder, *echo.Context) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = handler(c)
	return rec, c
}

func TestTenantResolver_BaseDomainEmpty_NoOp(t *testing.T) {
	svc := tenants.NewService(nil, 0) // store nil,不应被调用
	e, handler, called := newEchoWithTenantResolver(t, svc, "", "app")

	rec, _ := doReq(e, handler, "localhost:8080", "/probe")
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, *called)
}

func TestTenantResolver_PlatformHost(t *testing.T) {
	svc := tenants.NewService(nil, 0)
	e, handler, _ := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	for _, host := range []string{"app.smart-router.com", "www.smart-router.com", "smart-router.com"} {
		rec, c := doReq(e, handler, host, "/probe")
		require.Equal(t, http.StatusOK, rec.Code, host)
		require.True(t, core.GetPlatformHost(c.Request().Context()), host)
		require.Empty(t, core.GetTenantID(c.Request().Context()), host)
	}
}

func TestTenantResolver_TenantSubdomain(t *testing.T) {
	store := &stubStore{tenant: tenants.Tenant{ID: "t-1", Subdomain: "xyz", Status: tenants.StatusActive}}
	svc := tenants.NewService(store, time.Minute)
	e, handler, _ := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	rec, c := doReq(e, handler, "xyz.smart-router.com", "/probe")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "t-1", core.GetTenantID(c.Request().Context()))
	require.False(t, core.GetPlatformHost(c.Request().Context()))
}

func TestTenantResolver_UnknownSubdomain(t *testing.T) {
	store := &stubStore{getErr: tenants.ErrNotFound}
	svc := tenants.NewService(store, time.Minute)
	e, handler, _ := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	rec, _ := doReq(e, handler, "missing.smart-router.com", "/probe")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "unknown_tenant")
}

func TestTenantResolver_DisabledTenant(t *testing.T) {
	store := &stubStore{tenant: tenants.Tenant{ID: "t-2", Subdomain: "off", Status: tenants.StatusDisabled}}
	svc := tenants.NewService(store, time.Minute)
	e, handler, _ := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	rec, _ := doReq(e, handler, "off.smart-router.com", "/probe")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "tenant_disabled")
}

func TestTenantResolver_ForeignHost_NoOp(t *testing.T) {
	svc := tenants.NewService(nil, 0)
	e, handler, called := newEchoWithTenantResolver(t, svc, "smart-router.com", "app")

	// Host 不以 .smart-router.com 结尾
	rec, c := doReq(e, handler, "localhost:8080", "/probe")
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, *called)
	require.Empty(t, core.GetTenantID(c.Request().Context()))
	require.False(t, core.GetPlatformHost(c.Request().Context()))
}
