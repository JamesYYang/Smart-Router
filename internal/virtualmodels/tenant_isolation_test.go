package virtualmodels

import (
	"context"
	"testing"

	"smartrouter/internal/core"
)

// TestService_TenantIsolation_ResolveAndFilter verifies the inference hot path
// reads the calling tenant's snapshot: a redirect and a policy stored for
// tenant-a apply only for tenant-a, while tenant-b (uncached) and the default
// tenant are unaffected.
func TestService_TenantIsolation_ResolveAndFilter(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()

	// Seed tenant-a's rows directly (bypassing the default-tenant admin CRUD).
	if err := svc.store.Upsert(ctx, "tenant-a", VirtualModel{
		Source:  "fast",
		Targets: []Target{{Provider: "openai", Model: "gpt-4o"}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("store.Upsert(tenant-a redirect) error = %v", err)
	}
	if err := svc.store.Upsert(ctx, "tenant-a", VirtualModel{
		Source:    "openai/gpt-4o",
		UserPaths: []string{"/team"},
		Enabled:   true,
	}); err != nil {
		t.Fatalf("store.Upsert(tenant-a policy) error = %v", err)
	}

	if err := svc.RefreshAll(ctx, []string{"tenant-a"}); err != nil {
		t.Fatalf("RefreshAll() error = %v", err)
	}

	ctxA := core.WithTenantID(ctx, "tenant-a")
	ctxB := core.WithTenantID(ctx, "tenant-b")

	// Redirect "fast" resolves only for tenant-a.
	if sel, changed, err := svc.ResolveModel(ctxA, core.NewRequestedModelSelector("fast", "")); err != nil || !changed || sel.QualifiedModel() != "openai/gpt-4o" {
		t.Fatalf("ResolveModel(tenant-a, fast) = %q changed=%v err=%v, want openai/gpt-4o true nil", sel.QualifiedModel(), changed, err)
	}
	if _, changed, err := svc.ResolveModel(ctxB, core.NewRequestedModelSelector("fast", "")); err != nil || changed {
		t.Fatalf("ResolveModel(tenant-b, fast) changed=%v err=%v, want false nil (tenant-b unaffected)", changed, err)
	}
	if _, changed, err := svc.ResolveModel(ctx, core.NewRequestedModelSelector("fast", "")); err != nil || changed {
		t.Fatalf("ResolveModel(default, fast) changed=%v err=%v, want false nil (default unaffected)", changed, err)
	}

	// Policy "openai/gpt-4o" user_paths=/team gates access only for tenant-a.
	models := []core.Model{{ID: "openai/gpt-4o"}}
	if got := svc.FilterPublicModels(ctxA, models); len(got) != 0 {
		t.Fatalf("FilterPublicModels(tenant-a, no path) = %#v, want empty", got)
	}
	if got := svc.FilterPublicModels(ctxB, models); len(got) != 1 {
		t.Fatalf("FilterPublicModels(tenant-b) = %#v, want retained (no policy for tenant-b)", got)
	}
	if got := svc.FilterPublicModels(ctx, models); len(got) != 1 {
		t.Fatalf("FilterPublicModels(default) = %#v, want retained (no policy for default)", got)
	}
	// Matching tenant-a user path allows the model.
	allowedCtxA := core.WithEffectiveUserPath(ctxA, "/team/alice")
	if got := svc.FilterPublicModels(allowedCtxA, models); len(got) != 1 {
		t.Fatalf("FilterPublicModels(tenant-a, /team/alice) = %#v, want retained", got)
	}
}

// TestService_TenantIsolation_FallbackEmpty verifies a tenant with no cached
// snapshot (and no store rows) gets an empty snapshot, not the default tenant's.
func TestService_TenantIsolation_FallbackEmpty(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()

	// Seed the default tenant with a redirect, then refresh only the default.
	if err := svc.store.Upsert(ctx, "default", VirtualModel{
		Source:  "fast",
		Targets: []Target{{Provider: "openai", Model: "gpt-4o"}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("store.Upsert(default redirect) error = %v", err)
	}
	if err := svc.RefreshAll(ctx, nil); err != nil {
		t.Fatalf("RefreshAll(default) error = %v", err)
	}

	// Default resolves; an uncached tenant falls back to empty.
	if _, changed, _ := svc.ResolveModel(ctx, core.NewRequestedModelSelector("fast", "")); !changed {
		t.Fatalf("ResolveModel(default) changed = false, want true")
	}
	uncached := core.WithTenantID(ctx, "tenant-uncached")
	if _, changed, _ := svc.ResolveModel(uncached, core.NewRequestedModelSelector("fast", "")); changed {
		t.Fatalf("ResolveModel(uncached tenant) changed = true, want false (empty fallback, not default snapshot)")
	}
}

// TestService_RefreshIsolatesTenant verifies Refresh(ctx) swaps only the ctx
// tenant's snapshot, preserving other tenants' cached snapshots.
func TestService_RefreshIsolatesTenant(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.store.Upsert(ctx, "tenant-a", VirtualModel{
		Source:  "fast-a",
		Targets: []Target{{Provider: "openai", Model: "gpt-4o"}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("store.Upsert(tenant-a) error = %v", err)
	}
	if err := svc.store.Upsert(ctx, "tenant-b", VirtualModel{
		Source:  "fast-b",
		Targets: []Target{{Provider: "openai", Model: "gpt-4o"}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("store.Upsert(tenant-b) error = %v", err)
	}

	if err := svc.RefreshAll(ctx, []string{"tenant-a", "tenant-b"}); err != nil {
		t.Fatalf("RefreshAll() error = %v", err)
	}

	// Refresh only tenant-a; tenant-b's snapshot must survive the clone-swap.
	ctxA := core.WithTenantID(ctx, "tenant-a")
	if err := svc.Refresh(ctxA); err != nil {
		t.Fatalf("Refresh(tenant-a) error = %v", err)
	}
	ctxB := core.WithTenantID(ctx, "tenant-b")
	if _, changed, _ := svc.ResolveModel(ctxB, core.NewRequestedModelSelector("fast-b", "")); !changed {
		t.Fatalf("ResolveModel(tenant-b) after Refresh(tenant-a) changed = false, want true (tenant-b snapshot preserved)")
	}
	if _, changed, _ := svc.ResolveModel(ctxA, core.NewRequestedModelSelector("fast-a", "")); !changed {
		t.Fatalf("ResolveModel(tenant-a) after Refresh(tenant-a) changed = false, want true")
	}
}

// TestBalancer_PerTenantBalancer verifies round-robin position is per-tenant:
// tenant-a's rotation is independent of tenant-b's, so resolving tenant-a twice
// while tenant-b rotates does not advance tenant-a's position.
func TestBalancer_PerTenantBalancer(t *testing.T) {
	t.Parallel()
	svc := newBalancingService(t)
	ctx := context.Background()

	redirect := VirtualModel{
		Source:   "smart",
		Strategy: StrategyRoundRobin,
		Targets: []Target{
			{Provider: "openai", Model: "gpt-4o"},
			{Provider: "anthropic", Model: "claude"},
		},
		Enabled: true,
	}
	if err := svc.store.Upsert(ctx, "tenant-a", redirect); err != nil {
		t.Fatalf("store.Upsert(tenant-a) error = %v", err)
	}
	if err := svc.store.Upsert(ctx, "tenant-b", redirect); err != nil {
		t.Fatalf("store.Upsert(tenant-b) error = %v", err)
	}
	if err := svc.RefreshAll(ctx, []string{"tenant-a", "tenant-b"}); err != nil {
		t.Fatalf("RefreshAll() error = %v", err)
	}

	ctxA := core.WithTenantID(ctx, "tenant-a")
	ctxB := core.WithTenantID(ctx, "tenant-b")

	// Tenant-a takes position 0.
	firstA, _, err := svc.ResolveModel(ctxA, core.NewRequestedModelSelector("smart", ""))
	if err != nil {
		t.Fatalf("ResolveModel(tenant-a #1) error = %v", err)
	}
	// Tenant-b rotates three times (positions 0,1,2) independently.
	for i, want := range []string{"openai/gpt-4o", "anthropic/claude", "openai/gpt-4o"} {
		sel, _, err := svc.ResolveModel(ctxB, core.NewRequestedModelSelector("smart", ""))
		if err != nil {
			t.Fatalf("ResolveModel(tenant-b #%d) error = %v", i+1, err)
		}
		if sel.QualifiedModel() != want {
			t.Fatalf("ResolveModel(tenant-b #%d) = %q, want %q", i+1, sel.QualifiedModel(), want)
		}
	}
	// Tenant-a's next position must be 1 (claude), not 3 — proving its counter
	// was not advanced by tenant-b's rotations.
	secondA, _, err := svc.ResolveModel(ctxA, core.NewRequestedModelSelector("smart", ""))
	if err != nil {
		t.Fatalf("ResolveModel(tenant-a #2) error = %v", err)
	}
	if secondA.QualifiedModel() != "anthropic/claude" {
		t.Fatalf("ResolveModel(tenant-a #2) = %q, want anthropic/claude (per-tenant round-robin)", secondA.QualifiedModel())
	}
	if firstA.QualifiedModel() != "openai/gpt-4o" {
		t.Fatalf("ResolveModel(tenant-a #1) = %q, want openai/gpt-4o", firstA.QualifiedModel())
	}
}

// TestService_RefreshAll_EmptySeedsDefault verifies RefreshAll with an empty
// tenant list seeds only the default tenant.
func TestService_RefreshAll_EmptySeedsDefault(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.store.Upsert(ctx, "default", VirtualModel{
		Source:  "fast",
		Targets: []Target{{Provider: "openai", Model: "gpt-4o"}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("store.Upsert(default) error = %v", err)
	}
	if err := svc.RefreshAll(ctx, []string{}); err != nil {
		t.Fatalf("RefreshAll(empty) error = %v", err)
	}
	if _, changed, _ := svc.ResolveModel(ctx, core.NewRequestedModelSelector("fast", "")); !changed {
		t.Fatalf("ResolveModel(default) changed = false, want true")
	}
}
