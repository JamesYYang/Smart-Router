package failover

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"smartrouter/config"
	"smartrouter/internal/core"
)

// defaultTenantID is the tenant that owns the shared inference-time failover
// cache (the default/single-tenant deployment).
const defaultTenantID = "default"

// Service merges dashboard-managed mappings with read-only config/env mappings.
type Service struct {
	store      Store
	configRows []Rule
	snapshots  atomic.Value // map[string]*ruleSnapshot, keyed by tenant ID
	refreshMu  sync.Mutex
}

// ruleSnapshot is the immutable, atomically-published view of the merged rules.
// It caches the derived Rules/Disabled lookup maps so the per-request resolver
// hot path reads them without re-cloning rows or rebuilding maps on every call.
// The maps and their slices must be treated as read-only by callers.
type ruleSnapshot struct {
	rows     []Rule
	rules    map[string][]string
	disabled map[string]bool
}

func newRuleSnapshot(rows []Rule) *ruleSnapshot {
	rules := make(map[string][]string)
	disabled := make(map[string]bool)
	for _, row := range rows {
		if !row.Enabled {
			disabled[row.Source] = true
			continue
		}
		targets := normalizeTargets(row.Targets)
		if len(targets) == 0 {
			continue
		}
		rules[row.Source] = targets
	}
	return &ruleSnapshot{rows: rows, rules: rules, disabled: disabled}
}

func NewService(store Store, cfg config.FailoverConfig) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	service := &Service{store: store, configRows: ConfigRules(cfg)}
	service.snapshots.Store(map[string]*ruleSnapshot{defaultTenantID: newRuleSnapshot(nil)})
	return service, nil
}

func ConfigRules(cfg config.FailoverConfig) []Rule {
	rows := make([]Rule, 0, len(cfg.Manual))
	now := time.Now().UTC()
	for source, targets := range cfg.Manual {
		source = strings.TrimSpace(source)
		if source == "" {
			continue
		}
		cleanTargets := normalizeTargets(targets)
		rows = append(rows, Rule{
			Source:        source,
			Targets:       cleanTargets,
			Enabled:       !cfg.Disabled[source],
			ManagedSource: ManagedSourceConfig,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	}
	for source := range cfg.Disabled {
		source = strings.TrimSpace(source)
		if source == "" {
			continue
		}
		if hasRule(rows, source) {
			continue
		}
		rows = append(rows, Rule{
			Source:        source,
			Enabled:       false,
			ManagedSource: ManagedSourceConfig,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	}
	return rows
}

func hasRule(rows []Rule, source string) bool {
	for _, row := range rows {
		if row.Source == source {
			return true
		}
	}
	return false
}

// Refresh rebuilds the snapshot for the tenant carried in ctx (falling back to
// the default tenant when ctx carries no tenant ID). It merges only that
// tenant's stored rows into the existing per-tenant map, so concurrent refreshes
// of other tenants are not lost. This is the immediate single-tenant refresh
// path (e.g. admin writes, Recalculate); startup and background ticks use
// RefreshAll instead.
func (s *Service) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	tenantID := core.GetTenantID(ctx)
	if tenantID == "" {
		tenantID = defaultTenantID
	}
	rows, err := s.store.ListEffective(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("list failover mappings for tenant %s: %w", tenantID, err)
	}
	next := newRuleSnapshot(s.mergeConfig(rows))
	cloned := cloneSnapshotMap(s.currentSnapshotMap())
	cloned[tenantID] = next
	s.snapshots.Store(cloned)
	return nil
}

// RefreshAll rebuilds the full per-tenant snapshot map from storage. It is the
// startup and background-refresh path; unlike Refresh it does not depend on the
// tenant carried in ctx. An empty tenantIDs list falls back to the default
// tenant only.
func (s *Service) RefreshAll(ctx context.Context, tenantIDs []string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if len(tenantIDs) == 0 {
		tenantIDs = []string{defaultTenantID}
	}
	newMap := make(map[string]*ruleSnapshot, len(tenantIDs))
	for _, tid := range tenantIDs {
		rows, err := s.store.ListEffective(ctx, tid)
		if err != nil {
			return fmt.Errorf("failover refresh tenant %s: %w", tid, err)
		}
		newMap[tid] = newRuleSnapshot(s.mergeConfig(rows))
	}
	s.snapshots.Store(newMap)
	return nil
}

// snapshotFor returns the immutable snapshot for the tenant carried in ctx
// (falling back to the default tenant), or an empty snapshot when the tenant has
// no cached rules yet. The returned snapshot is valid for the duration of the
// call: map swaps publish whole new maps and never mutate existing snapshots, so
// no lock is needed on the read path.
func (s *Service) snapshotFor(ctx context.Context) *ruleSnapshot {
	tenantID := core.GetTenantID(ctx)
	if tenantID == "" {
		tenantID = defaultTenantID
	}
	if snap, ok := s.currentSnapshotMap()[tenantID]; ok {
		return snap
	}
	return &ruleSnapshot{}
}

func (s *Service) currentSnapshotMap() map[string]*ruleSnapshot {
	m, _ := s.snapshots.Load().(map[string]*ruleSnapshot)
	return m
}

func cloneSnapshotMap(m map[string]*ruleSnapshot) map[string]*ruleSnapshot {
	cloned := make(map[string]*ruleSnapshot, len(m))
	for tenantID, snap := range m {
		cloned[tenantID] = snap
	}
	return cloned
}

func (s *Service) mergeConfig(stored []Rule) []Rule {
	managed := make(map[string]struct{}, len(s.configRows))
	for _, row := range s.configRows {
		managed[row.Source] = struct{}{}
	}
	merged := make([]Rule, 0, len(stored)+len(s.configRows))
	for _, row := range stored {
		if _, ok := managed[strings.TrimSpace(row.Source)]; ok {
			continue
		}
		row.ManagedSource = ManagedSourceDashboard
		merged = append(merged, row.clone())
	}
	for _, row := range s.configRows {
		row.ManagedSource = ManagedSourceConfig
		merged = append(merged, row.clone())
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Source < merged[j].Source })
	return merged
}

// Rules returns the enabled source -> targets map for the tenant carried in
// ctx. The returned map is the cached snapshot and must not be mutated by
// callers.
func (s *Service) Rules(ctx context.Context) map[string][]string {
	return s.snapshotFor(ctx).rules
}

// Disabled returns the set of disabled sources for the tenant carried in ctx,
// or nil when none. The returned map is the cached snapshot and must not be
// mutated by callers.
func (s *Service) Disabled(ctx context.Context) map[string]bool {
	snap := s.snapshotFor(ctx)
	if len(snap.disabled) == 0 {
		return nil
	}
	return snap.disabled
}

// loadSnapshot returns the default tenant's snapshot for the legacy
// admin/dashboard methods (List/ListViews/Get) that predate per-tenant caching.
func (s *Service) loadSnapshot() *ruleSnapshot {
	if s == nil {
		return nil
	}
	return s.currentSnapshotMap()[defaultTenantID]
}

func (s *Service) List() []Rule {
	snap := s.loadSnapshot()
	if snap == nil {
		return nil
	}
	out := make([]Rule, 0, len(snap.rows))
	for _, row := range snap.rows {
		out = append(out, row.clone())
	}
	return out
}

func (s *Service) ListViews() []View {
	rows := s.List()
	views := make([]View, 0, len(rows))
	for _, row := range rows {
		views = append(views, row.view())
	}
	return views
}

func (s *Service) Get(source string) (*Rule, bool) {
	source = strings.TrimSpace(source)
	for _, row := range s.List() {
		if row.Source == source {
			return &row, true
		}
	}
	return nil, false
}

func (s *Service) Upsert(ctx context.Context, rule Rule) error {
	if s == nil {
		return fmt.Errorf("failover service is required")
	}
	normalized, err := s.normalizeForUpsert(rule)
	if err != nil {
		return err
	}
	existing, err := s.store.Get(ctx, defaultTenantID, normalized.Source)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("read existing failover rule: %w", err)
	}
	if existing != nil {
		normalized.CreatedAt = existing.CreatedAt
	}
	if err := s.store.Upsert(ctx, defaultTenantID, normalized); err != nil {
		return err
	}
	return s.Refresh(ctx)
}

func (s *Service) Delete(ctx context.Context, source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return fmt.Errorf("primary model is required")
	}
	if s.isManagedSource(source) {
		return ErrManaged
	}
	if err := s.store.Delete(ctx, defaultTenantID, source); err != nil {
		return err
	}
	return s.Refresh(ctx)
}

func (s *Service) ResetDashboardRules(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("failover service is required")
	}
	if err := s.store.DeleteAll(ctx, defaultTenantID); err != nil {
		return err
	}
	return s.Refresh(ctx)
}

func (s *Service) isManagedSource(source string) bool {
	source = strings.TrimSpace(source)
	for _, row := range s.configRows {
		if row.Source == source {
			return true
		}
	}
	return false
}

// StartBackgroundRefresh starts a goroutine that periodically rebuilds the
// per-tenant snapshot map until the returned stop function is called. Until Task
// 8 wires the active-tenant list through (from tenants.Service), each tick
// refreshes the default tenant only; the tenant-list plumbing is deferred there.
func (s *Service) StartBackgroundRefresh(interval time.Duration) func() {
	if interval <= 0 {
		interval = time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshCtx, refreshCancel := context.WithTimeout(ctx, 30*time.Second)
				if err := s.RefreshAll(refreshCtx, []string{defaultTenantID}); err != nil {
					slog.Error("failed to refresh failover mappings", "error", err)
				}
				refreshCancel()
			}
		}
	}()
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// normalizeForUpsert validates and prepares a rule for persistence: it trims
// and validates fields, rejects sources managed by configuration, and marks the
// rule as dashboard-managed.
func (s *Service) normalizeForUpsert(rule Rule) (Rule, error) {
	normalized, err := normalizeRule(rule)
	if err != nil {
		return Rule{}, err
	}
	if s.isManagedSource(normalized.Source) {
		return Rule{}, ErrManaged
	}
	normalized.ManagedSource = ManagedSourceDashboard
	return normalized, nil
}

func normalizeRule(rule Rule) (Rule, error) {
	rule.Source = strings.TrimSpace(rule.Source)
	if rule.Source == "" {
		return Rule{}, fmt.Errorf("primary model is required")
	}
	rule.Targets = normalizeTargets(rule.Targets)
	if rule.Enabled && len(rule.Targets) == 0 {
		return Rule{}, fmt.Errorf("targets must contain at least one model")
	}
	return rule, nil
}

func normalizeTargets(targets []string) []string {
	seen := make(map[string]struct{}, len(targets))
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
	}
	return out
}
