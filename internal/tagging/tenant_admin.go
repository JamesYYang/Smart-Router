package tagging

import (
	"context"
	"fmt"
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

// SaveRulesForTenant persists rules for tenantID. The shared in-memory
// snapshot used by the live TaggingCapture middleware is refreshed only
// when tenantID is the platform-default tenant (s.tenantID); a non-default
// tenant's rules do not affect live traffic until P5.
func (s *Service) SaveRulesForTenant(ctx context.Context, tenantID string, rules []Rule) ([]Rule, error) {
	if err := NormalizeRules(rules); err != nil {
		return nil, fmt.Errorf("tagging rules: %w", err)
	}
	if err := s.store.SaveRules(ctx, tenantID, rules); err != nil {
		return nil, err
	}
	if tenantID == s.tenantID {
		if err := s.Refresh(ctx); err != nil {
			return nil, err
		}
	}
	return rules, nil
}
