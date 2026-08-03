package server

import (
	"github.com/labstack/echo/v5"

	"smartrouter/internal/core"
	"smartrouter/internal/tagging"
)

// TaggingCapture extracts request labels from the configured tagging headers
// and attaches them, together with the do-not-pass strip set, to the request
// context. It runs after RequestSnapshotCapture so audit logging still sees
// the original headers; stripping happens at the provider forwarding boundary.
func TaggingCapture(service *tagging.Service) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req := c.Request()
			ctx := req.Context()
			if service == nil || !service.HasRules(ctx) {
				return next(c)
			}
			changed := false
			if labels := service.ExtractLabels(ctx, req.Header); len(labels) > 0 {
				ctx = core.WithRequestLabels(ctx, labels)
				changed = true
			}
			if strip := service.StripHeaders(ctx); len(strip) > 0 {
				ctx = core.WithTaggingStripHeaders(ctx, strip)
				changed = true
			}
			if changed {
				c.SetRequest(req.WithContext(ctx))
			}
			return next(c)
		}
	}
}
