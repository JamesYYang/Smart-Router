package tagging

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"smartrouter/internal/core"
)

// tenantStore returns rules keyed by tenant ID, modeling the SQLite/Postgres
// store's per-tenant rule sets.
type tenantStore struct {
	rules map[string][]Rule
}

func (f *tenantStore) GetRules(_ context.Context, tenantID string) ([]Rule, error) {
	return f.rules[tenantID], nil
}

func (f *tenantStore) SaveRules(_ context.Context, tenantID string, rules []Rule) error {
	f.rules[tenantID] = rules
	return nil
}

func (f *tenantStore) ListEffectiveRules(_ context.Context, _ string) ([]Rule, error) {
	return nil, nil
}

func (f *tenantStore) Close() error { return nil }

func TestService_ExtractLabels_PerTenantIsolation(t *testing.T) {
	store := &tenantStore{rules: map[string][]Rule{
		"default":  {{Header: "X-Team", Prefix: "team-", DoNotPass: true}},
		"tenant-a": {{Header: "X-Cost-Center", Prefix: "cc-"}},
	}}
	service := NewService(nil, store)
	require.NoError(t, service.RefreshAll(context.Background(), []string{"default", "tenant-a"}))

	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	ctxB := core.WithTenantID(context.Background(), "tenant-b")
	headers := http.Header{
		"X-Team":        {"team-alpha"},
		"X-Cost-Center": {"cc-42"},
	}

	// tenant-a's rule produces labels only for tenant-a.
	require.Equal(t, []string{"42"}, service.ExtractLabels(ctxA, headers))
	// Default tenant (empty ctx) sees only its own rule.
	require.Equal(t, []string{"alpha"}, service.ExtractLabels(context.Background(), headers))
	// tenant-b has no cached snapshot: no labels, no strip set.
	require.Nil(t, service.ExtractLabels(ctxB, headers))
	require.Nil(t, service.StripHeaders(ctxB))

	// Strip set is per-tenant: only the default tenant's X-Team is do-not-pass.
	_, ok := service.StripHeaders(ctxA)["X-Team"]
	require.False(t, ok, "tenant-a must not inherit default tenant's strip set")
	_, ok = service.StripHeaders(context.Background())["X-Team"]
	require.True(t, ok)
}

func TestService_Refresh_RefreshesCtxTenant(t *testing.T) {
	store := &tenantStore{rules: map[string][]Rule{
		"tenant-a": {{Header: "X-A", Prefix: "a-"}},
	}}
	service := NewService(nil, store)

	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	require.NoError(t, service.Refresh(ctxA))
	require.Equal(t, []string{"1"}, service.ExtractLabels(ctxA, http.Header{"X-A": {"a-1"}}))

	// Default tenant snapshot is unaffected.
	require.Empty(t, service.Rules())
	require.Nil(t, service.ExtractLabels(context.Background(), http.Header{"X-A": {"a-1"}}))
}

func TestService_RefreshAll(t *testing.T) {
	store := &tenantStore{rules: map[string][]Rule{
		"default":  {{Header: "X-Team", Prefix: "team-"}},
		"tenant-a": {{Header: "X-Cost-Center", Prefix: "cc-"}},
	}}
	service := NewService(nil, store)
	// Fresh service has no stored rules in the default snapshot.
	require.Empty(t, service.Rules())

	require.NoError(t, service.RefreshAll(context.Background(), []string{"default", "tenant-a"}))

	require.Len(t, service.Rules(), 1)
	require.Equal(t, "X-Team", service.Rules()[0].Header)
	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	require.Equal(t, []string{"42"}, service.ExtractLabels(ctxA, http.Header{"X-Cost-Center": {"cc-42"}}))

	// An empty tenantIDs list falls back to the default tenant only, replacing
	// the whole map.
	require.NoError(t, service.RefreshAll(context.Background(), nil))
	require.Len(t, service.Rules(), 1)
	require.Nil(t, service.ExtractLabels(ctxA, http.Header{"X-Cost-Center": {"cc-42"}}))
}

func TestService_HasRules_PerTenant(t *testing.T) {
	store := &tenantStore{rules: map[string][]Rule{
		"tenant-a": {{Header: "X-A"}},
	}}
	service := NewService(nil, store)
	require.NoError(t, service.RefreshAll(context.Background(), []string{"tenant-a"}))

	require.True(t, service.HasRules(core.WithTenantID(context.Background(), "tenant-a")))
	require.False(t, service.HasRules(context.Background()))
	require.False(t, service.HasRules(core.WithTenantID(context.Background(), "tenant-b")))
}
