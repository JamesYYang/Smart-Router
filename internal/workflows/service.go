package workflows

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

	"smartrouter/internal/core"
	"smartrouter/internal/guardrails"
)

const (
	ManagedDefaultGlobalName        = "default-global"
	ManagedDefaultGlobalDescription = "Bootstrapped from runtime configuration"

	// defaultTenantID is the platform-default tenant used when the request
	// context carries no tenant ID (core.GetTenantID returns ""), and as the
	// target tenant for the admin/management methods (s.tenantID).
	defaultTenantID = "default"
)

// CompiledWorkflow is the immutable runtime projection cached in the hot-path snapshot.
type CompiledWorkflow struct {
	Version  Version
	Policy   *core.ResolvedWorkflowPolicy
	Pipeline *guardrails.Pipeline
}

// Compiler turns one persisted workflow version into its runtime projection.
type Compiler interface {
	Compile(version Version) (*CompiledWorkflow, error)
}

type snapshot struct {
	global             *CompiledWorkflow
	paths              map[string]*CompiledWorkflow
	providers          map[string]*CompiledWorkflow
	providerPaths      map[string]map[string]*CompiledWorkflow
	providerModels     map[string]map[string]*CompiledWorkflow
	providerModelPaths map[string]map[string]map[string]*CompiledWorkflow
	byVersionID        map[string]*CompiledWorkflow
}

// Service keeps the active workflow set cached in memory, per tenant.
//
// snapshots holds one compiled snapshot per tenant ID (map[tenantID]snapshot).
// The hot path (Match / PipelineForWorkflow) selects the calling tenant's
// snapshot via core.GetTenantID(ctx), falling back to the platform-default
// tenant when ctx carries no tenant ID. The tenantID field identifies the
// platform-default tenant used by the admin/management methods (Create,
// Deactivate, GetView, ListViews, EnsureDefaultGlobal) and by the P4
// ForTenant admin methods — it is not the tenant index for the cache.
type Service struct {
	store     Store
	compiler  Compiler
	tenantID  string       // platform-default tenant (admin/management methods)
	snapshots atomic.Value // map[string]snapshot
	refreshMu sync.Mutex
}

// NewService creates a workflow service backed by storage.
func NewService(store Store, compiler Compiler) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if compiler == nil {
		return nil, fmt.Errorf("compiler is required")
	}

	service := &Service{
		store:    store,
		compiler: compiler,
		tenantID: defaultTenantID,
	}
	service.snapshots.Store(map[string]snapshot{
		defaultTenantID: emptySnapshot(),
	})
	return service, nil
}

// Refresh reloads active workflows for the tenant resolved from ctx (falling
// back to the platform-default tenant) and atomically swaps that tenant's
// in-memory snapshot.
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
		next, err := s.buildSnapshot(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("workflow refresh tenant %s: %w", tenantID, err)
		}
		newMap[tenantID] = next
	}
	s.snapshots.Store(newMap)
	return nil
}

// refreshLocked rebuilds one tenant's snapshot and swaps it into the map.
// Caller must hold refreshMu.
func (s *Service) refreshLocked(ctx context.Context, tenantID string) error {
	next, err := s.buildSnapshot(ctx, tenantID)
	if err != nil {
		return err
	}
	cloned := cloneSnapshotMap(s.snapshotMap())
	cloned[tenantID] = next
	s.snapshots.Store(cloned)
	return nil
}

// buildSnapshot loads and compiles the effective active workflows visible to
// tenantID into a single compiled snapshot. It requires a global workflow.
func (s *Service) buildSnapshot(ctx context.Context, tenantID string) (snapshot, error) {
	versions, err := s.store.ListEffective(ctx, tenantID)
	if err != nil {
		return snapshot{}, fmt.Errorf("list effective workflows: %w", err)
	}

	next := emptySnapshot()

	for _, version := range versions {
		scope, scopeKey, err := normalizeScope(version.Scope)
		if err != nil {
			return snapshot{}, fmt.Errorf("load workflow %q: %w", version.ID, err)
		}
		version.Scope = scope
		version.ScopeKey = scopeKey

		compiled, err := s.compiler.Compile(version)
		if err != nil {
			return snapshot{}, fmt.Errorf("compile workflow %q: %w", version.ID, err)
		}
		if compiled == nil || compiled.Policy == nil {
			return snapshot{}, fmt.Errorf("compile workflow %q: empty compiled workflow", version.ID)
		}

		next.byVersionID[compiled.Version.ID] = compiled

		switch {
		case scope.Provider == "" && scope.UserPath == "":
			if next.global != nil {
				return snapshot{}, fmt.Errorf("duplicate active global workflows: %q and %q", next.global.Version.ID, version.ID)
			}
			next.global = compiled
		case scope.Provider == "" && scope.UserPath != "":
			if existing := next.paths[scope.UserPath]; existing != nil {
				return snapshot{}, fmt.Errorf("duplicate active path workflows for %q: %q and %q", scope.UserPath, existing.Version.ID, version.ID)
			}
			next.paths[scope.UserPath] = compiled
		case scope.Model == "" && scope.UserPath == "":
			if existing := next.providers[scope.Provider]; existing != nil {
				return snapshot{}, fmt.Errorf("duplicate active provider workflows for %q: %q and %q", scope.Provider, existing.Version.ID, version.ID)
			}
			next.providers[scope.Provider] = compiled
		case scope.Model == "" && scope.UserPath != "":
			paths := next.providerPaths[scope.Provider]
			if paths == nil {
				paths = make(map[string]*CompiledWorkflow)
				next.providerPaths[scope.Provider] = paths
			}
			if existing := paths[scope.UserPath]; existing != nil {
				return snapshot{}, fmt.Errorf("duplicate active provider-path workflows for %q/%q: %q and %q", scope.Provider, scope.UserPath, existing.Version.ID, version.ID)
			}
			paths[scope.UserPath] = compiled
		case scope.UserPath == "":
			models := next.providerModels[scope.Provider]
			if models == nil {
				models = make(map[string]*CompiledWorkflow)
				next.providerModels[scope.Provider] = models
			}
			if existing := models[scope.Model]; existing != nil {
				return snapshot{}, fmt.Errorf("duplicate active provider-model workflows for %q/%q: %q and %q", scope.Provider, scope.Model, existing.Version.ID, version.ID)
			}
			models[scope.Model] = compiled
		default:
			providers := next.providerModelPaths[scope.Provider]
			if providers == nil {
				providers = make(map[string]map[string]*CompiledWorkflow)
				next.providerModelPaths[scope.Provider] = providers
			}
			paths := providers[scope.Model]
			if paths == nil {
				paths = make(map[string]*CompiledWorkflow)
				providers[scope.Model] = paths
			}
			if existing := paths[scope.UserPath]; existing != nil {
				return snapshot{}, fmt.Errorf("duplicate active provider-model-path workflows for %q/%q/%q: %q and %q", scope.Provider, scope.Model, scope.UserPath, existing.Version.ID, version.ID)
			}
			paths[scope.UserPath] = compiled
		}
	}

	if next.global == nil {
		return snapshot{}, fmt.Errorf("missing active global workflow")
	}

	return next, nil
}

// EnsureDefaultGlobal seeds or reconciles the managed active global workflow.
func (s *Service) EnsureDefaultGlobal(ctx context.Context, input CreateInput) error {
	input.Managed = true
	normalized, _, workflowHash, err := normalizeCreateInput(input)
	if err != nil {
		return err
	}
	if normalized.Scope.Provider != "" || normalized.Scope.Model != "" || normalized.Scope.UserPath != "" {
		return newValidationError("default workflow must use global scope", nil)
	}

	if !normalized.Activate {
		normalized.Activate = true
	}
	normalized.Managed = true
	previewCompiled, err := s.validateCreateCandidate(normalized, "global", workflowHash)
	if err != nil {
		return err
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	version, err := s.store.EnsureManagedDefaultGlobal(ctx, s.tenantID, normalized, workflowHash)
	if err != nil {
		return fmt.Errorf("ensure default global workflow: %w", err)
	}
	if version == nil {
		if s.snapshot().global == nil {
			if err := s.refreshLocked(ctx, s.tenantID); err != nil {
				return err
			}
		}
		return nil
	}

	s.storeActivatedCompiledLocked(compiledWorkflowForVersion(previewCompiled, *version))
	return nil
}

// Create inserts a new immutable workflow version and refreshes the
// in-memory snapshot so future requests can match it immediately.
func (s *Service) Create(ctx context.Context, input CreateInput) (*Version, error) {
	if s == nil {
		return nil, fmt.Errorf("workflow service is required")
	}

	normalized, scopeKey, workflowHash, err := normalizeCreateInput(input)
	if err != nil {
		return nil, err
	}
	previewCompiled, err := s.validateCreateCandidate(normalized, scopeKey, workflowHash)
	if err != nil {
		return nil, err
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	version, err := s.store.Create(ctx, s.tenantID, normalized)
	if err != nil {
		return nil, fmt.Errorf("create workflow: %w", err)
	}
	if version != nil && version.Active {
		s.storeActivatedCompiledLocked(compiledWorkflowForVersion(previewCompiled, *version))
	}
	return version, nil
}

// Deactivate turns off one active workflow version and refreshes the
// in-memory snapshot so future requests stop matching it immediately.
func (s *Service) Deactivate(ctx context.Context, id string) error {
	if s == nil {
		return fmt.Errorf("workflow service is required")
	}

	version, err := s.store.Get(ctx, s.tenantID, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("load workflow %q: %w", id, err)
	}
	if version == nil {
		return ErrNotFound
	}

	scope, scopeKey, err := normalizeScope(version.Scope)
	if err != nil {
		return fmt.Errorf("load workflow %q: %w", id, err)
	}
	version.Scope = scope
	version.ScopeKey = scopeKey

	if scope.Provider == "" && scope.Model == "" && scope.UserPath == "" {
		return newValidationError("cannot deactivate the global workflow", nil)
	}
	if !version.Active {
		return newValidationError("workflow is already inactive", nil)
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	if err := s.store.Deactivate(ctx, s.tenantID, version.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("deactivate workflow %q: %w", version.ID, err)
	}
	s.storeDeactivatedVersionLocked(*version)
	return nil
}

// GetView returns one workflow version view, including inactive historical versions.
func (s *Service) GetView(ctx context.Context, id string) (View, error) {
	if s == nil {
		return View{}, fmt.Errorf("workflow service is required")
	}

	version, err := s.store.Get(ctx, s.tenantID, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return View{}, err
		}
		return View{}, fmt.Errorf("load workflow %q: %w", id, err)
	}
	if version == nil {
		return View{}, ErrNotFound
	}

	return s.viewForVersion(*version)
}

// ListViews returns the active workflows together with their effective
// runtime features after process-level caps are applied.
func (s *Service) ListViews(ctx context.Context) ([]View, error) {
	if s == nil {
		return []View{}, nil
	}

	versions, err := s.store.ListActive(ctx, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("list active workflows: %w", err)
	}

	views := make([]View, 0, len(versions))
	for _, version := range versions {
		view, err := s.viewForVersion(version)
		if err != nil {
			slog.Warn("workflow view build failed", "version_id", strings.TrimSpace(version.ID), "error", err)
			views = append(views, viewWithError(version, err))
			continue
		}
		views = append(views, view)
	}

	sort.SliceStable(views, func(i, j int) bool {
		left, right := views[i], views[j]
		if leftSpecificity, rightSpecificity := viewScopeSpecificity(left.ScopeType), viewScopeSpecificity(right.ScopeType); leftSpecificity != rightSpecificity {
			return leftSpecificity < rightSpecificity
		}
		if left.ScopeDisplay != right.ScopeDisplay {
			return left.ScopeDisplay < right.ScopeDisplay
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.After(right.CreatedAt)
		}
		return left.ID < right.ID
	})

	return views, nil
}

// Match returns the most-specific compiled workflow policy for one request,
// resolved against the calling tenant's snapshot (core.GetTenantID(ctx),
// falling back to the platform-default tenant).
func (s *Service) Match(ctx context.Context, selector core.WorkflowSelector) (*core.ResolvedWorkflowPolicy, error) {
	compiled, err := s.matchCompiled(ctx, selector)
	if err != nil || compiled == nil {
		return nil, err
	}
	policy := *compiled.Policy
	return &policy, nil
}

// PipelineForContext resolves the active guardrails pipeline for the request context.
func (s *Service) PipelineForContext(ctx context.Context) *guardrails.Pipeline {
	if s == nil || ctx == nil {
		return nil
	}
	workflow := core.GetWorkflow(ctx)
	if workflow == nil {
		return nil
	}
	return s.PipelineForWorkflow(ctx, workflow)
}

// PipelineForWorkflow resolves the active guardrails pipeline for one request
// workflow, looked up in the calling tenant's snapshot (core.GetTenantID(ctx),
// falling back to the platform-default tenant).
func (s *Service) PipelineForWorkflow(ctx context.Context, workflow *core.Workflow) *guardrails.Pipeline {
	if s == nil || workflow == nil || workflow.Policy == nil || !workflow.GuardrailsEnabled() {
		return nil
	}
	versionID := strings.TrimSpace(workflow.Policy.VersionID)
	if versionID == "" {
		return nil
	}
	current := s.snapshotFor(ctx)
	compiled := current.byVersionID[versionID]
	if compiled == nil {
		return nil
	}
	return compiled.Pipeline
}

// StartBackgroundRefresh periodically reloads active workflows until stopped.
func (s *Service) StartBackgroundRefresh(interval time.Duration) func() {
	if interval <= 0 {
		interval = time.Minute
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
				func() {
					refreshCtx, refreshCancel := context.WithTimeout(ctx, 30*time.Second)
					defer refreshCancel()
					if err := s.Refresh(refreshCtx); err != nil {
						slog.Warn("workflow refresh failed", "error", err)
					}
				}()
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

func (s *Service) matchCompiled(ctx context.Context, selector core.WorkflowSelector) (*CompiledWorkflow, error) {
	if s == nil {
		return nil, nil
	}
	selector = core.NewWorkflowSelector(selector.Provider, selector.Model, selector.UserPath)
	current := s.snapshotFor(ctx)
	ancestors := core.UserPathAncestors(selector.UserPath)

	if len(ancestors) > 0 {
		for _, userPath := range ancestors {
			if selector.Provider != "" && selector.Model != "" {
				if models := current.providerModelPaths[selector.Provider]; models != nil {
					if paths := models[selector.Model]; paths != nil {
						if compiled := paths[userPath]; compiled != nil {
							return compiled, nil
						}
					}
				}
			}
			if selector.Provider != "" {
				if paths := current.providerPaths[selector.Provider]; paths != nil {
					if compiled := paths[userPath]; compiled != nil {
						return compiled, nil
					}
				}
			}
			if compiled := current.paths[userPath]; compiled != nil {
				return compiled, nil
			}
		}
	}
	if selector.Provider != "" && selector.Model != "" {
		if models := current.providerModels[selector.Provider]; models != nil {
			if compiled := models[selector.Model]; compiled != nil {
				return compiled, nil
			}
		}
	}
	if selector.Provider != "" {
		if compiled := current.providers[selector.Provider]; compiled != nil {
			return compiled, nil
		}
	}
	if current.global == nil {
		return nil, fmt.Errorf("missing active global workflow")
	}
	return current.global, nil
}

func (s *Service) validateCreateCandidate(input CreateInput, scopeKey, workflowHash string) (*CompiledWorkflow, error) {
	version := Version{
		ID:           "preview",
		Scope:        input.Scope,
		ScopeKey:     scopeKey,
		Version:      1,
		Active:       input.Activate,
		Name:         input.Name,
		Description:  input.Description,
		Payload:      input.Payload,
		WorkflowHash: workflowHash,
		CreatedAt:    time.Unix(0, 0).UTC(),
	}
	compiled, err := s.compiler.Compile(version)
	if err != nil {
		return nil, newValidationError(err.Error(), err)
	}
	if compiled == nil || compiled.Policy == nil {
		return nil, newValidationError("compiled workflow is empty or missing policy", nil)
	}
	return compiled, nil
}

func (s *Service) viewForVersion(version Version) (View, error) {
	scope, scopeKey, err := normalizeScope(version.Scope)
	if err != nil {
		return View{}, fmt.Errorf("load workflow %q: %w", version.ID, err)
	}
	version.Scope = scope
	if strings.TrimSpace(version.ScopeKey) == "" {
		version.ScopeKey = scopeKey
	}

	compiled, err := s.compiler.Compile(version)
	if err != nil {
		return View{}, fmt.Errorf("compile workflow %q: %w", version.ID, err)
	}
	if compiled == nil || compiled.Policy == nil {
		return View{}, fmt.Errorf("compile workflow %q: empty compiled workflow", version.ID)
	}

	view := NewViewFromVersion(version)
	view.ScopeType = scopeType(scope)
	view.ScopeDisplay = scopeDisplay(scope)
	view.EffectiveFeatures = compiled.Policy.Features
	view.GuardrailsHash = compiled.Policy.GuardrailsHash
	return view, nil
}

func viewWithError(version Version, err error) View {
	scope := Scope{
		Provider: strings.TrimSpace(version.Scope.Provider),
		Model:    strings.TrimSpace(version.Scope.Model),
		UserPath: strings.TrimSpace(version.Scope.UserPath),
	}
	version.Scope = scope

	view := NewViewFromVersion(version)
	view.ScopeType = rawScopeType(scope)
	view.ScopeDisplay = rawScopeDisplay(scope)
	view.CompileError = err.Error()
	return view
}

func rawScopeType(scope Scope) string {
	switch {
	case strings.TrimSpace(scope.Provider) == "" && strings.TrimSpace(scope.Model) == "" && strings.TrimSpace(scope.UserPath) == "":
		return "global"
	case strings.TrimSpace(scope.Provider) == "" && strings.TrimSpace(scope.UserPath) != "":
		return "path"
	case strings.TrimSpace(scope.Provider) != "" && strings.TrimSpace(scope.Model) == "":
		if strings.TrimSpace(scope.UserPath) != "" {
			return "provider_path"
		}
		return "provider"
	case strings.TrimSpace(scope.UserPath) != "":
		return "provider_model_path"
	default:
		return "provider_model"
	}
}

func rawScopeDisplay(scope Scope) string {
	provider := strings.TrimSpace(scope.Provider)
	model := strings.TrimSpace(scope.Model)
	userPath := strings.TrimSpace(scope.UserPath)

	switch {
	case provider == "" && model == "" && userPath == "":
		return "global"
	case provider == "" && userPath != "":
		return userPath
	case provider != "" && model == "" && userPath == "":
		return provider
	case provider != "" && model == "" && userPath != "":
		return provider + " @ " + userPath
	case provider == "" && model != "":
		return model
	case userPath != "":
		return provider + "/" + model + " @ " + userPath
	default:
		return provider + "/" + model
	}
}

func scopeType(scope Scope) string {
	switch {
	case strings.TrimSpace(scope.Provider) == "" && strings.TrimSpace(scope.UserPath) == "":
		return "global"
	case strings.TrimSpace(scope.Provider) == "" && strings.TrimSpace(scope.UserPath) != "":
		return "path"
	case strings.TrimSpace(scope.Model) == "" && strings.TrimSpace(scope.UserPath) == "":
		return "provider"
	case strings.TrimSpace(scope.Model) == "" && strings.TrimSpace(scope.UserPath) != "":
		return "provider_path"
	case strings.TrimSpace(scope.UserPath) != "":
		return "provider_model_path"
	default:
		return "provider_model"
	}
}

func scopeDisplay(scope Scope) string {
	switch scopeType(scope) {
	case "global":
		return "global"
	case "path":
		return scope.UserPath
	case "provider_path":
		return scope.Provider + " @ " + scope.UserPath
	case "provider_model_path":
		return scope.Provider + "/" + scope.Model + " @ " + scope.UserPath
	case "provider":
		return scope.Provider
	default:
		return scope.Provider + "/" + scope.Model
	}
}

func viewScopeSpecificity(scopeType string) int {
	switch strings.TrimSpace(scopeType) {
	case "global":
		return 0
	case "provider":
		return 1
	case "provider_model":
		return 2
	case "path":
		return 3
	case "provider_path":
		return 4
	default:
		return 5
	}
}

func emptySnapshot() snapshot {
	return snapshot{
		paths:              map[string]*CompiledWorkflow{},
		providers:          map[string]*CompiledWorkflow{},
		providerPaths:      map[string]map[string]*CompiledWorkflow{},
		providerModels:     map[string]map[string]*CompiledWorkflow{},
		providerModelPaths: map[string]map[string]map[string]*CompiledWorkflow{},
		byVersionID:        map[string]*CompiledWorkflow{},
	}
}

// snapshotMap returns the current per-tenant snapshot map, or an empty map when
// the service is nil or the atomic value has not been seeded.
func (s *Service) snapshotMap() map[string]snapshot {
	if s == nil {
		return map[string]snapshot{}
	}
	if m, ok := s.snapshots.Load().(map[string]snapshot); ok {
		return m
	}
	return map[string]snapshot{}
}

// snapshotFor selects the calling tenant's snapshot via core.GetTenantID(ctx),
// falling back to the platform-default tenant. Returns emptySnapshot when the
// tenant has no entry in the map (not yet refreshed).
func (s *Service) snapshotFor(ctx context.Context) snapshot {
	tenantID := core.GetTenantID(ctx)
	if tenantID == "" {
		tenantID = defaultTenantID
	}
	return s.snapshotForTenant(tenantID)
}

// snapshotForTenant selects the snapshot for one explicit tenant ID.
func (s *Service) snapshotForTenant(tenantID string) snapshot {
	m := s.snapshotMap()
	if snap, ok := m[tenantID]; ok {
		return snap
	}
	return emptySnapshot()
}

// snapshot returns the platform-default tenant's snapshot. Used by the
// admin/management methods (Create, Deactivate, EnsureDefaultGlobal) and the
// store*Locked helpers, all of which operate on the default tenant.
func (s *Service) snapshot() snapshot {
	return s.snapshotForTenant(s.tenantID)
}

func tenantIDFromContext(ctx context.Context) string {
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

func cloneSnapshot(current snapshot) snapshot {
	next := snapshot{
		global:             current.global,
		paths:              make(map[string]*CompiledWorkflow, len(current.paths)),
		providers:          make(map[string]*CompiledWorkflow, len(current.providers)),
		providerPaths:      make(map[string]map[string]*CompiledWorkflow, len(current.providerPaths)),
		providerModels:     make(map[string]map[string]*CompiledWorkflow, len(current.providerModels)),
		providerModelPaths: make(map[string]map[string]map[string]*CompiledWorkflow, len(current.providerModelPaths)),
		byVersionID:        make(map[string]*CompiledWorkflow, len(current.byVersionID)),
	}
	for userPath, compiled := range current.paths {
		next.paths[userPath] = compiled
	}
	for provider, compiled := range current.providers {
		next.providers[provider] = compiled
	}
	for provider, paths := range current.providerPaths {
		copied := make(map[string]*CompiledWorkflow, len(paths))
		for userPath, compiled := range paths {
			copied[userPath] = compiled
		}
		next.providerPaths[provider] = copied
	}
	for provider, models := range current.providerModels {
		copied := make(map[string]*CompiledWorkflow, len(models))
		for model, compiled := range models {
			copied[model] = compiled
		}
		next.providerModels[provider] = copied
	}
	for provider, models := range current.providerModelPaths {
		modelCopy := make(map[string]map[string]*CompiledWorkflow, len(models))
		for model, paths := range models {
			pathCopy := make(map[string]*CompiledWorkflow, len(paths))
			for userPath, compiled := range paths {
				pathCopy[userPath] = compiled
			}
			modelCopy[model] = pathCopy
		}
		next.providerModelPaths[provider] = modelCopy
	}
	for versionID, compiled := range current.byVersionID {
		next.byVersionID[versionID] = compiled
	}
	return next
}

func compiledWorkflowForVersion(compiled *CompiledWorkflow, version Version) *CompiledWorkflow {
	if compiled == nil {
		return nil
	}
	next := &CompiledWorkflow{
		Version:  version,
		Pipeline: compiled.Pipeline,
	}
	if compiled.Policy != nil {
		policy := *compiled.Policy
		policy.VersionID = version.ID
		policy.Version = version.Version
		policy.ScopeProvider = version.Scope.Provider
		policy.ScopeModel = version.Scope.Model
		policy.ScopeUserPath = version.Scope.UserPath
		policy.Name = version.Name
		policy.WorkflowHash = version.WorkflowHash
		next.Policy = &policy
	}
	return next
}

func (s *Service) storeActivatedCompiledLocked(compiled *CompiledWorkflow) {
	if s == nil || compiled == nil {
		return
	}
	clonedMap := cloneSnapshotMap(s.snapshotMap())
	next := cloneSnapshot(s.snapshotForTenant(s.tenantID))
	scope := compiled.Version.Scope

	switch {
	case scope.Provider == "" && scope.UserPath == "":
		if next.global != nil {
			delete(next.byVersionID, next.global.Version.ID)
		}
		next.global = compiled
	case scope.Provider == "" && scope.UserPath != "":
		if existing := next.paths[scope.UserPath]; existing != nil {
			delete(next.byVersionID, existing.Version.ID)
		}
		next.paths[scope.UserPath] = compiled
	case scope.Model == "" && scope.UserPath == "":
		if existing := next.providers[scope.Provider]; existing != nil {
			delete(next.byVersionID, existing.Version.ID)
		}
		next.providers[scope.Provider] = compiled
	case scope.Model == "" && scope.UserPath != "":
		paths := next.providerPaths[scope.Provider]
		if paths == nil {
			paths = make(map[string]*CompiledWorkflow)
			next.providerPaths[scope.Provider] = paths
		}
		if existing := paths[scope.UserPath]; existing != nil {
			delete(next.byVersionID, existing.Version.ID)
		}
		paths[scope.UserPath] = compiled
	case scope.UserPath == "":
		models := next.providerModels[scope.Provider]
		if models == nil {
			models = make(map[string]*CompiledWorkflow)
			next.providerModels[scope.Provider] = models
		}
		if existing := models[scope.Model]; existing != nil {
			delete(next.byVersionID, existing.Version.ID)
		}
		models[scope.Model] = compiled
	default:
		providers := next.providerModelPaths[scope.Provider]
		if providers == nil {
			providers = make(map[string]map[string]*CompiledWorkflow)
			next.providerModelPaths[scope.Provider] = providers
		}
		paths := providers[scope.Model]
		if paths == nil {
			paths = make(map[string]*CompiledWorkflow)
			providers[scope.Model] = paths
		}
		if existing := paths[scope.UserPath]; existing != nil {
			delete(next.byVersionID, existing.Version.ID)
		}
		paths[scope.UserPath] = compiled
	}

	next.byVersionID[compiled.Version.ID] = compiled
	clonedMap[s.tenantID] = next
	s.snapshots.Store(clonedMap)
}

func (s *Service) storeDeactivatedVersionLocked(version Version) {
	if s == nil {
		return
	}
	clonedMap := cloneSnapshotMap(s.snapshotMap())
	next := cloneSnapshot(s.snapshotForTenant(s.tenantID))
	scope := version.Scope

	delete(next.byVersionID, version.ID)

	switch {
	case scope.Provider == "" && scope.UserPath == "":
		next.global = nil
	case scope.Provider == "" && scope.UserPath != "":
		delete(next.paths, scope.UserPath)
	case scope.Model == "" && scope.UserPath == "":
		delete(next.providers, scope.Provider)
	case scope.Model == "" && scope.UserPath != "":
		paths := next.providerPaths[scope.Provider]
		if paths == nil {
			break
		}
		delete(paths, scope.UserPath)
		if len(paths) == 0 {
			delete(next.providerPaths, scope.Provider)
		}
	case scope.UserPath == "":
		models := next.providerModels[scope.Provider]
		if models == nil {
			break
		}
		delete(models, scope.Model)
		if len(models) == 0 {
			delete(next.providerModels, scope.Provider)
		}
	default:
		models := next.providerModelPaths[scope.Provider]
		if models == nil {
			break
		}
		paths := models[scope.Model]
		if paths == nil {
			break
		}
		delete(paths, scope.UserPath)
		if len(paths) == 0 {
			delete(models, scope.Model)
		}
		if len(models) == 0 {
			delete(next.providerModelPaths, scope.Provider)
		}
	}

	clonedMap[s.tenantID] = next
	s.snapshots.Store(clonedMap)
}
