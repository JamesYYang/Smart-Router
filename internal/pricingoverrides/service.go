package pricingoverrides

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"smartrouter/internal/core"
	"smartrouter/internal/modelselectors"
	"smartrouter/internal/usage"
)

const defaultTenantID = "default"

// Service keeps pricing overrides cached in memory and resolves effective pricing.
//
// snapshots holds one compiled snapshot per tenant ID (map[tenantID]snapshot).
// The hot path (ResolvePricing) selects the calling tenant's snapshot via
// core.GetTenantID(ctx), falling back to the platform-default tenant when ctx
// carries no tenant ID. The tenantID field identifies the platform-default
// tenant used by the admin/management methods (Upsert, Delete, List, Get) and
// by the P4 ForTenant admin methods — it is not the tenant index for the cache.
type Service struct {
	store     Store
	catalog   Catalog
	base      usage.PricingResolver
	tenantID  string       // platform-default tenant (admin/management methods)
	snapshots atomic.Value // map[string]snapshot
	refreshMu sync.Mutex
}

const refreshTimeout = 30 * time.Second

// NewService creates a pricing override service backed by storage.
func NewService(store Store, catalog Catalog, base usage.PricingResolver) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if catalog == nil {
		return nil, fmt.Errorf("catalog is required")
	}

	service := &Service{
		store:    store,
		catalog:  catalog,
		base:     base,
		tenantID: defaultTenantID,
	}
	service.snapshots.Store(map[string]snapshot{
		defaultTenantID: emptySnapshot(),
	})
	return service, nil
}

// Refresh reloads overrides for the tenant resolved from ctx (falling back to
// the platform-default tenant) and atomically swaps that tenant's snapshot.
func (s *Service) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.refreshLocked(ctx, tenantIDFromContext(ctx))
}

// RefreshAll rebuilds the full per-tenant snapshot map from storage, one
// snapshot per tenantID. Used for startup seeding and background refresh.
// A nil/empty tenantIDs list refreshes only the platform-default tenant.
func (s *Service) RefreshAll(ctx context.Context, tenantIDs []string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if len(tenantIDs) == 0 {
		tenantIDs = []string{defaultTenantID}
	}
	newMap := make(map[string]snapshot, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		if tenantID == "" {
			tenantID = defaultTenantID
		}
		next, err := s.buildSnapshotForTenant(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("pricing override refresh tenant %s: %w", tenantID, err)
		}
		newMap[tenantID] = next
	}
	s.snapshots.Store(newMap)
	return nil
}

// refreshLocked reloads one tenant's overrides and swaps its snapshot into the
// per-tenant map. Caller must hold refreshMu.
func (s *Service) refreshLocked(ctx context.Context, tenantID string) error {
	next, err := s.buildSnapshotForTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	cloned := cloneSnapshotMap(s.snapshotMap())
	cloned[tenantID] = next
	s.snapshots.Store(cloned)
	return nil
}

func (s *Service) buildSnapshotForTenant(ctx context.Context, tenantID string) (snapshot, error) {
	overrides, err := s.store.ListEffective(ctx, tenantID)
	if err != nil {
		return snapshot{}, fmt.Errorf("list model pricing overrides: %w", err)
	}
	return s.buildSnapshot(overrides)
}

// snapshotFor selects the calling tenant's snapshot via core.GetTenantID(ctx),
// falling back to the platform-default tenant. Returns emptySnapshot when the
// tenant has no entry in the map (not yet refreshed).
func (s *Service) snapshotFor(ctx context.Context) snapshot {
	if s == nil {
		return emptySnapshot()
	}
	return s.snapshotForTenant(tenantIDFromContext(ctx))
}

// snapshotForTenant selects the snapshot for one explicit tenant ID.
func (s *Service) snapshotForTenant(tenantID string) snapshot {
	if snap, ok := s.snapshotMap()[tenantID]; ok {
		return snap
	}
	return emptySnapshot()
}

// snapshot returns the platform-default tenant's snapshot. Used by the
// admin/management methods (List, Get, Upsert, Delete) which operate on the
// default tenant.
func (s *Service) snapshot() snapshot {
	if s == nil {
		return emptySnapshot()
	}
	return s.snapshotForTenant(s.tenantID)
}

// snapshotMap returns the current per-tenant snapshot map, or an empty map when
// the atomic value has not been seeded.
func (s *Service) snapshotMap() map[string]snapshot {
	if s == nil {
		return map[string]snapshot{}
	}
	if m, ok := s.snapshots.Load().(map[string]snapshot); ok {
		return m
	}
	return map[string]snapshot{}
}

func tenantIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return defaultTenantID
	}
	tenantID := core.GetTenantID(ctx)
	if tenantID == "" {
		return defaultTenantID
	}
	return tenantID
}

func cloneSnapshotMap(current map[string]snapshot) map[string]snapshot {
	next := make(map[string]snapshot, len(current))
	for tenantID, snap := range current {
		next[tenantID] = snap
	}
	return next
}

// List returns all cached overrides sorted by selector.
func (s *Service) List() []Override {
	snap := s.snapshot()
	result := make([]Override, 0, len(snap.order))
	for _, selector := range snap.order {
		result = append(result, overrideClone(snap.bySelector[selector]))
	}
	return result
}

// ListViews returns all cached overrides with scope metadata.
func (s *Service) ListViews() []View {
	overrides := s.List()
	result := make([]View, 0, len(overrides))
	for _, override := range overrides {
		result = append(result, viewForOverride(override))
	}
	return result
}

// GetView returns one cached override with scope metadata by normalized selector.
func (s *Service) GetView(selector string) (*View, bool) {
	override, ok := s.Get(selector)
	if !ok || override == nil {
		return nil, false
	}
	view := viewForOverride(*override)
	return &view, true
}

func viewForOverride(override Override) View {
	return View{
		Override:  override,
		ScopeKind: override.ScopeKind(),
	}
}

// Get returns one cached override by normalized selector.
func (s *Service) Get(selector string) (*Override, bool) {
	parts, err := modelselectors.NormalizeInput(s.catalog, selector)
	if err != nil {
		return nil, false
	}
	override, ok := s.snapshot().bySelector[parts.Selector]
	if !ok {
		return nil, false
	}
	override = overrideClone(override)
	return &override, true
}

// Upsert validates and stores one override, then refreshes the in-memory snapshot.
func (s *Service) Upsert(ctx context.Context, override Override) error {
	if s == nil {
		return fmt.Errorf("model pricing override service is required")
	}

	normalized, err := normalizeOverrideInput(s.catalog, override)
	if err != nil {
		return err
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	current := s.snapshot()
	if _, err := s.buildSnapshot(upsertOverride(snapshotOverrides(current), normalized)); err != nil {
		return fmt.Errorf("validate model pricing overrides: %w", err)
	}
	previous, existed := current.bySelector[normalized.Selector]
	if err := s.store.Upsert(ctx, s.tenantID, normalized); err != nil {
		return fmt.Errorf("upsert model pricing override: %w", err)
	}
	if err := s.refreshLocked(ctx, s.tenantID); err != nil {
		rollbackCtx, cancel := rollbackContext()
		defer cancel()

		var rollbackErr error
		if existed {
			rollbackErr = s.store.Upsert(rollbackCtx, s.tenantID, previous)
		} else {
			rollbackErr = s.store.Delete(rollbackCtx, s.tenantID, normalized.Selector)
		}
		if rollbackErr != nil {
			return s.reconcileSnapshotAfterRollbackFailureLocked("upsert", err, rollbackErr)
		}
		return fmt.Errorf("refresh model pricing overrides: %w", err)
	}
	return nil
}

// Delete removes one override and refreshes the in-memory snapshot.
func (s *Service) Delete(ctx context.Context, selector string) error {
	if s == nil {
		return fmt.Errorf("model pricing override service is required")
	}

	parts, err := modelselectors.NormalizeInput(s.catalog, selector)
	if err != nil {
		return err
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	current := s.snapshot()
	if _, err := s.buildSnapshot(deleteOverride(snapshotOverrides(current), parts.Selector)); err != nil {
		return fmt.Errorf("validate model pricing overrides: %w", err)
	}
	previous, existed := current.bySelector[parts.Selector]
	if err := s.store.Delete(ctx, s.tenantID, parts.Selector); err != nil {
		return fmt.Errorf("delete model pricing override: %w", err)
	}
	if err := s.refreshLocked(ctx, s.tenantID); err != nil {
		// If the selector was absent from the snapshot, there is no known previous
		// value to restore, so we intentionally skip rollback.
		if !existed {
			return fmt.Errorf("refresh model pricing overrides: %w", err)
		}
		rollbackCtx, cancel := rollbackContext()
		defer cancel()
		if rollbackErr := s.store.Upsert(rollbackCtx, s.tenantID, previous); rollbackErr != nil {
			return s.reconcileSnapshotAfterRollbackFailureLocked("delete", err, rollbackErr)
		}
		return fmt.Errorf("refresh model pricing overrides: %w", err)
	}
	return nil
}

// StartBackgroundRefresh periodically reloads pricing overrides from storage until stopped.
// Each s.Refresh call is capped by refreshTimeout, and shorter intervals are clamped to refreshTimeout.
func (s *Service) StartBackgroundRefresh(interval time.Duration) func() {
	interval = normalizedRefreshInterval(interval)

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
				refreshCtx, refreshCancel := context.WithTimeout(ctx, refreshTimeout)
				if err := s.Refresh(refreshCtx); err != nil {
					slog.Error("failed to refresh model pricing overrides", "error", err)
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

func normalizedRefreshInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return time.Minute
	}
	if interval < refreshTimeout {
		return refreshTimeout
	}
	return interval
}

// rollbackContext deliberately uses context.Background with context.WithTimeout
// so cleanup can continue briefly even when the caller's request context is canceled.
func rollbackContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), refreshTimeout)
}

func (s *Service) reconcileSnapshotAfterRollbackFailureLocked(operation string, refreshErr, rollbackErr error) error {
	reconcileCtx, cancel := rollbackContext()
	defer cancel()

	if reconcileErr := s.refreshLocked(reconcileCtx, s.tenantID); reconcileErr != nil {
		slog.Warn(
			"model pricing override snapshot may be stale after failed rollback",
			"operation", operation,
			"refresh_error", refreshErr,
			"rollback_error", rollbackErr,
			"reconcile_error", reconcileErr,
		)
		return fmt.Errorf("refresh model pricing overrides: %w (rollback failed: %v; snapshot refresh failed: %v)", refreshErr, rollbackErr, reconcileErr)
	}

	slog.Warn(
		"model pricing override rollback failed; refreshed snapshot from persisted state",
		"operation", operation,
		"refresh_error", refreshErr,
		"rollback_error", rollbackErr,
	)
	return fmt.Errorf("refresh model pricing overrides: %w (rollback failed: %v)", refreshErr, rollbackErr)
}
