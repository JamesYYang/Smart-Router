package admin

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
	"smartrouter/internal/failover"
	"smartrouter/internal/guardrails"
	"smartrouter/internal/pricingoverrides"
	"smartrouter/internal/providers"
	"smartrouter/internal/tagging"
	"smartrouter/internal/virtualmodels"
	"smartrouter/internal/workflows"
)

// TenantAdminHandler serves /admin/* on a tenant's own host
// (<subdomain>.<base_domain>). Every method auto-scopes to
// core.GetTenantID(c.Request().Context()) — it never accepts a tenant_id
// from the caller for its own tenant's resources.
type TenantAdminHandler struct {
	AuthKeys         *authkeys.Service
	VirtualModels    *virtualmodels.Service
	FailoverRules    *failover.Service
	Guardrails       *guardrails.Service
	Workflows        *workflows.Service
	PricingOverrides *pricingoverrides.Service
	Tagging          *tagging.Service
	Registry         *providers.ModelRegistry
	Config           *Handler // delegates usage/audit/budgets endpoints unchanged (already ctx-scoped)

	mutationMu sync.Mutex
}

type createTenantAuthKeyRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	UserPath    string   `json:"user_path,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// CreateAuthKey issues a regular API key scoped to the current tenant.
// tenant_id and is_tenant_admin from the request body (if any) are
// ignored — a tenant admin can never mint another admin key or escalate.
func (h *TenantAdminHandler) CreateAuthKey(c *echo.Context) error {
	if h.AuthKeys == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	var req createTenantAuthKeyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	issued, err := h.AuthKeys.Create(c.Request().Context(), authkeys.CreateInput{
		Name:        req.Name,
		Description: req.Description,
		UserPath:    req.UserPath,
		Labels:      req.Labels,
		TenantID:    tenantID,
		// IsTenantAdmin intentionally omitted (defaults to false) — only
		// PlatformAdminHandler.IssueTenantAdminKey can set it.
	})
	if err != nil {
		return handleError(c, err)
	}
	return c.JSON(http.StatusCreated, issued)
}

func (h *TenantAdminHandler) ListAuthKeys(c *echo.Context) error {
	if h.AuthKeys == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	views := h.AuthKeys.ListViews(tenantID)
	if views == nil {
		views = []authkeys.View{}
	}
	return c.JSON(http.StatusOK, views)
}

// updateAuthKeyLabelsRequest is declared in handler_authkeys.go and reused here.

func (h *TenantAdminHandler) UpdateAuthKeyLabels(c *echo.Context) error {
	if h.AuthKeys == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	var req updateAuthKeyLabelsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"type": "invalid_request", "message": err.Error()}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	updated, err := h.AuthKeys.UpdateLabels(c.Request().Context(), tenantID, c.Param("id"), req.Labels)
	if err != nil {
		if errors.Is(err, authkeys.ErrNotFound) {
			return handleError(c, core.NewNotFoundError("auth key not found: "+c.Param("id")))
		}
		return handleError(c, err)
	}
	return c.JSON(http.StatusOK, updated)
}

func (h *TenantAdminHandler) DeactivateAuthKey(c *echo.Context) error {
	if h.AuthKeys == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "auth_keys_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	if err := h.AuthKeys.Deactivate(c.Request().Context(), tenantID, c.Param("id")); err != nil {
		if errors.Is(err, authkeys.ErrNotFound) {
			return handleError(c, core.NewNotFoundError("auth key not found: "+c.Param("id")))
		}
		return handleError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// --- Virtual models (tenant overrides) ---

func (h *TenantAdminHandler) ListVirtualModels(c *echo.Context) error {
	if h.VirtualModels == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "virtual_models_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	rows, err := h.VirtualModels.ListEffectiveForTenant(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, err)
	}
	if rows == nil {
		rows = []virtualmodels.VirtualModel{}
	}
	return c.JSON(http.StatusOK, rows)
}

func (h *TenantAdminHandler) UpsertVirtualModel(c *echo.Context) error {
	if h.VirtualModels == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "virtual_models_unavailable"}})
	}
	var req upsertVirtualModelRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		return handleError(c, core.NewInvalidRequestError("source is required", nil))
	}
	vm, err := h.buildTenantVirtualModelUpsert(source, req)
	if err != nil {
		return handleError(c, err)
	}
	tenantID := core.GetTenantID(c.Request().Context())
	if err := h.VirtualModels.UpsertForTenant(c.Request().Context(), tenantID, vm); err != nil {
		return handleError(c, virtualModelWriteError(err))
	}
	saved, err := h.VirtualModels.GetForTenant(c.Request().Context(), tenantID, source)
	if err != nil {
		return handleError(c, err)
	}
	if saved == nil {
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, saved)
}

func (h *TenantAdminHandler) DeleteVirtualModel(c *echo.Context) error {
	if h.VirtualModels == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "virtual_models_unavailable"}})
	}
	var req deleteVirtualModelRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		return handleError(c, core.NewInvalidRequestError("source is required", nil))
	}
	tenantID := core.GetTenantID(c.Request().Context())
	if err := h.VirtualModels.DeleteForTenant(c.Request().Context(), tenantID, source); err != nil {
		if errors.Is(err, virtualmodels.ErrNotFound) {
			return handleError(c, core.NewNotFoundError("virtual model not found: "+source))
		}
		return handleError(c, virtualModelWriteError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

// buildTenantVirtualModelUpsert mirrors Handler.buildVirtualModelUpsert but
// resolves the enabled default from the tenant-scoped store.
func (h *TenantAdminHandler) buildTenantVirtualModelUpsert(source string, req upsertVirtualModelRequest) (virtualmodels.VirtualModel, error) {
	vm := virtualmodels.VirtualModel{
		Source:      source,
		Strategy:    strings.TrimSpace(req.Strategy),
		UserPaths:   req.UserPaths,
		Description: strings.TrimSpace(req.Description),
		Enabled:     h.VirtualModels.ResolveUpsertEnabled(source, req.OldSource, req.Enabled),
	}
	targets, err := buildVirtualModelTargets(req)
	if err != nil {
		return virtualmodels.VirtualModel{}, err
	}
	vm.Targets = targets
	return vm, nil
}

// --- Failover (tenant overrides) ---

func (h *TenantAdminHandler) ListFailoverRules(c *echo.Context) error {
	if h.FailoverRules == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "failover_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	rows, err := h.FailoverRules.ListEffectiveForTenant(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, err)
	}
	if rows == nil {
		rows = []failover.Rule{}
	}
	return c.JSON(http.StatusOK, rows)
}

func (h *TenantAdminHandler) UpsertFailoverRule(c *echo.Context) error {
	if h.FailoverRules == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "failover_unavailable"}})
	}
	var req upsertFailoverRuleRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	source := strings.TrimSpace(req.PrimaryModel)
	if source == "" {
		return handleError(c, core.NewInvalidRequestError("primary_model is required", nil))
	}
	tenantID := core.GetTenantID(c.Request().Context())
	enabled := true
	if existing, err := h.FailoverRules.GetForTenant(c.Request().Context(), tenantID, source); err == nil && existing != nil {
		enabled = existing.Enabled
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rule := failover.Rule{
		Source:  source,
		Targets: req.FallbackModels,
		Enabled: enabled,
	}
	if err := h.FailoverRules.UpsertForTenant(c.Request().Context(), tenantID, rule); err != nil {
		return handleError(c, failoverWriteError(err))
	}
	saved, err := h.FailoverRules.GetForTenant(c.Request().Context(), tenantID, source)
	if err != nil {
		return handleError(c, err)
	}
	if saved == nil {
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, saved)
}

func (h *TenantAdminHandler) DeleteFailoverRule(c *echo.Context) error {
	if h.FailoverRules == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "failover_unavailable"}})
	}
	source, err := failoverDeleteSource(c)
	if err != nil {
		return handleError(c, err)
	}
	tenantID := core.GetTenantID(c.Request().Context())
	err = h.FailoverRules.DeleteForTenant(c.Request().Context(), tenantID, source)
	switch {
	case err == nil:
		return c.NoContent(http.StatusNoContent)
	case errors.Is(err, failover.ErrNotFound):
		return handleError(c, core.NewNotFoundError("failover mapping not found: "+source))
	default:
		return handleError(c, failoverWriteError(err))
	}
}

// --- Guardrails (tenant overrides) ---

func (h *TenantAdminHandler) ListGuardrailTypes(c *echo.Context) error {
	if h.Guardrails == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "guardrails_unavailable"}})
	}
	return c.JSON(http.StatusOK, h.Guardrails.TypeDefinitions())
}

func (h *TenantAdminHandler) ListGuardrails(c *echo.Context) error {
	if h.Guardrails == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "guardrails_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	rows, err := h.Guardrails.ListEffectiveForTenant(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, err)
	}
	if rows == nil {
		rows = []guardrails.Definition{}
	}
	return c.JSON(http.StatusOK, rows)
}

func (h *TenantAdminHandler) UpsertGuardrail(c *echo.Context) error {
	if h.Guardrails == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "guardrails_unavailable"}})
	}
	var req upsertGuardrailRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return handleError(c, core.NewInvalidRequestError("guardrail name is required", nil))
	}
	userPath, err := normalizeUserPathQueryParam("user_path", req.UserPath)
	if err != nil {
		return handleError(c, err)
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	tenantID := core.GetTenantID(c.Request().Context())
	if err := h.Guardrails.UpsertForTenant(c.Request().Context(), tenantID, guardrails.Definition{
		Name:        name,
		Type:        req.Type,
		Description: req.Description,
		UserPath:    userPath,
		Config:      req.Config,
	}); err != nil {
		return handleError(c, guardrailWriteError(err))
	}
	definition, err := h.Guardrails.GetForTenant(c.Request().Context(), tenantID, name)
	if err != nil {
		return handleError(c, err)
	}
	if definition == nil {
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, guardrails.ViewFromDefinition(*definition))
}

func (h *TenantAdminHandler) DeleteGuardrail(c *echo.Context) error {
	if h.Guardrails == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "guardrails_unavailable"}})
	}
	var req deleteGuardrailRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return handleError(c, core.NewInvalidRequestError("guardrail name is required", nil))
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	tenantID := core.GetTenantID(c.Request().Context())
	referencingWorkflows, err := h.activeTenantWorkflowGuardrailReferences(c.Request().Context(), tenantID, name)
	if err != nil {
		return handleError(c, err)
	}
	if len(referencingWorkflows) > 0 {
		return handleError(c, core.NewInvalidRequestError("guardrail is used by active workflows: "+strings.Join(referencingWorkflows, ", "), nil))
	}
	if err := h.Guardrails.DeleteForTenant(c.Request().Context(), tenantID, name); err != nil {
		if errors.Is(err, guardrails.ErrNotFound) {
			return handleError(c, core.NewNotFoundError("guardrail not found: "+name))
		}
		return handleError(c, guardrailWriteError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

// activeTenantWorkflowGuardrailReferences mirrors Handler.activeWorkflowGuardrailReferences
// but scopes the search to the tenant's own active workflow versions.
func (h *TenantAdminHandler) activeTenantWorkflowGuardrailReferences(ctx context.Context, tenantID, name string) ([]string, error) {
	if h.Workflows == nil {
		return nil, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	versions, err := h.Workflows.ListActiveForTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	references := make([]string, 0)
	for _, version := range versions {
		if !version.Payload.Features.Guardrails {
			continue
		}
		for _, step := range version.Payload.Guardrails {
			if strings.TrimSpace(step.Ref) != name {
				continue
			}
			references = append(references, tenantWorkflowScopeDisplay(version.Scope))
			break
		}
	}
	sort.Strings(references)
	return references, nil
}

func tenantWorkflowScopeDisplay(scope workflows.Scope) string {
	parts := make([]string, 0, 3)
	if scope.Provider != "" {
		parts = append(parts, scope.Provider)
	}
	if scope.Model != "" {
		parts = append(parts, scope.Model)
	}
	if scope.UserPath != "" {
		parts = append(parts, scope.UserPath)
	}
	if len(parts) == 0 {
		return "global"
	}
	return strings.Join(parts, ":")
}

// --- Workflows (tenant overrides) ---

func (h *TenantAdminHandler) ListWorkflows(c *echo.Context) error {
	if h.Workflows == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "workflows_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	versions, err := h.Workflows.ListEffectiveForTenant(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, err)
	}
	if versions == nil {
		versions = []workflows.Version{}
	}
	return c.JSON(http.StatusOK, versions)
}

func (h *TenantAdminHandler) GetWorkflow(c *echo.Context) error {
	if h.Workflows == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "workflows_unavailable"}})
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return handleError(c, core.NewInvalidRequestError("workflow id is required", nil))
	}
	tenantID := core.GetTenantID(c.Request().Context())
	version, err := h.Workflows.GetForTenant(c.Request().Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, workflows.ErrNotFound) {
			return handleError(c, core.NewNotFoundError("workflow not found: "+id))
		}
		return handleError(c, err)
	}
	if version == nil {
		return handleError(c, core.NewNotFoundError("workflow not found: "+id))
	}
	return c.JSON(http.StatusOK, version)
}

func (h *TenantAdminHandler) ListWorkflowGuardrails(c *echo.Context) error {
	if h.Guardrails == nil {
		return c.JSON(http.StatusOK, []string{})
	}
	// M1: a tenant's own guardrails (persisted via UpsertForTenant, which
	// bypasses the shared in-memory snapshot) must be visible here. Collect
	// names from the tenant's effective guardrails instead of the shared
	// default-tenant Names().
	tenantID := core.GetTenantID(c.Request().Context())
	definitions, err := h.Guardrails.ListEffectiveForTenant(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, err)
	}
	names := make([]string, 0, len(definitions))
	for _, def := range definitions {
		names = append(names, def.Name)
	}
	sort.Strings(names)
	return c.JSON(http.StatusOK, names)
}

func (h *TenantAdminHandler) CreateWorkflow(c *echo.Context) error {
	if h.Workflows == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "workflows_unavailable"}})
	}
	var req createWorkflowRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	scopeProviderName := strings.TrimSpace(req.ScopeProviderName)
	if scopeProviderName == "" {
		scopeProviderName = strings.TrimSpace(req.LegacyScopeProvider)
	}
	scopeModel := strings.TrimSpace(req.ScopeModel)
	scopeUserPath, err := normalizeUserPathQueryParam("scope_user_path", req.ScopeUserPath)
	if err != nil {
		return handleError(c, err)
	}
	scopeProviderName, err = h.validateTenantWorkflowScope(scopeProviderName, scopeModel)
	if err != nil {
		return handleError(c, err)
	}
	if err := h.validateTenantWorkflowGuardrails(c.Request().Context(), core.GetTenantID(c.Request().Context()), req.Payload); err != nil {
		return handleError(c, err)
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	tenantID := core.GetTenantID(c.Request().Context())
	version, err := h.Workflows.CreateForTenant(c.Request().Context(), tenantID, workflows.CreateInput{
		Scope: workflows.Scope{
			Provider: scopeProviderName,
			Model:    scopeModel,
			UserPath: scopeUserPath,
		},
		Activate:    true,
		Name:        req.Name,
		Description: req.Description,
		Payload:     req.Payload,
	})
	if err != nil {
		return handleError(c, workflowWriteError(err))
	}
	if version == nil {
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusCreated, version)
}

func (h *TenantAdminHandler) DeactivateWorkflow(c *echo.Context) error {
	if h.Workflows == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "workflows_unavailable"}})
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return handleError(c, core.NewInvalidRequestError("workflow id is required", nil))
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	tenantID := core.GetTenantID(c.Request().Context())
	if err := h.Workflows.DeactivateForTenant(c.Request().Context(), tenantID, id); err != nil {
		if errors.Is(err, workflows.ErrNotFound) {
			return handleError(c, core.NewNotFoundError("workflow not found: "+id))
		}
		return handleError(c, workflowWriteError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *TenantAdminHandler) validateTenantWorkflowScope(scopeProviderName, scopeModel string) (string, error) {
	scopeProviderName = strings.TrimSpace(scopeProviderName)
	scopeModel = strings.TrimSpace(scopeModel)

	if scopeProviderName == "" {
		if scopeModel != "" {
			return "", core.NewInvalidRequestError("scope_model requires scope_provider_name", nil)
		}
		return "", nil
	}
	if h.Registry == nil {
		return "", core.NewInvalidRequestError("provider registry is unavailable for workflow provider-name validation", nil)
	}
	if !slices.Contains(h.Registry.ProviderNames(), scopeProviderName) {
		if resolvedProviderName := strings.TrimSpace(h.Registry.GetProviderNameForType(scopeProviderName)); resolvedProviderName != "" {
			scopeProviderName = resolvedProviderName
		}
	}
	if !slices.Contains(h.Registry.ProviderNames(), scopeProviderName) {
		return "", core.NewInvalidRequestError("unknown provider name: "+scopeProviderName, nil)
	}
	if scopeModel == "" {
		return scopeProviderName, nil
	}
	for _, model := range h.Registry.ListModelsWithProvider() {
		if model.ProviderName == scopeProviderName && model.Model.ID == scopeModel {
			return scopeProviderName, nil
		}
	}
	return "", core.NewInvalidRequestError("unknown model for provider name "+scopeProviderName+": "+scopeModel, nil)
}

func (h *TenantAdminHandler) validateTenantWorkflowGuardrails(ctx context.Context, tenantID string, payload workflows.Payload) error {
	if !payload.Features.Guardrails || len(payload.Guardrails) == 0 {
		return nil
	}
	if h.Guardrails == nil {
		return featureUnavailableError("guardrail registry is unavailable for workflow authoring")
	}
	// M1: validate against the tenant's effective guardrails (tenant overrides
	// merged over the platform-default tenant), not the shared default-tenant
	// snapshot — a tenant's own guardrails persisted via UpsertForTenant
	// bypass the shared cache and were invisible to Names().
	definitions, err := h.Guardrails.ListEffectiveForTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(definitions))
	for _, def := range definitions {
		known[def.Name] = struct{}{}
	}
	for _, step := range payload.Guardrails {
		ref := strings.TrimSpace(step.Ref)
		if ref == "" {
			continue
		}
		if _, ok := known[ref]; !ok {
			return core.NewInvalidRequestError("unknown guardrail ref: "+ref, nil)
		}
	}
	return nil
}

// --- Model pricing overrides (tenant overrides) ---

func (h *TenantAdminHandler) ListModelPricingOverrides(c *echo.Context) error {
	if h.PricingOverrides == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "model_pricing_overrides_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	rows, err := h.PricingOverrides.ListEffectiveForTenant(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, err)
	}
	if rows == nil {
		rows = []pricingoverrides.Override{}
	}
	return c.JSON(http.StatusOK, rows)
}

func (h *TenantAdminHandler) UpsertModelPricingOverride(c *echo.Context) error {
	if h.PricingOverrides == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "model_pricing_overrides_unavailable"}})
	}
	var req upsertModelPricingOverrideRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	selector, err := normalizeModelPricingOverrideSelector(req.Selector)
	if err != nil {
		return handleError(c, err)
	}
	tenantID := core.GetTenantID(c.Request().Context())
	if err := h.PricingOverrides.UpsertForTenant(c.Request().Context(), tenantID, pricingoverrides.Override{
		Selector: selector,
		Pricing:  req.Pricing,
	}); err != nil {
		return handleError(c, pricingOverrideWriteError(err))
	}
	saved, err := h.PricingOverrides.GetForTenant(c.Request().Context(), tenantID, selector)
	if err != nil {
		return handleError(c, err)
	}
	if saved == nil {
		return handleError(c, core.NewProviderError("model_pricing_overrides", http.StatusInternalServerError, "model pricing override update failed unexpectedly", nil))
	}
	return c.JSON(http.StatusOK, saved)
}

func (h *TenantAdminHandler) DeleteModelPricingOverride(c *echo.Context) error {
	if h.PricingOverrides == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "model_pricing_overrides_unavailable"}})
	}
	var req deleteModelPricingOverrideRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	selector, err := normalizeModelPricingOverrideSelector(req.Selector)
	if err != nil {
		return handleError(c, err)
	}
	tenantID := core.GetTenantID(c.Request().Context())
	if err := h.PricingOverrides.DeleteForTenant(c.Request().Context(), tenantID, selector); err != nil {
		if errors.Is(err, pricingoverrides.ErrNotFound) {
			return handleError(c, core.NewNotFoundError("model pricing override not found: "+selector))
		}
		return handleError(c, pricingOverrideWriteError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

// --- Tagging (tenant overrides) ---

func (h *TenantAdminHandler) TaggingSettings(c *echo.Context) error {
	if h.Tagging == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tagging_unavailable"}})
	}
	tenantID := core.GetTenantID(c.Request().Context())
	headers, err := h.Tagging.ListEffectiveRulesForTenant(c.Request().Context(), tenantID)
	if err != nil {
		return handleError(c, err)
	}
	return c.JSON(http.StatusOK, taggingSettingsResponse{
		Headers:  headers,
		Editable: h.Tagging.Editable(),
	})
}

func (h *TenantAdminHandler) UpdateTaggingSettings(c *echo.Context) error {
	if h.Tagging == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"type": "tagging_unavailable"}})
	}
	var req updateTaggingSettingsRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	saved, err := h.Tagging.SaveRulesForTenant(c.Request().Context(), core.GetTenantID(c.Request().Context()), req.Headers)
	if err != nil {
		if tagging.IsValidationError(err) {
			return handleError(c, core.NewInvalidRequestError(err.Error(), err))
		}
		return handleError(c, featureUnavailableError("failed to save tagging rules: "+err.Error()))
	}
	return c.JSON(http.StatusOK, taggingSettingsResponse{
		Headers:  saved,
		Editable: h.Tagging.Editable(),
	})
}
