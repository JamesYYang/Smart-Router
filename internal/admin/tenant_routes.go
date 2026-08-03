package admin

func (h *TenantAdminHandler) RegisterRoutes(g RouteRegistrar) {
	g.POST("/auth-keys", h.CreateAuthKey)
	g.GET("/auth-keys", h.ListAuthKeys)
	g.PUT("/auth-keys/:id/labels", h.UpdateAuthKeyLabels)
	g.DELETE("/auth-keys/:id", h.DeactivateAuthKey)

	g.GET("/virtual-models", h.ListVirtualModels)
	g.PUT("/virtual-models", h.UpsertVirtualModel)
	g.DELETE("/virtual-models", h.DeleteVirtualModel)

	g.GET("/failover", h.ListFailoverRules)
	g.PUT("/failover", h.UpsertFailoverRule)
	g.DELETE("/failover", h.DeleteFailoverRule)

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

	g.GET("/tagging/settings", h.TaggingSettings)
	g.PUT("/tagging/settings", h.UpdateTaggingSettings)

	if h.Config != nil {
		h.Config.registerUsageAuditBudgets(g) // usage/audit/budgets 端点复用现有实现(已按 ctx tenantID 读取)
	}
}
