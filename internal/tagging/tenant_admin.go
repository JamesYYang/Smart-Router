package tagging

import (
	"context"
	"fmt"

	"smartrouter/internal/core"
)

// GetRulesForTenant returns the persisted operator rules for tenantID directly
// from the store, bypassing the shared in-memory snapshot.
func (s *Service) GetRulesForTenant(ctx context.Context, tenantID string) ([]Rule, error) {
	return s.store.GetRules(ctx, tenantID)
}

// ListEffectiveRulesForTenant returns the effective rule set for tenantID
// (default-tenant rules merged with tenant-specific ones) directly from the
// store, bypassing the shared in-memory snapshot.
func (s *Service) ListEffectiveRulesForTenant(ctx context.Context, tenantID string) ([]Rule, error) {
	return s.store.ListEffectiveRules(ctx, tenantID)
}

// SaveRulesForTenant persists rules for tenantID and write-through refreshes
// that tenant's in-memory snapshot, so the live TaggingCapture middleware sees
// the new rules immediately. The affected tenant is carried in ctx so Refresh
// rebuilds exactly that tenant's snapshot; for the platform-default tenant
// this matches the legacy behavior (refresh the shared snapshot).
func (s *Service) SaveRulesForTenant(ctx context.Context, tenantID string, rules []Rule) ([]Rule, error) {
	if err := NormalizeRules(rules); err != nil {
		return nil, fmt.Errorf("tagging rules: %w", err)
	}
	if err := s.store.SaveRules(ctx, tenantID, rules); err != nil {
		return nil, err
	}
	refreshCtx := core.WithTenantID(ctx, tenantID)
	if err := s.Refresh(refreshCtx); err != nil {
		return nil, err
	}
	return rules, nil
}
