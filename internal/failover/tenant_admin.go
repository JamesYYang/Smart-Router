package failover

import "context"

func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]Rule, error) {
	return s.store.List(ctx, tenantID)
}

func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]Rule, error) {
	return s.store.ListEffective(ctx, tenantID)
}

func (s *Service) GetForTenant(ctx context.Context, tenantID, source string) (*Rule, error) {
	return s.store.Get(ctx, tenantID, source)
}

func (s *Service) UpsertForTenant(ctx context.Context, tenantID string, rule Rule) error {
	normalized, err := s.normalizeForUpsert(rule)
	if err != nil {
		return err
	}
	if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
		return err
	}
	if tenantID == defaultTenantID {
		return s.Refresh(ctx)
	}
	return nil
}

func (s *Service) DeleteForTenant(ctx context.Context, tenantID, source string) error {
	if err := s.store.Delete(ctx, tenantID, source); err != nil {
		return err
	}
	if tenantID == defaultTenantID {
		return s.Refresh(ctx)
	}
	return nil
}

func (s *Service) DeleteAllForTenant(ctx context.Context, tenantID string) error {
	if err := s.store.DeleteAll(ctx, tenantID); err != nil {
		return err
	}
	if tenantID == defaultTenantID {
		return s.Refresh(ctx)
	}
	return nil
}
