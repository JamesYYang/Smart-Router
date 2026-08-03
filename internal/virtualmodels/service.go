package virtualmodels

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"smartrouter/internal/core"
)

// defaultTenantID is the tenant that owns the shared inference-time virtual
// model cache (the default/single-tenant deployment).
const defaultTenantID = "default"

// Service is the single native engine over the virtual_models store. It serves
// both redirect resolution (alias behavior) and policy authorization (access
// override behavior) from per-tenant, atomically swapped in-memory snapshots.
type Service struct {
	store          Store
	catalog        Catalog
	defaultEnabled bool
	tenantID       string // platform-default tenant the legacy admin CRUD targets

	// configModels are virtual models supplied declaratively (config.yaml / env).
	// They are merged over the store rows on every refresh, override store rows of
	// the same source, and are read-only to the admin API.
	configModels []VirtualModel

	snapshots atomic.Value // map[string]snapshot, keyed by tenant ID
	refreshMu sync.Mutex
}

// NewService creates a virtual models service backed by the store and catalog.
// defaultEnabled is the process-wide model availability default consulted when
// no policy matches.
func NewService(store Store, catalog Catalog, defaultEnabled bool) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if catalog == nil {
		return nil, fmt.Errorf("catalog is required")
	}
	service := &Service{
		store:          store,
		catalog:        catalog,
		defaultEnabled: defaultEnabled,
		tenantID:       "default",
	}
	service.snapshots.Store(map[string]snapshot{"default": emptySnapshot(defaultEnabled)})
	return service, nil
}

// snapshotFor returns the immutable snapshot for the tenant carried in ctx
// (falling back to the default tenant when ctx carries no tenant ID), or an
// empty snapshot when the tenant has no cached rows yet. The returned snapshot
// is valid for the duration of the call: map swaps publish whole new maps and
// never mutate existing snapshots, so no lock is needed on the read path.
func (s *Service) snapshotFor(ctx context.Context) snapshot {
	if s == nil {
		return emptySnapshot(true)
	}
	return s.snapshotForTenant(core.GetTenantID(ctx))
}

func (s *Service) snapshotForTenant(tenantID string) snapshot {
	if tenantID == "" {
		tenantID = "default"
	}
	if snap, ok := s.currentSnapshotMap()[tenantID]; ok {
		return snap
	}
	return emptySnapshot(s.defaultEnabled)
}

// loadSnapshot returns the default tenant's snapshot for the legacy
// admin/dashboard methods (List/Get/ListViews/ValidateManagedConfig) that
// predate per-tenant caching.
func (s *Service) loadSnapshot() snapshot {
	if s == nil {
		return emptySnapshot(true)
	}
	if snap, ok := s.currentSnapshotMap()["default"]; ok {
		return snap
	}
	return emptySnapshot(s.defaultEnabled)
}

func (s *Service) currentSnapshotMap() map[string]snapshot {
	m, _ := s.snapshots.Load().(map[string]snapshot)
	return m
}

func cloneSnapshotMap(m map[string]snapshot) map[string]snapshot {
	cloned := make(map[string]snapshot, len(m))
	for tenantID, snap := range m {
		cloned[tenantID] = snap
	}
	return cloned
}

// Refresh reloads the virtual models for the tenant carried in ctx (falling
// back to the default tenant when ctx carries no tenant ID) from storage and
// atomically swaps just that tenant's snapshot into the per-tenant map, so
// concurrent refreshes of other tenants are not lost. Startup and background
// ticks use RefreshAll instead.
func (s *Service) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	tenantID := core.GetTenantID(ctx)
	if tenantID == "" {
		tenantID = "default"
	}
	return s.refreshLocked(ctx, tenantID)
}

func (s *Service) refreshLocked(ctx context.Context, tenantID string) error {
	rows, err := s.store.ListEffective(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("list virtual models for tenant %s: %w", tenantID, err)
	}
	next, err := buildSnapshot(s.mergeConfigModels(rows), s.defaultEnabled)
	if err != nil {
		return err
	}
	// Carry the tenant's load-balancing position across snapshot swaps so
	// round-robin state survives periodic reloads.
	if prev, ok := s.currentSnapshotMap()[tenantID]; ok && prev.balancer != nil {
		next.balancer = prev.balancer
		next.balancer.prune(next.redirects)
	}
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
		tenantIDs = []string{"default"}
	}
	newMap := make(map[string]snapshot, len(tenantIDs))
	for _, tid := range tenantIDs {
		rows, err := s.store.ListEffective(ctx, tid)
		if err != nil {
			return fmt.Errorf("virtual models refresh tenant %s: %w", tid, err)
		}
		next, err := buildSnapshot(s.mergeConfigModels(rows), s.defaultEnabled)
		if err != nil {
			return fmt.Errorf("virtual models refresh tenant %s: %w", tid, err)
		}
		// Carry each tenant's load-balancing position across full rebuilds.
		if prev, ok := s.currentSnapshotMap()[tid]; ok && prev.balancer != nil {
			next.balancer = prev.balancer
			next.balancer.prune(next.redirects)
		}
		newMap[tid] = next
	}
	s.snapshots.Store(newMap)
	return nil
}

// SetConfigModels installs the declarative (config.yaml / VIRTUAL_MODELS) virtual
// models that override store rows of the same source. Call it before the first
// Refresh, then ValidateManagedConfig to reject invalid declarations at startup.
func (s *Service) SetConfigModels(models []VirtualModel) {
	cloned := make([]VirtualModel, 0, len(models))
	for _, model := range models {
		model.Managed = true
		cloned = append(cloned, model.clone())
	}
	s.configModels = cloned
}

// mergeConfigModels overlays the config-managed rows onto the store rows. A
// managed row replaces a store row of the same source, keeping config the source
// of truth for the entries it defines.
func (s *Service) mergeConfigModels(stored []VirtualModel) []VirtualModel {
	if len(s.configModels) == 0 {
		return stored
	}
	merged := make([]VirtualModel, 0, len(stored)+len(s.configModels))
	for _, row := range stored {
		if s.isManagedSource(row.Source) {
			continue
		}
		merged = append(merged, row)
	}
	return append(merged, s.configModels...)
}

// isManagedSource reports whether source is owned by a declarative config row.
func (s *Service) isManagedSource(source string) bool {
	source = strings.TrimSpace(source)
	for _, model := range s.configModels {
		if strings.TrimSpace(model.Source) == source {
			return true
		}
	}
	return false
}

// ValidateManagedConfig checks that every declarative config redirect satisfies
// the STRUCTURAL redirect invariants (valid selector, no self- or cross-redirect
// target), so a malformed IaC entry fails startup loudly. Call it once after the
// initial Refresh.
//
// It deliberately does NOT require targets to be catalog-supported: the provider
// model catalog loads asynchronously and may still be warming when this runs, and
// an unavailable target is skipped at resolve time like any other redirect target
// (the background ticker also skips this gate so a transient provider-catalog gap
// cannot freeze the snapshot). Gating startup on availability would abort an
// otherwise-valid declaration on a cold cache or a momentarily-unreachable
// provider — availability is runtime state, not a property of the declaration.
func (s *Service) ValidateManagedConfig() error {
	current := s.loadSnapshot()
	for _, vm := range current.bySource {
		if !vm.Managed || !vm.IsRedirect() {
			continue
		}
		if err := validateRedirectStructure(current, vm); err != nil {
			return fmt.Errorf("load virtual model %q: %w", vm.Source, err)
		}
	}
	return nil
}

// StartBackgroundRefresh periodically reloads virtual models until stopped.
// Each tick asks getTenantIDs for the complete active-tenant list and refreshes
// every one of them via RefreshAll (a full map swap, so the list must always
// include the platform-default tenant). A nil getter, a getter error, or an
// empty list falls back to the default tenant only.
func (s *Service) StartBackgroundRefresh(interval time.Duration, getTenantIDs func(context.Context) ([]string, error)) func() {
	if interval <= 0 {
		interval = time.Hour
	}
	if getTenantIDs == nil {
		getTenantIDs = func(context.Context) ([]string, error) { return []string{defaultTenantID}, nil }
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
				tenantIDs, err := getTenantIDs(refreshCtx)
				if err != nil || len(tenantIDs) == 0 {
					tenantIDs = []string{defaultTenantID}
				}
				if err := s.RefreshAll(refreshCtx, tenantIDs); err != nil {
					slog.Error("failed to refresh virtual models", "error", err)
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

// List returns all cached virtual models sorted by source.
func (s *Service) List() []VirtualModel {
	return s.loadSnapshot().rows()
}

// Get returns one cached virtual model by source.
func (s *Service) Get(source string) (*VirtualModel, bool) {
	if vm, _, ok := s.loadSnapshot().lookupCanonicalSource(source); ok {
		clone := vm.clone()
		return &clone, true
	}
	return nil, false
}

// ListViews returns all virtual models (redirects and policies) for the admin UI.
func (s *Service) ListViews() []View {
	rows := s.List()
	views := make([]View, 0, len(rows))
	for _, vm := range rows {
		view := View{
			Source:       vm.Source,
			Kind:         vm.Kind(),
			Targets:      vm.Targets,
			Strategy:     vm.Strategy,
			ProviderName: vm.ProviderName,
			Model:        vm.Model,
			UserPaths:    vm.UserPaths,
			Description:  vm.Description,
			Enabled:      vm.Enabled,
			Managed:      vm.Managed,
			CreatedAt:    vm.CreatedAt,
			UpdatedAt:    vm.UpdatedAt,
		}
		if vm.IsRedirect() {
			view.ResolvedModel, view.ProviderType, view.Valid = s.redirectViewResolution(vm)
		} else {
			view.ScopeKind = string(scopeKindFor(vm.Source, vm.ProviderName, vm.Model))
		}
		views = append(views, view)
	}
	return views
}

// redirectViewResolution summarizes a redirect for the admin view: a
// representative resolved model (the first available target, else the
// first declared one), its provider type, and whether any target is available.
func (s *Service) redirectViewResolution(vm VirtualModel) (resolved, providerType string, valid bool) {
	for _, target := range vm.Targets {
		selector, err := target.selector()
		if err != nil {
			continue
		}
		qualified := selector.QualifiedModel()
		if s.catalog.ModelAvailable(qualified) {
			return qualified, strings.TrimSpace(s.catalog.GetProviderType(qualified)), true
		}
		if resolved == "" {
			resolved = qualified
			providerType = strings.TrimSpace(s.catalog.GetProviderType(qualified))
		}
	}
	return resolved, providerType, valid
}

// Upsert validates and stores one virtual model, then refreshes the in-memory
// snapshot with rollback on refresh failure.
func (s *Service) Upsert(ctx context.Context, vm VirtualModel) error {
	if s == nil {
		return fmt.Errorf("virtual models service is required")
	}

	normalized, err := s.normalizeForUpsert(vm)
	if err != nil {
		return err
	}
	if s.isManagedSource(normalized.Source) || s.isManagedSource(vm.Source) {
		return managedSourceError(normalized.Source)
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	current := s.loadSnapshot()
	if err := s.ensureSourceKind(current, normalized.Source, normalized.IsRedirect()); err != nil {
		return err
	}
	if err := s.validateRedirectTarget(current, normalized); err != nil {
		return err
	}
	if _, err := buildSnapshot(upsertRow(current.rows(), normalized), s.defaultEnabled); err != nil {
		return fmt.Errorf("validate virtual models: %w", err)
	}

	previous, existed := current.bySource[normalized.Source]
	if err := s.store.Upsert(ctx, s.tenantID, normalized); err != nil {
		return fmt.Errorf("upsert virtual model: %w", err)
	}
	return s.commitRefresh(ctx, map[string]*VirtualModel{
		normalized.Source: priorRow(previous, existed),
	})
}

// Rename moves an existing virtual model to a new source: it stores the row
// under the new source and removes the old one, validating and refreshing like
// Upsert with rollback on failure. A no-op rename (old == new after
// normalization) delegates to Upsert. The new source must be free — renaming
// onto an existing row is rejected rather than silently overwriting it, since
// source is the primary key on every store backend.
func (s *Service) Rename(ctx context.Context, oldSource string, vm VirtualModel) error {
	if s == nil {
		return fmt.Errorf("virtual models service is required")
	}
	oldSource = strings.TrimSpace(oldSource)
	if oldSource == "" {
		return newValidationError("source is required", nil)
	}

	normalized, err := s.normalizeForUpsert(vm)
	if err != nil {
		return err
	}
	if normalized.Source == oldSource {
		// Not actually a rename; fall back to a plain update under the same key.
		return s.Upsert(ctx, vm)
	}
	if s.isManagedSource(oldSource) || s.isManagedSource(normalized.Source) || s.isManagedSource(vm.Source) {
		return managedSourceError(normalized.Source)
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	current := s.loadSnapshot()
	previous, oldExisted := current.bySource[oldSource]
	if !oldExisted {
		return ErrNotFound
	}
	if _, taken := current.bySource[normalized.Source]; taken {
		return newValidationError(fmt.Sprintf("virtual model %q already exists; choose a different source", normalized.Source), nil)
	}
	if err := s.validateRedirectTarget(current, normalized); err != nil {
		return err
	}
	rows := upsertRow(removeRow(current.rows(), oldSource), normalized)
	if _, err := buildSnapshot(rows, s.defaultEnabled); err != nil {
		return fmt.Errorf("validate virtual models: %w", err)
	}

	if err := s.store.Upsert(ctx, s.tenantID, normalized); err != nil {
		return fmt.Errorf("upsert virtual model: %w", err)
	}
	// Restoring the new source means deleting it (it did not exist before); the
	// old source is restored to its prior row.
	prior := map[string]*VirtualModel{
		normalized.Source: nil,
		oldSource:         &previous,
	}
	if err := s.store.Delete(ctx, s.tenantID, oldSource); err != nil {
		// The new row is in but the old one survives; undo so the rename leaves
		// no duplicate behind.
		rollbackCtx, cancel := rollbackContext()
		defer cancel()
		if rollbackErr := s.restore(rollbackCtx, prior); rollbackErr != nil {
			return fmt.Errorf("delete old virtual model: %w (rollback failed: %v)", err, rollbackErr)
		}
		return fmt.Errorf("delete old virtual model: %w", err)
	}
	return s.commitRefresh(ctx, prior)
}

// Delete removes one virtual model and refreshes the in-memory snapshot.
func (s *Service) Delete(ctx context.Context, source string) error {
	if s == nil {
		return fmt.Errorf("virtual models service is required")
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return newValidationError("source is required", nil)
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	current := s.loadSnapshot()
	previous, canonical, existed := current.lookupCanonicalSource(source)
	if !existed {
		return ErrNotFound
	}
	if previous.Managed || s.isManagedSource(canonical) {
		return managedSourceError(canonical)
	}
	source = canonical

	if err := s.store.Delete(ctx, s.tenantID, source); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("delete virtual model: %w", err)
	}
	return s.commitRefresh(ctx, map[string]*VirtualModel{source: &previous})
}

func (s *Service) normalizeForUpsert(vm VirtualModel) (VirtualModel, error) {
	if vm.IsRedirect() {
		normalized, _, err := normalizeRedirect(vm)
		return normalized, err
	}
	return normalizePolicyInput(s.catalog, vm)
}

// ensureSourceKind rejects an upsert that would clobber an existing row of the
// other kind. Source is a single namespace.
func (s *Service) ensureSourceKind(current snapshot, source string, wantRedirect bool) error {
	existing, ok := current.bySource[source]
	if !ok {
		return nil
	}
	if existing.IsRedirect() == wantRedirect {
		return nil
	}
	return crossKindError(source, wantRedirect)
}

// validateRedirectTarget enforces redirect rules for an admin write: the
// structural invariants plus a catalog-availability check. The admin API runs
// against a warm catalog, so a target it cannot serve is a caller mistake worth
// rejecting up front. Startup config validation uses validateRedirectStructure
// alone, because the catalog may not be warm yet (see ValidateManagedConfig).
func (s *Service) validateRedirectTarget(current snapshot, vm VirtualModel) error {
	if err := validateRedirectStructure(current, vm); err != nil {
		return err
	}
	if missing, ok := s.firstUnsupportedTarget(vm); ok {
		return newValidationError("target model not found: "+missing, nil)
	}
	return nil
}

// validateRedirectStructure enforces the catalog-INDEPENDENT redirect invariants:
// each target must parse, a redirect cannot target itself, and it cannot target
// another redirect's source. These are pure properties of the declaration and the
// redirect graph, so they hold whether or not the provider catalog is warm —
// making them safe to enforce at startup, before async model loading completes.
func validateRedirectStructure(current snapshot, vm VirtualModel) error {
	if !vm.IsRedirect() {
		return nil
	}
	for _, target := range vm.Targets {
		selector, err := target.selector()
		if err != nil {
			return newValidationError("invalid target selector: "+err.Error(), err)
		}
		qualified := selector.QualifiedModel()
		if vm.Source == qualified {
			return newValidationError(fmt.Sprintf("virtual model %q cannot target itself", vm.Source), nil)
		}
		if existing, ok := current.redirects[qualified]; ok && existing.vm.Source != vm.Source {
			return newValidationError(fmt.Sprintf("target %q refers to another virtual model", qualified), nil)
		}
	}
	return nil
}

// firstUnsupportedTarget reports the first target the catalog cannot currently
// serve, if any. Availability is transient — it depends on async model loading
// and provider health — and is already handled at resolve time by skipping
// unavailable targets, so it gates the admin write path only, never startup.
func (s *Service) firstUnsupportedTarget(vm VirtualModel) (string, bool) {
	for _, target := range vm.Targets {
		selector, err := target.selector()
		if err != nil {
			continue // selector parse errors are reported by validateRedirectStructure
		}
		if qualified := selector.QualifiedModel(); !s.catalog.Supports(qualified) {
			return qualified, true
		}
	}
	return "", false
}

// managedSourceError is returned when the admin API tries to write a virtual
// model that is owned declaratively by config.yaml or the VIRTUAL_MODELS env var.
func managedSourceError(source string) error {
	return newValidationError(fmt.Sprintf(
		"virtual model %q is managed by config.yaml or VIRTUAL_MODELS and cannot be changed from the admin API; edit your configuration instead",
		source), nil)
}

func upsertRow(rows []VirtualModel, next VirtualModel) []VirtualModel {
	for i := range rows {
		if rows[i].Source == next.Source {
			rows[i] = next.clone()
			return rows
		}
	}
	return append(rows, next.clone())
}

func removeRow(rows []VirtualModel, source string) []VirtualModel {
	out := make([]VirtualModel, 0, len(rows))
	for _, row := range rows {
		if row.Source != source {
			out = append(out, row)
		}
	}
	return out
}

func rollbackContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// commitRefresh refreshes the snapshot after a store write succeeds and restores
// the touched rows if the refresh fails, so a failed refresh never leaves the
// store ahead of the in-memory snapshot. prior maps each touched source to its
// state before the write — a nil row means the source did not exist and is
// deleted on rollback.
func (s *Service) commitRefresh(ctx context.Context, prior map[string]*VirtualModel) error {
	// The legacy admin CRUD writes to the platform-default tenant (s.tenantID),
	// so the refresh after a successful write must rebuild that tenant's snapshot
	// regardless of any tenant carried in ctx.
	if err := s.refreshLocked(ctx, s.tenantID); err != nil {
		rollbackCtx, cancel := rollbackContext()
		defer cancel()
		if rollbackErr := s.restore(rollbackCtx, prior); rollbackErr != nil {
			return fmt.Errorf("refresh virtual models: %w (rollback failed: %v)", err, rollbackErr)
		}
		return fmt.Errorf("refresh virtual models: %w", err)
	}
	return nil
}

// restore returns each source in prior to its captured state: re-upserting a row
// that existed, or deleting one that did not (nil). It is best-effort — every
// entry is attempted and the errors are joined, so one failure never leaves the
// other touched rows unrepaired.
func (s *Service) restore(ctx context.Context, prior map[string]*VirtualModel) error {
	var restoreErr error
	for source, row := range prior {
		var err error
		if row == nil {
			// The source did not exist before; an already-absent row is the
			// intended end state, not a rollback failure.
			if err = s.store.Delete(ctx, s.tenantID, source); errors.Is(err, ErrNotFound) {
				err = nil
			}
		} else {
			err = s.store.Upsert(ctx, s.tenantID, *row)
		}
		if err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore virtual model %q: %w", source, err))
		}
	}
	return restoreErr
}

// priorRow captures a row's pre-write state for restore: the row itself when it
// existed, or nil when it did not.
func priorRow(row VirtualModel, existed bool) *VirtualModel {
	if !existed {
		return nil
	}
	return &row
}

// ResolveUpsertEnabled returns the enabled flag an upsert should persist when
// the request may omit it: the requested value when present; otherwise the
// stored value for source (or, on a rename, for oldSource, since the new
// source does not exist yet); defaulting to true for new rows.
func (s *Service) ResolveUpsertEnabled(source, oldSource string, requested *bool) bool {
	if requested != nil {
		return *requested
	}
	if existing, ok := s.Get(source); ok && existing != nil {
		return existing.Enabled
	}
	if old := strings.TrimSpace(oldSource); old != "" {
		if existing, ok := s.Get(old); ok && existing != nil {
			return existing.Enabled
		}
	}
	return true
}

// Compile-time check that *Service satisfies the resolver, user-path resolver,
// refresh-target, exposed-model lister, and authorizer seams its consumers
// (gateway, server, batch) depend on, so a signature drift fails to compile here.
var _ interface {
	ResolveModel(context.Context, core.RequestedModelSelector) (core.ModelSelector, bool, error)
	ResolveModelForUserPath(context.Context, core.RequestedModelSelector) (core.ModelSelector, bool, error)
	ResolveRefreshTarget(context.Context, core.RequestedModelSelector) (core.ModelSelector, bool, error)
	ExposedModels(context.Context) []core.Model
	ExposedModelsFiltered(context.Context, func(core.ModelSelector) bool) []core.Model
	ExposedModelsForUserPath(context.Context, string, func(core.ModelSelector) bool) []core.Model
	ValidateModelAccess(context.Context, core.ModelSelector) error
	AllowsModel(context.Context, core.ModelSelector) bool
	FilterPublicModels(context.Context, []core.Model) []core.Model
} = (*Service)(nil)
