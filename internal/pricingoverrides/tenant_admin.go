package pricingoverrides

import (
	"context"

	"smartrouter/internal/modelselectors"
)

// ListForTenant returns all pricing overrides persisted for one tenant.
func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]Override, error) {
	return s.store.List(ctx, tenantID)
}

// ListEffectiveForTenant returns pricing overrides effective for one tenant,
// including fallthrough overrides from the platform-default tenant.
func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]Override, error) {
	return s.store.ListEffective(ctx, tenantID)
}

// GetForTenant returns one persisted pricing override for a tenant, or nil,nil if not found.
func (s *Service) GetForTenant(ctx context.Context, tenantID, selector string) (*Override, error) {
	rows, err := s.store.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Selector == selector {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// UpsertForTenant validates and persists one override for a tenant. The shared
// inference-time cache is refreshed only when tenantID is the platform-default tenant.
func (s *Service) UpsertForTenant(ctx context.Context, tenantID string, override Override) error {
	normalized, err := normalizeOverrideInput(s.catalog, override)
	if err != nil {
		return err
	}
	if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
		return err
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}

// DeleteForTenant removes one override for a tenant. The shared inference-time
// cache is refreshed only when tenantID is the platform-default tenant; no
// rollback is needed because non-default tenants never touch the shared cache.
func (s *Service) DeleteForTenant(ctx context.Context, tenantID, selector string) error {
	parts, err := modelselectors.NormalizeInput(s.catalog, selector)
	if err != nil {
		return err
	}
	if err := s.store.Delete(ctx, tenantID, parts.Selector); err != nil {
		return err
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}
