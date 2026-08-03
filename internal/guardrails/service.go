package guardrails

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"smartrouter/internal/core"
)

// defaultTenantID is the tenant that owns the shared inference-time guardrail
// cache (the default/single-tenant deployment).
const defaultTenantID = "default"

type serviceSnapshot struct {
	definitions map[string]Definition
	order       []string
	registry    *Registry
}

// emptySnapshot is the shared immutable snapshot served to tenants that have no
// cached guardrails yet. It is never mutated after construction, so it can be
// shared across all read paths.
var emptySnapshot = &serviceSnapshot{
	definitions: map[string]Definition{},
	order:       []string{},
	registry:    NewRegistry(),
}

// Service keeps reusable guardrails cached in memory and refreshes them from storage.
//
// snapshots holds one snapshot per tenant ID (map[tenantID]*serviceSnapshot).
// The hot path (BuildPipeline) selects the calling tenant's snapshot via
// core.GetTenantID(ctx), falling back to the platform-default tenant when ctx
// carries no tenant ID. The tenantID field identifies the platform-default
// tenant used by the legacy admin/dashboard and P4 ForTenant management methods
// — it is not the cache index.
type Service struct {
	store    Store
	executor ChatCompletionExecutor

	tenantID string

	refreshMu sync.Mutex
	mu        sync.Mutex
	snapshots map[string]*serviceSnapshot
}

// NewService creates a guardrail service backed by the provided store.
func NewService(store Store, executors ...ChatCompletionExecutor) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if len(executors) > 1 {
		return nil, fmt.Errorf("only one ChatCompletionExecutor is supported")
	}
	var executor ChatCompletionExecutor
	if len(executors) > 0 {
		executor = executors[0]
	}
	return &Service{
		store:    store,
		executor: executor,
		tenantID: defaultTenantID,
		snapshots: map[string]*serviceSnapshot{
			defaultTenantID: {
				definitions: map[string]Definition{},
				order:       []string{},
				registry:    NewRegistry(),
			},
		},
	}, nil
}

// Refresh reloads guardrails from storage for the tenant carried in ctx
// (falling back to the platform-default tenant) and swaps that tenant's
// in-memory snapshot. This is the immediate single-tenant refresh path (e.g.
// admin writes); startup and background ticks use RefreshAll instead.
func (s *Service) Refresh(ctx context.Context) error {
	return s.refreshTenant(ctx, tenantIDFromContext(ctx))
}

// RefreshAll rebuilds the full per-tenant snapshot map from storage, one
// snapshot per tenantID. An empty tenantIDs list falls back to the default
// tenant only.
func (s *Service) RefreshAll(ctx context.Context, tenantIDs []string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	if len(tenantIDs) == 0 {
		tenantIDs = []string{defaultTenantID}
	}
	newMap := make(map[string]*serviceSnapshot, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		if tenantID == "" {
			tenantID = defaultTenantID
		}
		definitions, err := s.store.ListEffective(ctx, tenantID)
		if err != nil {
			return guardrailServiceError("list guardrails", err)
		}
		next, err := buildSnapshot(definitions, s.executor)
		if err != nil {
			return fmt.Errorf("guardrails refresh tenant %s: %w", tenantID, err)
		}
		newMap[tenantID] = &next
	}
	s.mu.Lock()
	s.snapshots = newMap
	s.mu.Unlock()
	return nil
}

// refreshTenant rebuilds the snapshot for one explicit tenant ID from storage
// and swaps it into the map.
func (s *Service) refreshTenant(ctx context.Context, tenantID string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.refreshTenantLocked(ctx, tenantID)
}

func (s *Service) refreshTenantLocked(ctx context.Context, tenantID string) error {
	definitions, err := s.store.ListEffective(ctx, tenantID)
	if err != nil {
		return guardrailServiceError("list guardrails", err)
	}
	next, err := buildSnapshot(definitions, s.executor)
	if err != nil {
		return guardrailServiceError("load guardrails", err)
	}

	s.storeSnapshot(tenantID, next)
	return nil
}

// storeSnapshot publishes a rebuilt snapshot for tenantID under mu. Snapshots
// are immutable after publication, so readers may hold the returned pointer
// without further locking.
func (s *Service) storeSnapshot(tenantID string, next serviceSnapshot) {
	s.mu.Lock()
	s.snapshots[tenantID] = &next
	s.mu.Unlock()
}

// SetExecutor swaps the auxiliary chat executor used by llm_based_altering
// guardrails and rebuilds every cached tenant snapshot atomically.
func (s *Service) SetExecutor(ctx context.Context, executor ChatCompletionExecutor) error {
	if s == nil {
		return nil
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	nextSnapshots := make(map[string]*serviceSnapshot, len(s.snapshots))
	for tenantID, snap := range s.snapshots {
		definitions := make([]Definition, 0, len(snap.definitions))
		for _, definition := range snap.definitions {
			definitions = append(definitions, definition)
		}
		next, err := buildSnapshot(definitions, executor)
		if err != nil {
			return guardrailServiceError("load guardrails", err)
		}
		nextSnapshots[tenantID] = &next
	}
	s.executor = executor
	s.snapshots = nextSnapshots
	return nil
}

// UpsertDefinitions validates and upserts a definition set, then swaps the default
// tenant's snapshot on success.
func (s *Service) UpsertDefinitions(ctx context.Context, definitions []Definition) error {
	if s == nil || len(definitions) == 0 {
		return nil
	}

	normalized := make([]Definition, 0, len(definitions))
	for _, definition := range definitions {
		normalizedDefinition, err := normalizeDefinition(definition)
		if err != nil {
			return err
		}
		normalized = append(normalized, normalizedDefinition)
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	currentDefinitions, err := s.store.List(ctx, s.tenantID)
	if err != nil {
		return guardrailServiceError("list guardrails", err)
	}
	nextDefinitions := definitionMap(currentDefinitions)
	for _, definition := range normalized {
		nextDefinitions[definition.Name] = definition
	}
	next, err := buildSnapshot(definitionsFromMap(nextDefinitions), s.executor)
	if err != nil {
		return err
	}
	if err := s.store.UpsertMany(ctx, s.tenantID, normalized); err != nil {
		return guardrailServiceError("upsert guardrails", err)
	}
	s.storeSnapshot(s.tenantID, next)
	return nil
}

// List returns all cached guardrail definitions sorted by name.
func (s *Service) List() []Definition {
	snap := s.loadSnapshot()
	if snap == nil {
		return []Definition{}
	}

	result := make([]Definition, 0, len(snap.order))
	for _, name := range snap.order {
		result = append(result, cloneDefinition(snap.definitions[name]))
	}
	return result
}

// ListViews returns all cached guardrail definitions with lightweight summaries.
func (s *Service) ListViews() []View {
	definitions := s.List()
	views := make([]View, 0, len(definitions))
	for _, definition := range definitions {
		views = append(views, ViewFromDefinition(definition))
	}
	return views
}

// Get returns one cached guardrail by name.
func (s *Service) Get(name string) (*Definition, bool) {
	name = normalizeDefinitionName(name)
	if name == "" {
		return nil, false
	}

	snap := s.loadSnapshot()
	if snap == nil {
		return nil, false
	}
	definition, ok := snap.definitions[name]
	if !ok {
		return nil, false
	}
	copy := cloneDefinition(definition)
	return &copy, true
}

// Upsert validates and stores a guardrail definition, then swaps the default
// tenant's snapshot on success.
func (s *Service) Upsert(ctx context.Context, definition Definition) error {
	normalized, err := normalizeDefinition(definition)
	if err != nil {
		return err
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	next, err := s.buildUpsertSnapshot(ctx, s.tenantID, normalized)
	if err != nil {
		return err
	}
	if err := s.store.Upsert(ctx, s.tenantID, normalized); err != nil {
		return guardrailServiceError("upsert guardrail", err)
	}
	s.storeSnapshot(s.tenantID, next)
	return nil
}

// buildUpsertSnapshot reads the current definitions for tenantID, merges the
// normalized definition into them, and builds the next in-memory snapshot. It
// does not persist anything or swap the cached snapshot; the caller must hold
// refreshMu.
func (s *Service) buildUpsertSnapshot(ctx context.Context, tenantID string, normalized Definition) (serviceSnapshot, error) {
	currentDefinitions, err := s.store.List(ctx, tenantID)
	if err != nil {
		return serviceSnapshot{}, guardrailServiceError("list guardrails", err)
	}
	nextDefinitions := definitionMap(currentDefinitions)
	nextDefinitions[normalized.Name] = normalized
	next, err := buildSnapshot(definitionsFromMap(nextDefinitions), s.executor)
	if err != nil {
		return serviceSnapshot{}, err
	}
	return next, nil
}

// Delete removes a guardrail definition from storage and swaps the default
// tenant's snapshot on success.
func (s *Service) Delete(ctx context.Context, name string) error {
	name = normalizeDefinitionName(name)
	if name == "" {
		return newValidationError("guardrail name is required", nil)
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	currentDefinitions, err := s.store.List(ctx, s.tenantID)
	if err != nil {
		return guardrailServiceError("list guardrails", err)
	}
	nextDefinitions := definitionMap(currentDefinitions)
	delete(nextDefinitions, name)
	next, err := buildSnapshot(definitionsFromMap(nextDefinitions), s.executor)
	if err != nil {
		return err
	}
	if err := s.store.Delete(ctx, s.tenantID, name); err != nil {
		return guardrailServiceError("delete guardrail", err)
	}
	s.storeSnapshot(s.tenantID, next)
	return nil
}

// TypeDefinitions returns the supported guardrail type schemas.
func (s *Service) TypeDefinitions() []TypeDefinition {
	return TypeDefinitions()
}

// Len returns the number of loaded guardrails.
func (s *Service) Len() int {
	snap := s.loadSnapshot()
	if snap == nil {
		return 0
	}
	return len(snap.order)
}

// Names returns the loaded guardrail names in sorted order.
func (s *Service) Names() []string {
	snap := s.loadSnapshot()
	if snap == nil {
		return []string{}
	}
	return append([]string(nil), snap.order...)
}

// BuildPipeline resolves named steps through the current in-memory guardrail
// registry for the tenant carried in ctx (falling back to the platform-default
// tenant). Tenants without a cached snapshot see an empty catalog.
func (s *Service) BuildPipeline(ctx context.Context, steps []StepReference) (*Pipeline, string, error) {
	if len(steps) == 0 {
		return nil, "", nil
	}

	registry := s.snapshotFor(ctx).registry
	if registry == nil {
		return nil, "", core.NewProviderError("", http.StatusBadGateway, "guardrail catalog is not loaded", nil)
	}
	return registry.BuildPipeline(ctx, steps)
}

// snapshotFor returns the snapshot for the tenant carried in ctx (falling back
// to the default tenant), or the shared empty snapshot when the tenant has no
// cached guardrails yet.
func (s *Service) snapshotFor(ctx context.Context) *serviceSnapshot {
	tenantID := tenantIDFromContext(ctx)

	s.mu.Lock()
	snap := s.snapshots[tenantID]
	s.mu.Unlock()
	if snap == nil {
		return emptySnapshot
	}
	return snap
}

// loadSnapshot returns the default tenant's snapshot for the legacy
// admin/dashboard methods (List/ListViews/Get/Len/Names) that predate
// per-tenant caching.
func (s *Service) loadSnapshot() *serviceSnapshot {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	snap := s.snapshots[s.tenantID]
	s.mu.Unlock()
	return snap
}

func tenantIDFromContext(ctx context.Context) string {
	tenantID := core.GetTenantID(ctx)
	if tenantID == "" {
		return defaultTenantID
	}
	return tenantID
}

func buildSnapshot(definitions []Definition, executor ChatCompletionExecutor) (serviceSnapshot, error) {
	next := serviceSnapshot{
		definitions: make(map[string]Definition, len(definitions)),
		order:       make([]string, 0, len(definitions)),
		registry:    NewRegistry(),
	}
	for _, definition := range definitions {
		normalized, err := normalizeDefinition(definition)
		if err != nil {
			return serviceSnapshot{}, fmt.Errorf("load guardrail %q: %w", definition.Name, err)
		}
		instance, descriptor, err := buildDefinition(normalized, executor)
		if err != nil {
			return serviceSnapshot{}, fmt.Errorf("load guardrail %q: %w", normalized.Name, err)
		}
		if err := next.registry.Register(instance, descriptor); err != nil {
			return serviceSnapshot{}, fmt.Errorf("register guardrail %q: %w", normalized.Name, err)
		}
		next.definitions[normalized.Name] = normalized
		next.order = append(next.order, normalized.Name)
	}
	sort.Strings(next.order)
	return next, nil
}

func definitionMap(definitions []Definition) map[string]Definition {
	next := make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		next[definition.Name] = cloneDefinition(definition)
	}
	return next
}

func definitionsFromMap(definitions map[string]Definition) []Definition {
	result := make([]Definition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, definition)
	}
	return result
}

func guardrailServiceError(message string, err error) error {
	if err == nil {
		return nil
	}
	if gatewayErr, ok := errors.AsType[*core.GatewayError](err); ok {
		return gatewayErr
	}
	if IsValidationError(err) {
		return core.NewInvalidRequestError(message+": "+err.Error(), err)
	}
	return core.NewProviderError("", http.StatusBadGateway, message+": "+err.Error(), err)
}
