package tagging

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"smartrouter/internal/core"
)

// snapshotTags is the immutable, atomically swapped view served on the hot path.
type snapshotTags struct {
	rules []Rule
	strip map[string]struct{}
}

const defaultTenantID = "default"

// Service merges declarative (config/env) tagging rules over operator rules
// persisted in the store and serves label extraction on the request hot path.
//
// snapshots holds one compiled snapshot per tenant ID (map[tenantID]snapshotTags).
// The hot path (ExtractLabels/StripHeaders/HasRules) selects the calling
// tenant's snapshot via core.GetTenantID(ctx), falling back to the default
// tenant when ctx carries no tenant ID. The tenantID field identifies the
// platform-default tenant used by the legacy admin/management methods and by
// the P4 ForTenant admin methods — it is not the tenant index for the cache.
type Service struct {
	store Store

	// configRules are supplied declaratively (config.yaml / TAGGING_HEADER_*).
	// They override store rows with the same header and are read-only.
	configRules []Rule

	tenantID  string
	snapshots atomic.Value // map[string]snapshotTags, keyed by tenant ID
	refreshMu sync.Mutex
}

// NewService creates a tagging service. configRules must already be normalized
// by config.Load; store may be nil, in which case only config rules apply and
// dashboard edits are unavailable.
func NewService(configRules []Rule, store Store) *Service {
	managed := make([]Rule, len(configRules))
	for i, rule := range configRules {
		rule.Managed = true
		managed[i] = rule
	}
	service := &Service{store: store, configRules: managed, tenantID: defaultTenantID}
	service.snapshots.Store(map[string]snapshotTags{defaultTenantID: buildSnapshotTags(managed)})
	return service
}

func buildSnapshotTags(rules []Rule) snapshotTags {
	return snapshotTags{rules: rules, strip: StripHeaderSet(rules)}
}

// Refresh reloads operator rules from the store for the tenant carried in ctx
// (falling back to the default tenant when ctx carries no tenant ID) and
// atomically swaps that tenant's snapshot. It is the immediate single-tenant
// refresh path (e.g. admin writes); startup and background ticks use
// RefreshAll instead.
func (s *Service) Refresh(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	tenantID := core.GetTenantID(ctx)
	if tenantID == "" {
		tenantID = defaultTenantID
	}
	stored, err := s.store.GetRules(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("load tagging rules for tenant %s: %w", tenantID, err)
	}
	if err := NormalizeRules(stored); err != nil {
		return fmt.Errorf("stored tagging rules: %w", err)
	}
	next := buildSnapshotTags(s.mergeConfigRules(stored))
	cloned := cloneSnapshotMap(s.snapshotMap())
	cloned[tenantID] = next
	s.snapshots.Store(cloned)
	return nil
}

// RefreshAll rebuilds the full per-tenant snapshot map from storage, one
// snapshot per tenantID. It is the startup and background-refresh path; unlike
// Refresh it does not depend on the tenant carried in ctx. An empty tenantIDs
// list falls back to the default tenant only.
func (s *Service) RefreshAll(ctx context.Context, tenantIDs []string) error {
	if s == nil || s.store == nil {
		return nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if len(tenantIDs) == 0 {
		tenantIDs = []string{defaultTenantID}
	}
	newMap := make(map[string]snapshotTags, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		if tenantID == "" {
			tenantID = defaultTenantID
		}
		stored, err := s.store.GetRules(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("tagging refresh tenant %s: %w", tenantID, err)
		}
		if err := NormalizeRules(stored); err != nil {
			return fmt.Errorf("stored tagging rules for tenant %s: %w", tenantID, err)
		}
		newMap[tenantID] = buildSnapshotTags(s.mergeConfigRules(stored))
	}
	s.snapshots.Store(newMap)
	return nil
}

// snapshotFor returns the immutable snapshot for the tenant carried in ctx
// (falling back to the default tenant), or an empty snapshot when the tenant
// has no cached rules yet. The returned snapshot is valid for the duration of
// the call: map swaps publish whole new maps and never mutate existing
// snapshots, so no lock is needed on the read path.
func (s *Service) snapshotFor(ctx context.Context) snapshotTags {
	if s == nil {
		return snapshotTags{}
	}
	tenantID := core.GetTenantID(ctx)
	if tenantID == "" {
		tenantID = defaultTenantID
	}
	return s.snapshotForTenant(tenantID)
}

// snapshotForTenant selects the snapshot for one explicit tenant ID.
func (s *Service) snapshotForTenant(tenantID string) snapshotTags {
	if snap, ok := s.snapshotMap()[tenantID]; ok {
		return snap
	}
	return snapshotTags{}
}

// snapshot returns the platform-default tenant's snapshot. Used by the legacy
// admin/management methods (Rules, ListEffectiveRules) which operate on the
// default tenant.
func (s *Service) snapshot() snapshotTags {
	return s.snapshotForTenant(s.tenantID)
}

// snapshotMap returns the current per-tenant snapshot map, or an empty map when
// the atomic value has not been seeded.
func (s *Service) snapshotMap() map[string]snapshotTags {
	if s == nil {
		return map[string]snapshotTags{}
	}
	if m, ok := s.snapshots.Load().(map[string]snapshotTags); ok {
		return m
	}
	return map[string]snapshotTags{}
}

func cloneSnapshotMap(current map[string]snapshotTags) map[string]snapshotTags {
	next := make(map[string]snapshotTags, len(current))
	for tenantID, snap := range current {
		next[tenantID] = snap
	}
	return next
}

// mergeConfigRules overlays the config-managed rules onto the store rows.
// Store rows shadowed by a managed rule of the same header are dropped, so
// config stays the source of truth for the headers it declares.
func (s *Service) mergeConfigRules(stored []Rule) []Rule {
	merged := make([]Rule, 0, len(s.configRules)+len(stored))
	merged = append(merged, s.configRules...)
	for _, rule := range stored {
		if s.isManagedHeader(rule.Header) {
			continue
		}
		rule.Managed = false
		merged = append(merged, rule)
	}
	return merged
}

// isManagedHeader reports whether header is owned by a declarative config rule.
func (s *Service) isManagedHeader(header string) bool {
	for _, rule := range s.configRules {
		if strings.EqualFold(rule.Header, header) {
			return true
		}
	}
	return false
}

// Rules returns the effective default-tenant rules: managed config rules first,
// then operator rules from the store.
func (s *Service) Rules() []Rule {
	current := s.snapshot()
	rules := make([]Rule, len(current.rules))
	copy(rules, current.rules)
	return rules
}

// ListEffectiveRules merges config rules (stamped tenant_id='default') with
// operator rules persisted for the given tenant, then returns the full effective
// rule set. Tenant-stored rules win over config rules by header key.
func (s *Service) ListEffectiveRules(ctx context.Context) ([]Rule, error) {
	if s.store == nil {
		return s.configRules, nil
	}
	stored, err := s.store.ListEffectiveRules(ctx, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("list effective tagging rules: %w", err)
	}
	merged := make([]Rule, 0, len(s.configRules)+len(stored))
	merged = append(merged, s.configRules...)
	for _, rule := range stored {
		if s.isManagedHeader(rule.Header) {
			continue
		}
		rule.Managed = false
		merged = append(merged, rule)
	}
	return merged, nil
}

// SaveRules validates and persists the operator-managed rule set (replacing
// the previous set), refreshes the snapshot, and returns the merged view.
// Rules whose header is declared in config/env are rejected as read-only.
func (s *Service) SaveRules(ctx context.Context, rules []Rule) ([]Rule, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("tagging rules storage is not available")
	}
	if err := NormalizeRules(rules); err != nil {
		return nil, err
	}
	for _, rule := range rules {
		if s.isManagedHeader(rule.Header) {
			return nil, newValidationError("header %q is managed by config/env and is read-only", rule.Header)
		}
	}
	for i := range rules {
		rules[i].Managed = false
	}
	if err := s.store.SaveRules(ctx, s.tenantID, rules); err != nil {
		return nil, fmt.Errorf("save tagging rules: %w", err)
	}
	if err := s.Refresh(ctx); err != nil {
		return nil, err
	}
	return s.Rules(), nil
}

// Editable reports whether operator rules can be persisted.
func (s *Service) Editable() bool {
	return s != nil && s.store != nil
}

// ExtractLabels returns the request labels for the given inbound headers,
// applying the tagging rules of the tenant resolved from ctx.
func (s *Service) ExtractLabels(ctx context.Context, headers http.Header) []string {
	return ExtractLabels(s.snapshotFor(ctx).rules, headers)
}

// StripHeaders returns the canonical header names that must not be forwarded
// to upstream providers for the tenant resolved from ctx. Callers must treat
// the returned map as read-only.
func (s *Service) StripHeaders(ctx context.Context) map[string]struct{} {
	return s.snapshotFor(ctx).strip
}

// HasRules reports whether the tenant resolved from ctx currently has any
// active tagging rule.
func (s *Service) HasRules(ctx context.Context) bool {
	return len(s.snapshotFor(ctx).rules) > 0
}
