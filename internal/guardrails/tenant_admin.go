package guardrails

import "context"

// ListForTenant returns the guardrail definitions persisted for the given
// tenant, bypassing the shared in-memory snapshot.
func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]Definition, error) {
	return s.store.List(ctx, tenantID)
}

// ListEffectiveForTenant returns the effective guardrail definitions for the
// given tenant (tenant overrides merged over the platform-default tenant),
// bypassing the shared in-memory snapshot.
func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]Definition, error) {
	return s.store.ListEffective(ctx, tenantID)
}

// GetForTenant returns one guardrail definition persisted for the given tenant,
// or nil when it does not exist. It bypasses the shared in-memory snapshot.
func (s *Service) GetForTenant(ctx context.Context, tenantID, name string) (*Definition, error) {
	rows, err := s.store.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// UpsertForTenant validates and stores a guardrail definition for the given
// tenant. It only rebuilds and swaps the shared in-memory snapshot when the
// tenant is the service's default tenant; other tenants bypass the cache.
func (s *Service) UpsertForTenant(ctx context.Context, tenantID string, definition Definition) error {
	normalized, err := normalizeDefinition(definition)
	if err != nil {
		return err
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if tenantID == s.tenantID {
		next, err := s.buildUpsertSnapshot(ctx, tenantID, normalized)
		if err != nil {
			return err
		}
		if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
			return guardrailServiceError("upsert guardrail", err)
		}
		s.mu.Lock()
		s.snapshot = next
		s.mu.Unlock()
		return nil
	}
	if err := s.store.Upsert(ctx, tenantID, normalized); err != nil {
		return guardrailServiceError("upsert guardrail", err)
	}
	return nil
}

// DeleteForTenant removes a guardrail definition for the given tenant. It only
// refreshes the shared in-memory snapshot when the tenant is the service's
// default tenant; other tenants bypass the cache.
func (s *Service) DeleteForTenant(ctx context.Context, tenantID, name string) error {
	if err := s.store.Delete(ctx, tenantID, name); err != nil {
		return guardrailServiceError("delete guardrail", err)
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}
