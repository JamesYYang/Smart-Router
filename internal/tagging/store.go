package tagging

import "context"

// Store persists the operator-managed tagging rules (the dashboard-editable
// set). Declarative config/env rules are never stored.
type Store interface {
	// GetRules returns the persisted operator rules for the given tenant,
	// empty when none were saved.
	GetRules(ctx context.Context, tenantID string) ([]Rule, error)

	// SaveRules replaces the persisted operator rule set for the given tenant.
	SaveRules(ctx context.Context, tenantID string, rules []Rule) error

	// ListEffectiveRules merges the default tenant's stored rules with the
	// given tenant's stored rules. Tenant-specific rules win over defaults
	// when they share the same header key.
	ListEffectiveRules(ctx context.Context, tenantID string) ([]Rule, error)

	// Close releases store resources.
	Close() error
}

// rulesSettingKey is the single settings key the rule set is stored under.
const rulesSettingKey = "headers"
