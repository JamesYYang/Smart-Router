package virtualmodels

import "context"

// ListForTenant returns the raw override rows stored for tenantID (no
// platform-default merge), bypassing the shared inference-time cache.
// Intended for admin handlers managing one tenant's overrides directly.
func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	return s.store.List(ctx, tenantID)
}

// ListEffectiveForTenant returns tenantID's effective view (tenant
// override merged over the platform default), bypassing the shared cache.
func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	return s.store.ListEffective(ctx, tenantID)
}

// GetForTenant returns a single row scoped to tenantID, bypassing the cache.
func (s *Service) GetForTenant(ctx context.Context, tenantID, source string) (*VirtualModel, error) {
	return s.store.Get(ctx, tenantID, source)
}

// UpsertForTenant upserts vm under tenantID. The shared inference-time
// cache is refreshed only when tenantID is the platform-default tenant
// (s.tenantID) — a non-default tenant's override does not affect live
// routing until P5 makes the cache tenant-aware.
func (s *Service) UpsertForTenant(ctx context.Context, tenantID string, vm VirtualModel) error {
	normalized, err := s.normalizeForUpsert(vm)
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

// DeleteForTenant deletes source under tenantID. See UpsertForTenant for
// the shared-cache refresh rule.
func (s *Service) DeleteForTenant(ctx context.Context, tenantID, source string) error {
	if err := s.store.Delete(ctx, tenantID, source); err != nil {
		return err
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}
