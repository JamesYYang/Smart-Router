package admin

func (h *PlatformAdminHandler) RegisterRoutes(g RouteRegistrar) {
	g.POST("/tenants", h.CreateTenant)
	g.GET("/tenants", h.ListTenants)
	g.GET("/tenants/:id", h.GetTenant)
	g.PATCH("/tenants/:id", h.UpdateTenant)
	g.DELETE("/tenants/:id", h.DeleteTenant)
	g.POST("/tenants/:id/admin-keys", h.IssueTenantAdminKey)
	if h.AuthKeys != nil {
		g.GET("/auth-keys", h.ListAuthKeysAcrossTenants)
	}
	if h.Default != nil {
		h.Default.RegisterRoutes(g) // 平台默认配置端点(virtual-models/failover/guardrails/workflows/pricing/tagging/providers)复用现有实现,隐式操作 "default" 租户
	}
}
