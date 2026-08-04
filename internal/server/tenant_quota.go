package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/budget"
	"smartrouter/internal/core"
)

// TenantQuotaMiddleware enforces tenant-aggregate budgets on the routes it is
// mounted on (the /v1 inference group). A tenant-level budget is a budget whose
// user path is the wildcard "*" ("/*" once normalized), which the budget stores
// interpret as "sum usage across all user paths".
//
// Requests without a resolved tenant (platform host) are not checked, and a
// transient check failure is logged but never breaks inference.
func TenantQuotaMiddleware(checker BudgetChecker) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if checker == nil {
			return next
		}
		return func(c *echo.Context) error {
			tenantID := core.GetTenantID(c.Request().Context())
			if tenantID == "" {
				return next(c) // platform host — no tenant budget
			}
			if err := checker.CheckTenant(c.Request().Context(), time.Now().UTC()); err != nil {
				var exceeded *budget.ExceededError
				if errors.As(err, &exceeded) {
					return writeGatewayError(c, tenantQuotaExceededError(exceeded))
				}
				// Transient budget-check failure: log but allow the request
				// through so an unavailable budget store never breaks inference.
				slog.Warn("tenant budget check failed",
					"tenant_id", tenantID,
					"error", err,
				)
			}
			return next(c)
		}
	}
}

// tenantQuotaExceededError renders an exhausted tenant budget as a 402 gateway
// error with a dedicated type, rendered in the request's wire dialect by
// writeGatewayError like the rest of the server's error responses.
func tenantQuotaExceededError(exceeded *budget.ExceededError) *core.GatewayError {
	message := exceeded.Error()
	if message == "" {
		message = "tenant budget exceeded"
	}
	return &core.GatewayError{
		Type:       core.ErrorType("tenant_budget_exceeded"),
		Message:    message,
		StatusCode: http.StatusPaymentRequired,
		Provider:   "budget",
	}
}
