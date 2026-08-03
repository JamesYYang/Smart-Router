package pricingoverrides

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"smartrouter/internal/core"
)

// tenantPricingStore is a tenant-aware Store backed by a (tenantID, selector)
// map. The shared testStore in service_test.go ignores the tenantID parameter,
// so per-tenant cache tests need this one to prove cache isolation.
type tenantPricingStore struct {
	items map[string]map[string]Override
}

func newTenantPricingStore() *tenantPricingStore {
	return &tenantPricingStore{items: map[string]map[string]Override{}}
}

func (s *tenantPricingStore) List(_ context.Context, tenantID string) ([]Override, error) {
	rows := s.items[tenantID]
	result := make([]Override, 0, len(rows))
	for _, item := range rows {
		result = append(result, item)
	}
	return result, nil
}

func (s *tenantPricingStore) ListEffective(_ context.Context, tenantID string) ([]Override, error) {
	// Mirrors the SQLite ListEffective fallthrough: the tenant's own rows win
	// over the platform-default tenant's rows.
	bySelector := map[string]Override{}
	for _, item := range s.items[defaultTenantID] {
		bySelector[item.Selector] = item
	}
	for _, item := range s.items[tenantID] {
		bySelector[item.Selector] = item
	}
	result := make([]Override, 0, len(bySelector))
	for _, item := range bySelector {
		result = append(result, item)
	}
	return result, nil
}

func (s *tenantPricingStore) Upsert(_ context.Context, tenantID string, override Override) error {
	rows := s.items[tenantID]
	if rows == nil {
		rows = map[string]Override{}
		s.items[tenantID] = rows
	}
	rows[override.Selector] = override
	return nil
}

func (s *tenantPricingStore) Delete(_ context.Context, tenantID, selector string) error {
	if rows := s.items[tenantID]; rows != nil {
		delete(rows, selector)
	}
	return nil
}

func (s *tenantPricingStore) Close() error { return nil }

func TestServiceResolvePricingPerTenantIsolation(t *testing.T) {
	store := newTenantPricingStore()
	baseInput := 1.0
	service, err := NewService(
		store,
		testCatalog{providerNames: []string{"openai"}},
		staticPricingResolver{pricing: &core.ModelPricing{InputPerMtok: &baseInput}},
	)
	require.NoError(t, err)

	require.NoError(t, store.Upsert(context.Background(), "default", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(5.0)},
	}))
	require.NoError(t, store.Upsert(context.Background(), "tenant-a", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(2.0)},
	}))
	// Refresh tenant-b too so its snapshot carries the default tenant's
	// fallthrough override — proving tenant-a's override does not leak.
	require.NoError(t, service.RefreshAll(context.Background(), []string{"default", "tenant-a", "tenant-b"}))

	// tenant-a resolves its own override, not the default tenant's.
	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	pricingA := service.ResolvePricing(ctxA, "gpt-4o", "openai")
	require.NotNil(t, pricingA)
	require.NotNil(t, pricingA.InputPerMtok)
	require.Equal(t, 2.0, *pricingA.InputPerMtok)

	// tenant-b's snapshot has no tenant-b override, so the default tenant's
	// fallthrough override applies.
	ctxB := core.WithTenantID(context.Background(), "tenant-b")
	pricingB := service.ResolvePricing(ctxB, "gpt-4o", "openai")
	require.NotNil(t, pricingB)
	require.NotNil(t, pricingB.InputPerMtok)
	require.Equal(t, 5.0, *pricingB.InputPerMtok)
}

func TestServiceResolvePricingFallsBackToBaseWhenTenantNotCached(t *testing.T) {
	baseInput := 1.0
	service, err := NewService(
		newTenantPricingStore(),
		testCatalog{providerNames: []string{"openai"}},
		staticPricingResolver{pricing: &core.ModelPricing{InputPerMtok: &baseInput}},
	)
	require.NoError(t, err)
	require.NoError(t, service.RefreshAll(context.Background(), []string{"tenant-a"}))

	// tenant-b has no snapshot in the map; ResolvePricing falls back to base.
	ctxB := core.WithTenantID(context.Background(), "tenant-b")
	pricing := service.ResolvePricing(ctxB, "gpt-4o", "openai")
	require.NotNil(t, pricing)
	require.NotNil(t, pricing.InputPerMtok)
	require.Equal(t, baseInput, *pricing.InputPerMtok)
}

func TestServiceResolvePricingEmptyCtxFallsBackToDefaultTenant(t *testing.T) {
	store := newTenantPricingStore()
	service, err := NewService(store, testCatalog{providerNames: []string{"openai"}}, nil)
	require.NoError(t, err)

	require.NoError(t, store.Upsert(context.Background(), "default", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(7.0)},
	}))
	// nil tenantIDs => refresh the platform-default tenant only.
	require.NoError(t, service.RefreshAll(context.Background(), nil))

	pricing := service.ResolvePricing(context.Background(), "gpt-4o", "openai")
	require.NotNil(t, pricing)
	require.NotNil(t, pricing.InputPerMtok)
	require.Equal(t, 7.0, *pricing.InputPerMtok)
}

func TestServiceRefreshAllBuildsPerTenantSnapshots(t *testing.T) {
	store := newTenantPricingStore()
	service, err := NewService(store, testCatalog{providerNames: []string{"openai"}}, nil)
	require.NoError(t, err)

	require.NoError(t, store.Upsert(context.Background(), "tenant-a", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(2.0)},
	}))
	require.NoError(t, store.Upsert(context.Background(), "tenant-b", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(3.0)},
	}))

	require.NoError(t, service.RefreshAll(context.Background(), []string{"tenant-a", "tenant-b"}))

	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	ctxB := core.WithTenantID(context.Background(), "tenant-b")
	pricingA := service.ResolvePricing(ctxA, "gpt-4o", "openai")
	pricingB := service.ResolvePricing(ctxB, "gpt-4o", "openai")
	require.NotNil(t, pricingA)
	require.NotNil(t, pricingB)
	require.NotNil(t, pricingA.InputPerMtok)
	require.NotNil(t, pricingB.InputPerMtok)
	require.Equal(t, 2.0, *pricingA.InputPerMtok)
	require.Equal(t, 3.0, *pricingB.InputPerMtok)
}
