package pricingoverrides

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newSQLiteStoreForTest(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	return store
}

func TestSQLiteStoreStoresPricingWithoutCurrency(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	ctx := context.Background()

	if err := store.Upsert(ctx, "default", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(1.25)},
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	var rawPricing string
	if err := store.db.QueryRow(`SELECT pricing FROM model_pricing_overrides WHERE tenant_id = 'default' AND selector = 'openai/gpt-4o'`).Scan(&rawPricing); err != nil {
		t.Fatalf("read pricing JSON: %v", err)
	}
	if strings.Contains(rawPricing, "currency") {
		t.Fatalf("pricing JSON = %s, did not expect currency field", rawPricing)
	}

	overrides, err := store.List(ctx, "default")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(overrides) != 1 {
		t.Fatalf("len(overrides) = %d, want 1", len(overrides))
	}
	if overrides[0].ProviderName != "openai" || overrides[0].Model != "gpt-4o" {
		t.Fatalf("stored parts = (%q, %q), want (openai, gpt-4o)", overrides[0].ProviderName, overrides[0].Model)
	}
}

func TestSQLiteStore_TenantIsolation(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	ctx := context.Background()

	// Upsert same selector under two tenants.
	if err := store.Upsert(ctx, "default", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(1.0)},
	}); err != nil {
		t.Fatalf("Upsert(default, openai/gpt-4o) error = %v", err)
	}
	if err := store.Upsert(ctx, "tenant-A", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(2.0)},
	}); err != nil {
		t.Fatalf("Upsert(tenant-A, openai/gpt-4o) error = %v", err)
	}

	// List(ctx, "default") returns only default's row.
	defaultRows, err := store.List(ctx, "default")
	if err != nil {
		t.Fatalf("List(default) error = %v", err)
	}
	if len(defaultRows) != 1 {
		t.Fatalf("List(default) = %d rows, want 1", len(defaultRows))
	}
	if defaultRows[0].Pricing.InputPerMtok == nil || *defaultRows[0].Pricing.InputPerMtok != 1.0 {
		t.Fatalf("List(default) input = %v, want 1.0", defaultRows[0].Pricing.InputPerMtok)
	}

	// List(ctx, "tenant-A") returns only tenant-A's row.
	aRows, err := store.List(ctx, "tenant-A")
	if err != nil {
		t.Fatalf("List(tenant-A) error = %v", err)
	}
	if len(aRows) != 1 {
		t.Fatalf("List(tenant-A) = %d rows, want 1", len(aRows))
	}
	if aRows[0].Pricing.InputPerMtok == nil || *aRows[0].Pricing.InputPerMtok != 2.0 {
		t.Fatalf("List(tenant-A) input = %v, want 2.0", aRows[0].Pricing.InputPerMtok)
	}

	// List(ctx, "nonexistent") returns empty.
	noneRows, err := store.List(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("List(nonexistent) error = %v", err)
	}
	if len(noneRows) != 0 {
		t.Fatalf("List(nonexistent) = %d rows, want 0", len(noneRows))
	}
}

func TestSQLiteStore_ListEffective(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	ctx := context.Background()

	// Seed: default has "openai/gpt-4o" -> 1.0 and "openai/gpt-4o-mini" -> 0.5
	if err := store.Upsert(ctx, "default", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(1.0)},
	}); err != nil {
		t.Fatalf("Upsert(default, openai/gpt-4o) error = %v", err)
	}
	if err := store.Upsert(ctx, "default", Override{
		Selector: "openai/gpt-4o-mini",
		Pricing:  Pricing{InputPerMtok: ptr(0.5)},
	}); err != nil {
		t.Fatalf("Upsert(default, openai/gpt-4o-mini) error = %v", err)
	}

	// Tenant-A overrides "openai/gpt-4o" -> 2.0.
	if err := store.Upsert(ctx, "tenant-A", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(2.0)},
	}); err != nil {
		t.Fatalf("Upsert(tenant-A, openai/gpt-4o) error = %v", err)
	}

	// ListEffective(ctx, "tenant-A"): A's override wins for gpt-4o, default's gpt-4o-mini comes through.
	effA, err := store.ListEffective(ctx, "tenant-A")
	if err != nil {
		t.Fatalf("ListEffective(tenant-A) error = %v", err)
	}
	bySelector := make(map[string]Override, len(effA))
	for _, o := range effA {
		bySelector[o.Selector] = o
	}
	if len(bySelector) != 2 {
		t.Fatalf("ListEffective(tenant-A) = %d overrides, want 2", len(bySelector))
	}
	if o, ok := bySelector["openai/gpt-4o"]; !ok {
		t.Fatal("ListEffective(tenant-A) missing openai/gpt-4o")
	} else if o.Pricing.InputPerMtok == nil || *o.Pricing.InputPerMtok != 2.0 {
		t.Fatalf("openai/gpt-4o input = %v, want 2.0 (tenant wins)", o.Pricing.InputPerMtok)
	}
	if o, ok := bySelector["openai/gpt-4o-mini"]; !ok {
		t.Fatal("ListEffective(tenant-A) missing openai/gpt-4o-mini")
	} else if o.Pricing.InputPerMtok == nil || *o.Pricing.InputPerMtok != 0.5 {
		t.Fatalf("openai/gpt-4o-mini input = %v, want 0.5 (default)", o.Pricing.InputPerMtok)
	}

	// ListEffective(ctx, "tenant-B"): no overrides, gets only defaults.
	effB, err := store.ListEffective(ctx, "tenant-B")
	if err != nil {
		t.Fatalf("ListEffective(tenant-B) error = %v", err)
	}
	if len(effB) != 2 {
		t.Fatalf("ListEffective(tenant-B) = %d overrides, want 2 (all from default)", len(effB))
	}

	// ListEffective(ctx, "nonexistent"): returns only defaults.
	effNone, err := store.ListEffective(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListEffective(nonexistent) error = %v", err)
	}
	if len(effNone) != 2 {
		t.Fatalf("ListEffective(nonexistent) = %d overrides, want 2 (all from default)", len(effNone))
	}
}

func TestSQLiteStore_DeleteTenantScoped(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	ctx := context.Background()

	// Same selector, two tenants.
	if err := store.Upsert(ctx, "default", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(1.0)},
	}); err != nil {
		t.Fatalf("Upsert(default) error = %v", err)
	}
	if err := store.Upsert(ctx, "tenant-A", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(2.0)},
	}); err != nil {
		t.Fatalf("Upsert(tenant-A) error = %v", err)
	}

	// Delete under tenant-A — default's row is unaffected.
	if err := store.Delete(ctx, "tenant-A", "openai/gpt-4o"); err != nil {
		t.Fatalf("Delete(tenant-A) error = %v", err)
	}

	defaultRows, err := store.List(ctx, "default")
	if err != nil {
		t.Fatalf("List(default) error = %v", err)
	}
	if len(defaultRows) != 1 {
		t.Fatalf("List(default) = %d rows, want 1 (default row still exists)", len(defaultRows))
	}

	aRows, err := store.List(ctx, "tenant-A")
	if err != nil {
		t.Fatalf("List(tenant-A) error = %v", err)
	}
	if len(aRows) != 0 {
		t.Fatalf("List(tenant-A) = %d rows, want 0 (deleted)", len(aRows))
	}
}
