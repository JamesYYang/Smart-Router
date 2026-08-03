package admin

func (h *PlatformAdminHandler) RegisterRoutes(g RouteRegistrar) {
	g.POST("/tenants", h.CreateTenant)
	g.GET("/tenants", h.ListTenants)
	g.GET("/tenants/:id", h.GetTenant)
	g.PATCH("/tenants/:id", h.UpdateTenant)
	g.DELETE("/tenants/:id", h.DeleteTenant)
	g.POST("/tenants/:id/admin-keys", h.IssueTenantAdminKey)
	if h.Default != nil {
		// 平台默认配置 + 全量现有 admin 端点(runtime/usage/audit/budgets/
		// providers/models/auth-keys/guardrails/workflows/...)复用现有实现,
		// 隐式操作 "default" 租户。GET /auth-keys 通过 ?tenant_id= 支持跨租户/指定租户。
		h.Default.RegisterRoutes(g)
	}
}
