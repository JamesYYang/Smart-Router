package failover

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"smartrouter/config"
	"smartrouter/internal/core"
)

type memoryStore struct {
	rows map[string]Rule
}

func newMemoryStore(rows ...Rule) *memoryStore {
	store := &memoryStore{rows: make(map[string]Rule)}
	for _, row := range rows {
		store.rows[row.Source] = row.clone()
	}
	return store
}

func (s *memoryStore) List(_ context.Context, tenantID string) ([]Rule, error) {
	_ = tenantID
	rows := make([]Rule, 0, len(s.rows))
	for _, row := range s.rows {
		rows = append(rows, row.clone())
	}
	return rows, nil
}

func (s *memoryStore) ListEffective(_ context.Context, tenantID string) ([]Rule, error) {
	return s.List(nil, tenantID)
}

func (s *memoryStore) Get(_ context.Context, tenantID, source string) (*Rule, error) {
	_ = tenantID
	row, ok := s.rows[source]
	if !ok {
		return nil, ErrNotFound
	}
	clone := row.clone()
	return &clone, nil
}

func (s *memoryStore) Upsert(_ context.Context, tenantID string, rule Rule) error {
	_ = tenantID
	s.rows[rule.Source] = rule.clone()
	return nil
}

func (s *memoryStore) Delete(_ context.Context, tenantID, source string) error {
	_ = tenantID
	if _, ok := s.rows[source]; !ok {
		return ErrNotFound
	}
	delete(s.rows, source)
	return nil
}

func (s *memoryStore) DeleteAll(_ context.Context, tenantID string) error {
	_ = tenantID
	s.rows = make(map[string]Rule)
	return nil
}

func (s *memoryStore) Close() error { return nil }

// errGetStore returns a fixed error from Get, simulating a transient storage
// fault during the Upsert pre-read.
type errGetStore struct {
	*memoryStore
	getErr error
}

func (s *errGetStore) Get(_ context.Context, _ string, _ string) (*Rule, error) {
	return nil, s.getErr
}

func TestServiceUpsertPropagatesUnexpectedGetError(t *testing.T) {
	wantErr := errors.New("boom")
	store := &errGetStore{memoryStore: newMemoryStore(), getErr: wantErr}
	service, err := NewService(store, config.FailoverConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	err = service.Upsert(context.Background(), Rule{Source: "gpt-4o", Targets: []string{"azure/gpt-4o"}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Upsert() error = %v, want it to wrap %v", err, wantErr)
	}
	if _, ok := store.rows["gpt-4o"]; ok {
		t.Fatalf("rule was written despite the failed pre-read")
	}
}

func TestServiceConfigRulesOverrideDashboardRules(t *testing.T) {
	store := newMemoryStore(Rule{
		Source:        "gpt-4o",
		Targets:       []string{"openrouter/gpt-4o"},
		Enabled:       true,
		ManagedSource: ManagedSourceDashboard,
	})
	service, err := NewService(store, config.FailoverConfig{
		Enabled: true,
		Manual: map[string][]string{
			"gpt-4o": {"azure/gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	got := service.Rules(context.Background())["gpt-4o"]
	want := []string{"azure/gpt-4o"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Rules()[gpt-4o] = %v, want %v", got, want)
	}
	view, ok := service.Get("gpt-4o")
	if !ok || view.ManagedSource != ManagedSourceConfig {
		t.Fatalf("Get(gpt-4o) = %+v, %v; want config-managed rule", view, ok)
	}
}

// TestServiceRulesReuseCachedSnapshot guards the resolver hot path: Rules and
// Disabled are read on every request, so they must return the cached snapshot
// maps rather than rebuilding (and re-cloning every rule) on each call. A new
// snapshot is published only on Refresh.
func TestServiceRulesReuseCachedSnapshot(t *testing.T) {
	store := newMemoryStore(
		Rule{Source: "gpt-4o", Targets: []string{"azure/gpt-4o"}, Enabled: true, ManagedSource: ManagedSourceDashboard},
		Rule{Source: "gpt-4o-mini", Enabled: false, ManagedSource: ManagedSourceDashboard},
	)
	service, err := NewService(store, config.FailoverConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	if a, b := service.Rules(context.Background()), service.Rules(context.Background()); reflect.ValueOf(a).Pointer() != reflect.ValueOf(b).Pointer() {
		t.Fatal("Rules() rebuilt the map; expected the cached snapshot reused across calls")
	}
	if a, b := service.Disabled(context.Background()), service.Disabled(context.Background()); reflect.ValueOf(a).Pointer() != reflect.ValueOf(b).Pointer() {
		t.Fatal("Disabled() rebuilt the map; expected the cached snapshot reused across calls")
	}

	before := service.Rules(context.Background())
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if reflect.ValueOf(before).Pointer() == reflect.ValueOf(service.Rules(context.Background())).Pointer() {
		t.Fatal("Refresh() did not publish a new snapshot")
	}
}

// tenantMemoryStore keys rules by (tenantID, source) so per-tenant cache
// behavior can be exercised end-to-end.
type tenantMemoryStore struct {
	rows map[string]map[string]Rule
}

func newTenantMemoryStore() *tenantMemoryStore {
	return &tenantMemoryStore{rows: make(map[string]map[string]Rule)}
}

func (s *tenantMemoryStore) List(_ context.Context, tenantID string) ([]Rule, error) {
	out := make([]Rule, 0, len(s.rows[tenantID]))
	for _, row := range s.rows[tenantID] {
		out = append(out, row.clone())
	}
	return out, nil
}

func (s *tenantMemoryStore) ListEffective(_ context.Context, tenantID string) ([]Rule, error) {
	return s.List(nil, tenantID)
}

func (s *tenantMemoryStore) Get(_ context.Context, tenantID, source string) (*Rule, error) {
	row, ok := s.rows[tenantID][source]
	if !ok {
		return nil, ErrNotFound
	}
	clone := row.clone()
	return &clone, nil
}

func (s *tenantMemoryStore) Upsert(_ context.Context, tenantID string, rule Rule) error {
	if s.rows[tenantID] == nil {
		s.rows[tenantID] = make(map[string]Rule)
	}
	s.rows[tenantID][rule.Source] = rule.clone()
	return nil
}

func (s *tenantMemoryStore) Delete(_ context.Context, tenantID, source string) error {
	if _, ok := s.rows[tenantID][source]; !ok {
		return ErrNotFound
	}
	delete(s.rows[tenantID], source)
	return nil
}

func (s *tenantMemoryStore) DeleteAll(_ context.Context, tenantID string) error {
	s.rows[tenantID] = make(map[string]Rule)
	return nil
}

func (s *tenantMemoryStore) Close() error { return nil }

// TestServiceRefreshUsesTenantFromContext verifies the immediate single-tenant
// refresh path reads the tenant ID from ctx (fallback "default"), and that a
// tenant-scoped Refresh does not clobber other tenants' snapshots.
func TestServiceRefreshUsesTenantFromContext(t *testing.T) {
	store := newTenantMemoryStore()
	if err := store.Upsert(context.Background(), "tenant-a", Rule{
		Source: "gpt-4o", Targets: []string{"azure/gpt-4o"}, Enabled: true, ManagedSource: ManagedSourceDashboard,
	}); err != nil {
		t.Fatalf("seed tenant-a: %v", err)
	}
	if err := store.Upsert(context.Background(), defaultTenantID, Rule{
		Source: "gpt-4o", Targets: []string{"gemini/gemini-2.5-pro"}, Enabled: true, ManagedSource: ManagedSourceDashboard,
	}); err != nil {
		t.Fatalf("seed default: %v", err)
	}

	service, err := NewService(store, config.FailoverConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	if err := service.Refresh(ctxA); err != nil {
		t.Fatalf("Refresh(tenant-a) error = %v", err)
	}
	if got := service.Rules(ctxA)["gpt-4o"]; !reflect.DeepEqual(got, []string{"azure/gpt-4o"}) {
		t.Fatalf("Rules(tenant-a)[gpt-4o] = %v, want [azure/gpt-4o]", got)
	}
	// The default tenant's snapshot is untouched by a tenant-scoped Refresh.
	if got := service.Rules(context.Background())["gpt-4o"]; len(got) != 0 {
		t.Fatalf("Rules(default)[gpt-4o] = %v, want empty before default refresh", got)
	}

	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(default) error = %v", err)
	}
	if got := service.Rules(context.Background())["gpt-4o"]; !reflect.DeepEqual(got, []string{"gemini/gemini-2.5-pro"}) {
		t.Fatalf("Rules(default)[gpt-4o] = %v, want [gemini/gemini-2.5-pro]", got)
	}
}

// TestServiceRefreshAllIsolatesTenantSnapshots verifies RefreshAll builds an
// independent snapshot per tenant and that ResolveFailovers resolves the rules
// of the tenant carried in ctx.
func TestServiceRefreshAllIsolatesTenantSnapshots(t *testing.T) {
	store := newTenantMemoryStore()
	if err := store.Upsert(context.Background(), "tenant-a", Rule{
		Source: "gpt-4o", Targets: []string{"azure/gpt-4o"}, Enabled: true, ManagedSource: ManagedSourceDashboard,
	}); err != nil {
		t.Fatalf("seed tenant-a: %v", err)
	}
	if err := store.Upsert(context.Background(), "tenant-b", Rule{
		Source: "gpt-4o", Targets: []string{"gemini/gemini-2.5-pro"}, Enabled: true, ManagedSource: ManagedSourceDashboard,
	}); err != nil {
		t.Fatalf("seed tenant-b: %v", err)
	}

	service, err := NewService(store, config.FailoverConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := service.RefreshAll(context.Background(), []string{"tenant-a", "tenant-b"}); err != nil {
		t.Fatalf("RefreshAll() error = %v", err)
	}

	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	ctxB := core.WithTenantID(context.Background(), "tenant-b")
	if got := service.Rules(ctxA)["gpt-4o"]; !reflect.DeepEqual(got, []string{"azure/gpt-4o"}) {
		t.Fatalf("Rules(tenant-a)[gpt-4o] = %v, want [azure/gpt-4o]", got)
	}
	if got := service.Rules(ctxB)["gpt-4o"]; !reflect.DeepEqual(got, []string{"gemini/gemini-2.5-pro"}) {
		t.Fatalf("Rules(tenant-b)[gpt-4o] = %v, want [gemini/gemini-2.5-pro]", got)
	}

	registry := newFakeRegistry(
		modelInfo("gpt-4o", "openai", "openai", 1287, "gpt-4o"),
		modelInfo("gpt-4o", "azure", "azure", 1287, "gpt-4o"),
		modelInfo("gemini-2.5-pro", "gemini", "gemini", 1290, "gemini-2.5-pro"),
	)
	resolver := NewResolverWithRuleProvider(config.FailoverConfig{Enabled: true}, registry, service)
	resolution := &core.RequestModelResolution{
		Requested:        core.NewRequestedModelSelector("gpt-4o", ""),
		ResolvedSelector: core.ModelSelector{Model: "gpt-4o"},
		ProviderType:     "openai",
	}

	gotA := resolver.ResolveFailovers(ctxA, resolution, core.OperationChatCompletions)
	if len(gotA) != 1 || gotA[0].QualifiedModel() != "azure/gpt-4o" {
		t.Fatalf("ResolveFailovers(tenant-a) = %v, want [azure/gpt-4o]", gotA)
	}
	gotB := resolver.ResolveFailovers(ctxB, resolution, core.OperationChatCompletions)
	if len(gotB) != 1 || gotB[0].QualifiedModel() != "gemini/gemini-2.5-pro" {
		t.Fatalf("ResolveFailovers(tenant-b) = %v, want [gemini/gemini-2.5-pro]", gotB)
	}
}

// TestSnapshotForUnknownTenantFallsBackToEmpty verifies that a tenant with no
// cached snapshot gets an empty rule snapshot (and therefore no failovers)
// rather than the default tenant's rules.
func TestSnapshotForUnknownTenantFallsBackToEmpty(t *testing.T) {
	store := newTenantMemoryStore()
	if err := store.Upsert(context.Background(), "tenant-a", Rule{
		Source: "gpt-4o", Targets: []string{"azure/gpt-4o"}, Enabled: true, ManagedSource: ManagedSourceDashboard,
	}); err != nil {
		t.Fatalf("seed tenant-a: %v", err)
	}
	service, err := NewService(store, config.FailoverConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := service.RefreshAll(context.Background(), []string{"tenant-a"}); err != nil {
		t.Fatalf("RefreshAll() error = %v", err)
	}

	ctxUnknown := core.WithTenantID(context.Background(), "tenant-zz")
	snap := service.snapshotFor(ctxUnknown)
	if len(snap.rules) != 0 || len(snap.disabled) != 0 {
		t.Fatalf("snapshotFor(tenant-zz) = %+v, want empty rules/disabled", snap)
	}
	if got := service.Rules(ctxUnknown)["gpt-4o"]; len(got) != 0 {
		t.Fatalf("Rules(tenant-zz)[gpt-4o] = %v, want empty", got)
	}
}
