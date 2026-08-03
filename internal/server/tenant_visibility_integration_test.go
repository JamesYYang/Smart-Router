package server

// P5 Task 10: end-to-end tenant visibility on the inference hot path.
//
// The chain mirrors internal/server/http.go's /v1/* surface:
//   - TenantResolver (active because baseDomain is configured — a.smart-router.com
//     / b.smart-router.com / app.smart-router.com resolve exactly as in production)
//   - AuthMiddlewareWithAuthenticator (master key + managed auth keys)
//   - WorkflowResolutionWithResolverAndPolicy with the REAL tenant-aware
//     virtualmodels.Service as the selector resolver (the same wiring http.go
//     uses), so the hot-path model resolution consults the calling tenant's own
//     snapshot.
//   - /v1 group carrying hostGuard("tenant") (Task 1)
//
// Real SQLite stores and real services are used throughout; the only stub is
// the /v1/chat/completions handler, which records the resolved model that the
// real WorkflowResolution middleware computed. The brief's four goals map to
// TestTenantVisibility.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
	"smartrouter/internal/providers"
	"smartrouter/internal/tenants"
	"smartrouter/internal/virtualmodels"

	_ "modernc.org/sqlite"
)

const (
	tenantVisBaseDomain   = "smart-router.com"
	tenantVisPlatformHost = "app"
	tenantVisMasterKey    = "master-secret"
)

// tenantVisAlias is the virtual-model override source that only tenant A owns.
const tenantVisAlias = "acme-alias"

// tenantVisChain captures request-visible state from the stub inference handler.
type tenantVisChain struct {
	e             *echo.Echo
	gotResolution *core.RequestModelResolution
	gotTenantID   string
}

// tenantVisEcho assembles the real /v1/* inference chain (mirrors http.go's
// middleware ordering: tenant resolution → auth → workflow/model resolution →
// route-level hostGuard).
func tenantVisEcho(t *testing.T, authSvc *authkeys.Service, tenantSvc *tenants.Service, provider core.RoutableProvider, vmSvc *virtualmodels.Service, chain *tenantVisChain) *echo.Echo {
	t.Helper()
	e := echo.New()
	e.Use(TenantResolver(tenantSvc, tenantVisBaseDomain, tenantVisPlatformHost))
	e.Use(AuthMiddlewareWithAuthenticator(tenantVisMasterKey, authSvc, nil))
	// Real workflow-resolution middleware with the real tenant-aware virtualmodels
	// service as the selector resolver — resolution here is exactly what the
	// production hot path runs before the inference handler.
	e.Use(WorkflowResolutionWithResolverAndPolicy(provider, vmSvc, nil))
	v1 := e.Group("/v1", hostGuard("tenant"))
	v1.POST("/chat/completions", func(c *echo.Context) error {
		chain.gotTenantID = core.GetTenantID(c.Request().Context())
		if wf := core.GetWorkflow(c.Request().Context()); wf != nil {
			chain.gotResolution = wf.Resolution
		}
		return c.NoContent(http.StatusOK)
	})
	// Real GET /v1/models handler (provider list merged with the tenant-aware
	// virtual-model exposure).
	handler := &Handler{provider: provider, exposedModelLister: vmSvc}
	v1.GET("/models", handler.ListModels)
	return e
}

// TestTenantVisibility covers the brief's four goals end to end:
//
//  1. Platform host POST /v1/chat/completions → 404 (hostGuard("tenant")).
//  2. Tenant A's API key on a.smart-router.com → 200, and the workflow/model
//     resolution used tenant A's OWN snapshot (the acme-alias override), not the
//     default tenant's.
//  3. Tenant A's API key on an unresolved host → 401 (the P5
//     enforceTenantAndRole fix).
//  4. Tenant A's virtual-model override affects tenant A's inference and
//     /v1/models listing but not tenant B's.
func TestTenantVisibility(t *testing.T) {
	ctx := context.Background()

	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	tenantSvc := tenants.NewService(tenantStore, time.Minute, tenantVisPlatformHost)
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-a", Subdomain: "a", Name: "Tenant A", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-b", Subdomain: "b", Name: "Tenant B", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))

	// One mock provider backs both the model catalog (so the redirect target
	// openai/gpt-4o is available for resolution/exposure) and the inference
	// server (so the resolved selector is supported).
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o"},
		providerTypes:   map[string]string{"openai/gpt-4o": "openai"},
		modelsResponse:  &core.ModelsResponse{Object: "list", Data: []core.Model{{ID: "gpt-4o", Object: "model", OwnedBy: "openai"}}},
	}
	vmRegistry := providers.NewModelRegistry()
	vmRegistry.RegisterProviderWithType(mock, "openai")
	require.NoError(t, vmRegistry.Initialize(ctx))

	vmStore, err := virtualmodels.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = vmStore.Close() })
	vmSvc, err := virtualmodels.NewService(vmStore, vmRegistry, true)
	require.NoError(t, err)

	// Tenant API keys (non-admin inference keys scoped to each tenant).
	keyA, err := authSvc.Create(ctx, authkeys.CreateInput{Name: "A inference", TenantID: "tenant-a"})
	require.NoError(t, err)
	keyB, err := authSvc.Create(ctx, authkeys.CreateInput{Name: "B inference", TenantID: "tenant-b"})
	require.NoError(t, err)

	// Tenant A creates a virtual-model override. UpsertForTenant does not refresh
	// the live cache for non-default tenants, so RefreshAll materializes the
	// per-tenant snapshots (default/tenant-a/tenant-b) exactly as the P5
	// background-refresh path does.
	require.NoError(t, vmSvc.UpsertForTenant(ctx, "tenant-a", virtualmodels.VirtualModel{
		Source:  tenantVisAlias,
		Targets: []virtualmodels.Target{{Model: "openai/gpt-4o"}},
		Enabled: true,
	}))
	require.NoError(t, vmSvc.RefreshAll(ctx, []string{"default", "tenant-a", "tenant-b"}))

	chain := &tenantVisChain{}
	e := tenantVisEcho(t, authSvc, tenantSvc, mock, vmSvc, chain)

	platformHost := tenantVisPlatformHost + "." + tenantVisBaseDomain
	hostA := "a." + tenantVisBaseDomain
	hostB := "b." + tenantVisBaseDomain
	chatBody := func(model string) map[string]any {
		return map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": "hi"}}}
	}

	// ---- Goal 1: platform host POST /v1/chat/completions → 404
	// (hostGuard("tenant")). A supported model is used so the workflow-resolution
	// middleware passes and the route-level hostGuard is actually reached.
	// hostGuard replies NoContent, so the empty body distinguishes this from a
	// model_not_found 404 (which carries a JSON error body).
	rec := adminSplitReq(t, e, http.MethodPost, platformHost, "/v1/chat/completions", tenantVisMasterKey, chatBody("gpt-4o"))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Empty(t, rec.Body.String(), "hostGuard 404 must be an empty body (route-level, not model_not_found)")

	// Control for Goal 2: on the platform host the default tenant's snapshot does
	// NOT resolve tenant A's alias — validation fails (404 model_not_found with a
	// JSON body) before hostGuard. This proves the 200 on tenant A's host comes
	// from tenant A's snapshot, not the default tenant's.
	rec = adminSplitReq(t, e, http.MethodPost, platformHost, "/v1/chat/completions", tenantVisMasterKey, chatBody(tenantVisAlias))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "model_not_found")

	// ---- Goal 2: tenant A's key on tenant A's host → 200, and the
	// workflow/model resolution used tenant A's OWN snapshot (the override
	// applied: acme-alias → openai/gpt-4o).
	rec = adminSplitReq(t, e, http.MethodPost, hostA, "/v1/chat/completions", keyA.Value, chatBody(tenantVisAlias))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "tenant-a", chain.gotTenantID)
	require.NotNil(t, chain.gotResolution, "workflow resolution must be populated by the real WorkflowResolution middleware")
	require.Equal(t, "openai/gpt-4o", chain.gotResolution.ResolvedQualifiedModel())
	require.True(t, chain.gotResolution.AliasApplied, "tenant A's alias must be applied from tenant A's snapshot")

	// ---- Goal 3: tenant A's key on an unresolved host → 401 (the P5
	// enforceTenantAndRole fix — foreign Host header leaves no tenant ctx, so the
	// tenant-scoped key is rejected instead of leaking through).
	rec = adminSplitReq(t, e, http.MethodPost, "unresolved.example.com", "/v1/chat/completions", keyA.Value, chatBody("gpt-4o"))
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "key_tenant_mismatch")

	// ---- Goal 4: tenant B is NOT affected by tenant A's override.
	// 4a. GET /v1/models: tenant A sees acme-alias, tenant B does not.
	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/v1/models", keyA.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), tenantVisAlias, "tenant A must see its own virtual-model override in /v1/models")

	rec = adminSplitReq(t, e, http.MethodGet, hostB, "/v1/models", keyB.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), tenantVisAlias, "tenant B must not see tenant A's virtual-model override in /v1/models")

	// 4b. Inference: tenant B's request for tenant A's alias is not redirected by
	// tenant B's (empty) snapshot — it falls through to the literal model and is
	// rejected as unsupported, while the same request on tenant A's host returned
	// 200 above.
	rec = adminSplitReq(t, e, http.MethodPost, hostB, "/v1/chat/completions", keyB.Value, chatBody(tenantVisAlias))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "model_not_found")
}
