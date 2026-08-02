package virtualmodels

import (
	"context"
	"errors"
	"math"
	"testing"
)

const tenantDefault = "default"

func TestSQLiteStore_RoundTripRedirectAndPolicy(t *testing.T) {
	t.Parallel()
	store := newSQLiteVMStore(t)
	ctx := context.Background()

	redirect := VirtualModel{
		Source:      "fast",
		Targets:     []Target{{Provider: "openai", Model: "gpt-4o"}},
		Description: "primary",
		Enabled:     true,
	}
	policy := VirtualModel{
		Source:       "openai/gpt-4o",
		ProviderName: "openai",
		Model:        "gpt-4o",
		UserPaths:    []string{"/team"},
		Enabled:      true,
	}
	if err := store.Upsert(ctx, tenantDefault, redirect); err != nil {
		t.Fatalf("Upsert(redirect) error = %v", err)
	}
	if err := store.Upsert(ctx, tenantDefault, policy); err != nil {
		t.Fatalf("Upsert(policy) error = %v", err)
	}

	got, err := store.List(ctx, tenantDefault)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(List()) = %d, want 2", len(got))
	}

	gotRedirect, err := store.Get(ctx, tenantDefault, "fast")
	if err != nil {
		t.Fatalf("Get(fast) error = %v", err)
	}
	if !gotRedirect.IsRedirect() {
		t.Fatalf("Get(fast).IsRedirect() = false, want true")
	}
	if len(gotRedirect.Targets) != 1 || gotRedirect.Targets[0].Model != "gpt-4o" || gotRedirect.Targets[0].Provider != "openai" {
		t.Fatalf("Get(fast).Targets = %#v, want [{openai gpt-4o 0}]", gotRedirect.Targets)
	}

	gotPolicy, err := store.Get(ctx, tenantDefault, "openai/gpt-4o")
	if err != nil {
		t.Fatalf("Get(policy) error = %v", err)
	}
	if gotPolicy.IsRedirect() {
		t.Fatalf("Get(policy).IsRedirect() = true, want false")
	}
	if len(gotPolicy.UserPaths) != 1 || gotPolicy.UserPaths[0] != "/team" {
		t.Fatalf("Get(policy).UserPaths = %#v, want [/team]", gotPolicy.UserPaths)
	}
}

func TestSQLiteStore_GetMissingAndDelete(t *testing.T) {
	t.Parallel()
	store := newSQLiteVMStore(t)
	ctx := context.Background()

	if _, err := store.Get(ctx, tenantDefault, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, tenantDefault, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(missing) error = %v, want ErrNotFound", err)
	}

	if err := store.Upsert(ctx, tenantDefault, VirtualModel{Source: "x", Targets: []Target{{Model: "m"}}, Enabled: true}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if err := store.Delete(ctx, tenantDefault, "x"); err != nil {
		t.Fatalf("Delete(x) error = %v", err)
	}
	if _, err := store.Get(ctx, tenantDefault, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(deleted) error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteStore_UpsertAllIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	vms := []VirtualModel{
		{Source: "fast", Targets: []Target{{Provider: "openai", Model: "gpt-4o"}}, Enabled: true},
		{Source: "openai/gpt-4o", Enabled: false},
	}

	// Success: the whole batch is committed.
	store := newSQLiteVMStore(t)
	if err := store.UpsertAll(ctx, tenantDefault, vms); err != nil {
		t.Fatalf("UpsertAll() error = %v", err)
	}
	got, err := store.List(ctx, tenantDefault)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(List()) = %d, want 2", len(got))
	}

	// Mid-batch failure: the first row is written inside the transaction, then the
	// second row fails to encode (a non-finite Weight cannot be JSON-marshalled).
	// The whole batch must roll back — the first row must not survive.
	store2 := newSQLiteVMStore(t)
	err = store2.UpsertAll(ctx, tenantDefault, []VirtualModel{
		{Source: "good", Targets: []Target{{Provider: "openai", Model: "gpt-4o"}}, Enabled: true},
		{Source: "bad", Targets: []Target{{Provider: "openai", Model: "gpt-4o", Weight: math.Inf(1)}}, Enabled: true},
	})
	if err == nil {
		t.Fatal("UpsertAll(mid-batch failure) error = nil, want error")
	}
	got2, err := store2.List(ctx, tenantDefault)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got2) != 0 {
		t.Fatalf("len(List()) = %d after mid-batch failure, want 0 (atomic rollback)", len(got2))
	}
}

// --- P3: Tenant isolation tests ---

func TestSQLiteStore_TenantIsolation(t *testing.T) {
	t.Parallel()
	store := newSQLiteVMStore(t)
	ctx := context.Background()

	// Upsert source="s" under tenant A and tenant B.
	vmA := VirtualModel{Source: "s", Targets: []Target{{Provider: "openai", Model: "gpt-4o"}}, Enabled: true}
	vmB := VirtualModel{Source: "s", Targets: []Target{{Provider: "openai", Model: "gpt-4o-mini"}}, Enabled: true}

	if err := store.Upsert(ctx, "tenant-A", vmA); err != nil {
		t.Fatalf("Upsert(A,s) error = %v", err)
	}
	if err := store.Upsert(ctx, "tenant-B", vmB); err != nil {
		t.Fatalf("Upsert(B,s) error = %v", err)
	}

	// List(ctx, "tenant-A") returns only A's row.
	gotA, err := store.List(ctx, "tenant-A")
	if err != nil {
		t.Fatalf("List(tenant-A) error = %v", err)
	}
	if len(gotA) != 1 {
		t.Fatalf("len(List(tenant-A)) = %d, want 1", len(gotA))
	}
	if gotA[0].Targets[0].Model != "gpt-4o" {
		t.Fatalf("List(tenant-A)[0].Targets = %v, want gpt-4o", gotA[0].Targets)
	}

	// List(ctx, "tenant-B") returns only B's row.
	gotB, err := store.List(ctx, "tenant-B")
	if err != nil {
		t.Fatalf("List(tenant-B) error = %v", err)
	}
	if len(gotB) != 1 {
		t.Fatalf("len(List(tenant-B)) = %d, want 1", len(gotB))
	}
	if gotB[0].Targets[0].Model != "gpt-4o-mini" {
		t.Fatalf("List(tenant-B)[0].Targets = %v, want gpt-4o-mini", gotB[0].Targets)
	}

	// Get with wrong tenant returns ErrNotFound.
	if _, err := store.Get(ctx, "tenant-A", "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(wrong tenant) error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteStore_ListEffective(t *testing.T) {
	t.Parallel()
	store := newSQLiteVMStore(t)
	ctx := context.Background()

	// Seed a platform default row.
	defaultVM := VirtualModel{Source: "s", Targets: []Target{{Provider: "openai", Model: "gpt-4o"}}, Enabled: true}
	if err := store.Upsert(ctx, "default", defaultVM); err != nil {
		t.Fatalf("Upsert(default,s) error = %v", err)
	}

	// Tenant-A overrides source="s".
	tenantAVM := VirtualModel{Source: "s", Targets: []Target{{Provider: "anthropic", Model: "claude"}}, Enabled: true}
	if err := store.Upsert(ctx, "tenant-A", tenantAVM); err != nil {
		t.Fatalf("Upsert(A,s) error = %v", err)
	}

	// Tenant-B has no override for source="s", but has its own row.
	tenantBVM := VirtualModel{Source: "other", Targets: []Target{{Provider: "openai", Model: "gpt-4o-mini"}}, Enabled: true}
	if err := store.Upsert(ctx, "tenant-B", tenantBVM); err != nil {
		t.Fatalf("Upsert(B,other) error = %v", err)
	}

	// ListEffective(ctx, "tenant-A"): tenant-A's "s" wins over default's "s".
	gotA, err := store.ListEffective(ctx, "tenant-A")
	if err != nil {
		t.Fatalf("ListEffective(tenant-A) error = %v", err)
	}
	if len(gotA) != 1 {
		t.Fatalf("len(ListEffective(tenant-A)) = %d, want 1 (only s, tenant overrides default)", len(gotA))
	}
	if gotA[0].Targets[0].Provider != "anthropic" {
		t.Fatalf("ListEffective(tenant-A): got provider=%q, want anthropic (tenant wins)", gotA[0].Targets[0].Provider)
	}

	// ListEffective(ctx, "tenant-B"): tenant-B has no override for "s", returns default's "s".
	gotB, err := store.ListEffective(ctx, "tenant-B")
	if err != nil {
		t.Fatalf("ListEffective(tenant-B) error = %v", err)
	}
	bySourceB := make(map[string]VirtualModel, len(gotB))
	for _, vm := range gotB {
		bySourceB[vm.Source] = vm
	}
	if len(bySourceB) != 2 {
		t.Fatalf("len(ListEffective(tenant-B)) = %d, want 2 (default:s + tenant-B:other)", len(bySourceB))
	}
	if svm, ok := bySourceB["s"]; !ok || svm.Targets[0].Provider != "openai" {
		t.Fatalf("ListEffective(tenant-B) s: provider=%q, want openai (no override, use default)", svm.Targets[0].Provider)
	}
	if _, ok := bySourceB["other"]; !ok {
		t.Fatal("ListEffective(tenant-B): missing tenant-B's own row 'other'")
	}

	// ListEffective(ctx, "nonexistent"): returns only default rows.
	gotNone, err := store.ListEffective(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListEffective(nonexistent) error = %v", err)
	}
	if len(gotNone) != 1 {
		t.Fatalf("len(ListEffective(nonexistent)) = %d, want 1 (only default:s)", len(gotNone))
	}
	if gotNone[0].Targets[0].Provider != "openai" {
		t.Fatalf("ListEffective(nonexistent)[0]: provider=%q, want openai", gotNone[0].Targets[0].Provider)
	}
}
