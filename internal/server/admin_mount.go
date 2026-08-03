package server

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/admin"
	"smartrouter/internal/core"
)

// The echo router in this fork rejects duplicate method+path registrations
// (Echo.Add panics; see the router's allowOverwritingRoute guard), so the
// platform and tenant admin handlers — which legitimately share paths such as
// auth-keys, the config resources, and usage/audit/budgets — cannot each mount
// the same route on one echo instance. mountAdminRoutesByHost records both
// handlers' route tables and replays them merged onto a single group.

// capturedRoute is one method+path registration captured from a handler's
// RegisterRoutes call.
type capturedRoute struct {
	method  string
	path    string
	handler echo.HandlerFunc
}

// routeCapture implements admin.RouteRegistrar, recording registrations
// instead of mounting them on a real router.
type routeCapture struct {
	routes []capturedRoute
}

func (c *routeCapture) add(method, path string, h echo.HandlerFunc, _ ...echo.MiddlewareFunc) echo.RouteInfo {
	c.routes = append(c.routes, capturedRoute{method: method, path: path, handler: h})
	return echo.RouteInfo{}
}

func (c *routeCapture) GET(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return c.add(http.MethodGet, path, h, m...)
}
func (c *routeCapture) POST(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return c.add(http.MethodPost, path, h, m...)
}
func (c *routeCapture) PUT(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return c.add(http.MethodPut, path, h, m...)
}
func (c *routeCapture) PATCH(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return c.add(http.MethodPatch, path, h, m...)
}
func (c *routeCapture) DELETE(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo {
	return c.add(http.MethodDelete, path, h, m...)
}

// mountAdminRoutesByHost registers the combined platform + tenant admin API on
// g (an echo group rooted at /admin). Each method+path is registered exactly
// once:
//   - a path owned by only one host kind carries a hostGuard for that kind, so
//     the other host sees a 404 and the route never appears to exist there;
//   - a path served by both kinds is registered once with a host-aware
//     dispatcher that delegates to the platform or tenant handler based on
//     core.GetPlatformHost(ctx).
//
// When baseDomainConfigured is false (no base_domain configured and/or no
// tenant service wired, so TenantResolver never runs), the whole host split is
// moot: every request sees GetPlatformHost=false and GetTenantID="". Mounting
// the tenant surface there would scope writes to tenant_id='' (invisible to
// the live "default" cache) and the platform-only infra routes would 404 for
// everyone. In that legacy single-tenant mode we therefore mount the FULL
// platform surface with NO hostGuard and skip tenant routes entirely — exactly
// the pre-P4 behavior where the single Handler served everything on "default".
func mountAdminRoutesByHost(g *echo.Group, platform *admin.PlatformAdminHandler, tenant *admin.TenantAdminHandler, baseDomainConfigured bool) {
	if !baseDomainConfigured {
		if platform != nil {
			platform.RegisterRoutes(g)
		}
		return
	}
	var platformRoutes, tenantRoutes []capturedRoute
	if platform != nil {
		cap := &routeCapture{}
		platform.RegisterRoutes(cap)
		platformRoutes = cap.routes
	}
	if tenant != nil {
		cap := &routeCapture{}
		tenant.RegisterRoutes(cap)
		tenantRoutes = cap.routes
	}

	tenantByKey := make(map[string]echo.HandlerFunc, len(tenantRoutes))
	for _, r := range tenantRoutes {
		tenantByKey[r.method+" "+r.path] = r.handler
	}

	registered := make(map[string]bool, len(platformRoutes))
	for _, r := range platformRoutes {
		key := r.method + " " + r.path
		registered[key] = true
		if tenantFn, ok := tenantByKey[key]; ok {
			g.Add(r.method, r.path, hostAwareAdminHandler(r.handler, tenantFn))
		} else {
			g.Add(r.method, r.path, r.handler, hostGuard("platform"))
		}
	}
	for _, r := range tenantRoutes {
		key := r.method + " " + r.path
		if registered[key] {
			continue
		}
		g.Add(r.method, r.path, r.handler, hostGuard("tenant"))
	}
}

// hostAwareAdminHandler dispatches a shared admin endpoint to the platform
// handler on the platform host and to the tenant handler otherwise.
func hostAwareAdminHandler(platform, tenant echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if core.GetPlatformHost(c.Request().Context()) {
			return platform(c)
		}
		return tenant(c)
	}
}
