package admin

import "github.com/labstack/echo/v5"

// RouteRegistrar is the subset of *echo.Group / *echo.Echo that RegisterRoutes
// uses. Decoupling from a concrete echo type keeps the admin package useful for
// callers that want to mount the API under a different path prefix or wrap the
// routes with extra middleware.
type RouteRegistrar interface {
	GET(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	POST(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	PUT(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	PATCH(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	DELETE(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
}

// RegisterRoutes mounts the admin REST API on the given route group.
// Callers typically pass an *echo.Group rooted at /admin.
//
// The route table is split into unexported sub-registration methods so the
// new PlatformAdminHandler/TenantAdminHandler can delegate the exact subset
// they own without re-registering duplicate method+path entries (the echo
// router used here rejects duplicates and Group.Add panics on the error).
func (h *Handler) RegisterRoutes(g RouteRegistrar) {
	h.registerPlatformInfra(g)
	h.registerUsageAuditBudgets(g)
	h.registerConfigResources(g)
	h.registerAuthKeys(g)
}

// registerPlatformInfra mounts endpoints that are global to the gateway
// process (not tenant-scoped): runtime/config, cache/overview, live/logs,
// providers/status, runtime/refresh, and the model inventory.
func (h *Handler) registerPlatformInfra(g RouteRegistrar) {
	g.GET("/runtime/config", h.DashboardConfig)
	g.GET("/cache/overview", h.CacheOverview)
	g.GET("/live/logs", h.LiveLogs)

	g.GET("/providers/status", h.ProviderStatus)
	g.POST("/runtime/refresh", h.RefreshRuntime)

	g.GET("/models", h.ListModels)
	g.GET("/models/categories", h.ListCategories)
}

// registerUsageAuditBudgets mounts the usage/*, audit/*, and budgets/*
// endpoints. These already derive the tenant from the request context
// (core.GetTenantID), so the tenant host delegates exactly this subset.
func (h *Handler) registerUsageAuditBudgets(g RouteRegistrar) {
	g.GET("/usage/summary", h.UsageSummary)
	g.GET("/usage/daily", h.DailyUsage)
	g.GET("/usage/models", h.UsageByModel)
	g.GET("/usage/user-paths", h.UsageByUserPath)
	g.GET("/usage/labels", h.UsageByLabel)
	g.GET("/usage/log", h.UsageLog)
	g.GET("/usage/throughput", h.TokenThroughput)
	g.POST("/usage/recalculate-pricing", h.RecalculateUsagePricing)

	g.GET("/audit/log", h.AuditLog)
	g.GET("/audit/detail", h.AuditLogDetail)
	g.GET("/audit/conversation", h.AuditConversation)

	g.GET("/budgets", h.ListBudgets)
	g.PUT("/budgets", h.UpsertBudget)
	g.DELETE("/budgets", h.DeleteBudget)
	g.GET("/budgets/settings", h.BudgetSettings)
	g.PUT("/budgets/settings", h.UpdateBudgetSettings)
	g.POST("/budgets/reset-one", h.ResetBudget)
	g.POST("/budgets/reset", h.ResetBudgets)
}

// registerConfigResources mounts the per-tenant config resources
// (tagging/settings, virtual-models, failover, model-pricing-overrides,
// guardrails, workflows). The platform handler serves these via the
// existing Handler methods (implicit "default" tenant); the tenant handler
// serves them via its ForTenant methods.
func (h *Handler) registerConfigResources(g RouteRegistrar) {
	g.GET("/tagging/settings", h.TaggingSettings)
	g.PUT("/tagging/settings", h.UpdateTaggingSettings)

	g.GET("/virtual-models", h.ListVirtualModels)
	g.PUT("/virtual-models", h.UpsertVirtualModel)
	g.DELETE("/virtual-models", h.DeleteVirtualModel)

	g.GET("/failover", h.ListFailoverRules)
	g.PUT("/failover", h.UpsertFailoverRule)
	g.DELETE("/failover", h.DeleteFailoverRule)
	g.POST("/failover/reset", h.ResetFailoverRules)
	g.POST("/failover/generate", h.GenerateFailoverRules)

	g.GET("/model-pricing-overrides", h.ListModelPricingOverrides)
	g.PUT("/model-pricing-overrides", h.UpsertModelPricingOverride)
	g.DELETE("/model-pricing-overrides", h.DeleteModelPricingOverride)

	g.GET("/guardrails/types", h.ListGuardrailTypes)
	g.GET("/guardrails", h.ListGuardrails)
	g.PUT("/guardrails", h.UpsertGuardrail)
	g.DELETE("/guardrails", h.DeleteGuardrail)

	g.GET("/workflows", h.ListWorkflows)
	g.GET("/workflows/guardrails", h.ListWorkflowGuardrails)
	g.GET("/workflows/:id", h.GetWorkflow)
	g.POST("/workflows", h.CreateWorkflow)
	g.POST("/workflows/:id/deactivate", h.DeactivateWorkflow)
}

// registerAuthKeys mounts the auth-keys block. GET /auth-keys honors an
// optional tenant_id query param (empty = cross-tenant, the platform view).
func (h *Handler) registerAuthKeys(g RouteRegistrar) {
	g.GET("/auth-keys", h.ListAuthKeys)
	g.POST("/auth-keys", h.CreateAuthKey)
	g.PUT("/auth-keys/:id/labels", h.UpdateAuthKeyLabels)
	g.POST("/auth-keys/:id/deactivate", h.DeactivateAuthKey)
}
