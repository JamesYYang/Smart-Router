package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/core"
	"smartrouter/internal/tenants"
)

// TenantResolver resolves the request tenant from the Host header and
// injects it into the request context. When baseDomain is empty the
// middleware is a no-op (development/localhost mode).
func TenantResolver(svc *tenants.Service, baseDomain, platformHost string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if baseDomain == "" || svc == nil {
				return next(c)
			}
			req := c.Request()
			subdomain, isPlatform, matched := parseHost(req.Host, baseDomain, platformHost)
			if !matched {
				// Foreign host (e.g. localhost) — leave context unset.
				return next(c)
			}
			if isPlatform {
				ctx := core.WithPlatformHost(req.Context(), true)
				c.SetRequest(req.WithContext(ctx))
				return next(c)
			}
			tenant, err := svc.ResolveBySubdomain(req.Context(), subdomain)
			if err != nil {
				return tenantError(c, err)
			}
			ctx := core.WithTenantID(req.Context(), tenant.ID)
			c.SetRequest(req.WithContext(ctx))
			return next(c)
		}
	}
}

// parseHost splits the Host header into a subdomain and whether it is the
// platform host. matched is false when the host does not belong to baseDomain.
func parseHost(host, baseDomain, platformHost string) (subdomain string, isPlatform, matched bool) {
	// strip :port
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || baseDomain == "" {
		return "", false, false
	}
	suffix := "." + baseDomain
	if !strings.HasSuffix(host, suffix) && host != baseDomain {
		return "", false, false
	}
	if host == baseDomain {
		return "", true, true // apex
	}
	first := strings.TrimSuffix(host, suffix)
	if first == "www" || first == platformHost {
		return "", true, true
	}
	return first, false, true
}

func tenantError(c *echo.Context, err error) error {
	if tenants.IsNotFound(err) {
		return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "unknown_tenant"}})
	}
	var disabledErr *tenants.TenantDisabledError
	if errors.As(err, &disabledErr) {
		return c.JSON(http.StatusForbidden, map[string]any{"error": map[string]string{"type": "tenant_disabled"}})
	}
	return c.JSON(http.StatusInternalServerError, map[string]any{"error": map[string]string{"type": "tenant_resolution_failed"}})
}
