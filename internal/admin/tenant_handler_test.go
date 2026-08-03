package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
	"smartrouter/internal/guardrails"
	"smartrouter/internal/workflows"

	_ "modernc.org/sqlite"
)

func TestTenantAdminHandler_CreateAuthKey_AutoScopesToCurrentTenant(t *testing.T) {
	authSvc := newTestAuthKeysService(t)
	h := &TenantAdminHandler{AuthKeys: authSvc}

	// 请求体尝试冒充别的租户 / 提权为 tenant admin —— 都应被忽略
	body := `{"name":"escalate","tenant_id":"someone-else","is_tenant_admin":true}`
	req := httptest.NewRequest(http.MethodPost, "/admin/auth-keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.CreateAuthKey(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)

	list, err := authSvc.List(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.False(t, list[0].IsTenantAdmin, "tenant admin must not be able to self-escalate")
}

func TestTenantAdminHandler_ListAuthKeys_ScopedToCurrentTenant(t *testing.T) {
	authSvc := newTestAuthKeysService(t)
	_, err := authSvc.Create(context.Background(), authkeys.CreateInput{Name: "a", TenantID: "tenant-a"})
	require.NoError(t, err)
	_, err = authSvc.Create(context.Background(), authkeys.CreateInput{Name: "b", TenantID: "tenant-b"})
	require.NoError(t, err)

	h := &TenantAdminHandler{AuthKeys: authSvc}
	req := httptest.NewRequest(http.MethodGet, "/admin/auth-keys", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err = h.ListAuthKeys(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"a"`)
	require.NotContains(t, rec.Body.String(), `"b"`)
}

func TestTenantAdminHandler_WorkflowGuardrails_ValidatedAgainstTenantEffective(t *testing.T) {
	// M1: a tenant's own guardrails (persisted via UpsertForTenant, which
	// bypasses the shared in-memory snapshot) must be visible to workflow
	// authoring — previously validation read the shared default-tenant
	// Names() and rejected the tenant's own guardrail refs.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	store, err := guardrails.NewSQLiteStore(context.Background(), db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	svc, err := guardrails.NewService(store)
	require.NoError(t, err)
	require.NoError(t, svc.Refresh(context.Background()))

	// Seed the platform-default tenant too, so the effective list proves the
	// merge covers both the default snapshot and the tenant override.
	require.NoError(t, svc.Upsert(context.Background(), guardrails.Definition{
		Name: "shared-policy",
		Type: "system_prompt",
		Config: json.RawMessage(`{"mode":"inject","content":"shared policy"}`),
	}))

	// Tenant-a's own guardrail — bypasses the shared snapshot.
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", guardrails.Definition{
		Name: "tenant-policy",
		Type: "system_prompt",
		Config: json.RawMessage(`{"mode":"inject","content":"tenant policy"}`),
	}))
	require.NotContains(t, svc.Names(), "tenant-policy", "tenant-only guardrail must not pollute the shared snapshot")

	h := &TenantAdminHandler{Guardrails: svc}

	// A workflow referencing the tenant's own guardrail validates.
	payload := workflows.Payload{
		SchemaVersion: 1,
		Features:      workflows.FeatureFlags{Guardrails: true},
		Guardrails:    []workflows.GuardrailStep{{Ref: "tenant-policy", Step: 10}},
	}
	require.NoError(t, h.validateTenantWorkflowGuardrails(context.Background(), "tenant-a", payload))

	// The platform-default guardrail is also valid for the tenant (effective merge).
	payload.Guardrails[0].Ref = "shared-policy"
	require.NoError(t, h.validateTenantWorkflowGuardrails(context.Background(), "tenant-a", payload))

	// An unknown ref is still rejected.
	payload.Guardrails[0].Ref = "does-not-exist"
	err = h.validateTenantWorkflowGuardrails(context.Background(), "tenant-a", payload)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown guardrail ref: does-not-exist")

	// ListWorkflowGuardrails shows the tenant's effective guardrails.
	req := httptest.NewRequest(http.MethodGet, "/admin/workflows/guardrails", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	require.NoError(t, h.ListWorkflowGuardrails(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "tenant-policy")
	require.Contains(t, rec.Body.String(), "shared-policy")
}
