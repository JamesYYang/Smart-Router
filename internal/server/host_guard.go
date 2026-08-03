package server

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/core"
)

// hostGuard restricts a route group to requests matching the given Host
// kind ("platform" or "tenant"), returning 404 on mismatch so the other
// kind's routes never appear to exist on the wrong host.
func hostGuard(kind string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			isPlatform := core.GetPlatformHost(c.Request().Context())
			switch kind {
			case "platform":
				if !isPlatform {
					return c.NoContent(http.StatusNotFound)
				}
			case "tenant":
				if isPlatform {
					return c.NoContent(http.StatusNotFound)
				}
			}
			return next(c)
		}
	}
}
