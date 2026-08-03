package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
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
