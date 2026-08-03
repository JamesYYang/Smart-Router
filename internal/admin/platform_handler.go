package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/tenants"
)

// PlatformAdminHandler serves /admin/* on the platform host (app.<base_domain>).
// It manages tenants and platform-default configuration. Every dependency
// is optional and nil-checked, matching the existing admin.Handler pattern.
type PlatformAdminHandler struct {
	Tenants  *tenants.Service
	AuthKeys *authkeys.Service
	Default  *Handler // delegates platform-default config/provider endpoints unchanged
}

type createTenantRequest struct {
	Subdomain string `json:"subdomain"`
	Name      string `json:"name"`
	Plan      string `json:"plan,omitempty"`
}

type tenantResponse struct {
	ID        string    `json:"id"`
	Subdomain string    `json:"subdomain"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Plan      string    `json:"plan"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func tenantToResponse(t tenants.Tenant) tenantResponse {
	return tenantResponse{ID: t.ID, Subdomain: t.Subdomain, Name: t.Name, Status: string(t.Status), Plan: t.Plan, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func newTenantID() string { return uuid.NewString() }

func (h *PlatformAdminHandler) CreateTenant(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	var req createTenantRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	req.Subdomain = strings.ToLower(strings.TrimSpace(req.Subdomain))
	if req.Subdomain == "" || req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": "subdomain and name are required"}})
	}
	now := time.Now().UTC()
	t := tenants.Tenant{ID: newTenantID(), Subdomain: req.Subdomain, Name: req.Name, Plan: req.Plan, Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}
	if err := h.Tenants.Create(c.Request().Context(), t); err != nil {
		if tenants.IsReservedSubdomain(err) || tenants.IsSubdomainTaken(err) {
			return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_subdomain", "message": err.Error()}})
		}
		return handleError(c, err)
	}
	return c.JSON(http.StatusCreated, tenantToResponse(t))
}

func (h *PlatformAdminHandler) ListTenants(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	list, err := h.Tenants.List(c.Request().Context())
	if err != nil {
		return handleError(c, err)
	}
	out := make([]tenantResponse, 0, len(list))
	for _, t := range list {
		out = append(out, tenantToResponse(t))
	}
	return c.JSON(http.StatusOK, map[string]any{"tenants": out})
}

func (h *PlatformAdminHandler) GetTenant(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	t, err := h.Tenants.GetByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		if tenants.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "tenant_not_found"}})
		}
		return handleError(c, err)
	}
	return c.JSON(http.StatusOK, tenantToResponse(t))
}

type updateTenantRequest struct {
	Name string `json:"name"`
	Plan string `json:"plan"`
}

func (h *PlatformAdminHandler) UpdateTenant(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	var req updateTenantRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	if err := h.Tenants.Update(c.Request().Context(), c.Param("id"), req.Name, req.Plan); err != nil {
		if tenants.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "tenant_not_found"}})
		}
		return handleError(c, err)
	}
	got, err := h.Tenants.GetByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return handleError(c, err)
	}
	return c.JSON(http.StatusOK, tenantToResponse(got))
}

// DeleteTenant soft-deletes a tenant by disabling it (no cascading data
// deletion — see Global Constraints).
func (h *PlatformAdminHandler) DeleteTenant(c *echo.Context) error {
	if h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tenants_unavailable"}})
	}
	if err := h.Tenants.UpdateStatus(c.Request().Context(), c.Param("id"), tenants.StatusDisabled, time.Now().UTC()); err != nil {
		if tenants.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "tenant_not_found"}})
		}
		return handleError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

type issueTenantAdminKeyRequest struct {
	Name string `json:"name"`
}

// IssueTenantAdminKey creates a tenant-admin auth key for the tenant
// identified by the :id path param. Only the platform admin (master key on
// the platform host) can call this — it is the only way a tenant admin
// credential is minted.
func (h *PlatformAdminHandler) IssueTenantAdminKey(c *echo.Context) error {
	if h.AuthKeys == nil || h.Tenants == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	tenantID := c.Param("id")
	if _, err := h.Tenants.GetByID(c.Request().Context(), tenantID); err != nil {
		if tenants.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": map[string]string{"type": "tenant_not_found"}})
		}
		return handleError(c, err)
	}
	var req issueTenantAdminKeyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	issued, err := h.AuthKeys.Create(c.Request().Context(), authkeys.CreateInput{
		Name:          req.Name,
		TenantID:      tenantID,
		IsTenantAdmin: true,
	})
	if err != nil {
		return handleError(c, err)
	}
	return c.JSON(http.StatusCreated, issued)
}
