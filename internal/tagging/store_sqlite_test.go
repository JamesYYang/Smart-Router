package tagging

import (
	"context"
	"database/sql"
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

func TestSQLiteStore_TenantIsolation(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	ctx := context.Background()

	// Save different rule sets under two tenants.
	rulesDefault := []Rule{
		{Header: "X-Team", Prefix: "team-"},
	}
	rulesA := []Rule{
		{Header: "X-Cost-Center", Prefix: "cc-"},
	}

	if err := store.SaveRules(ctx, "default", rulesDefault); err != nil {
		t.Fatalf("SaveRules(default) error = %v", err)
	}
	if err := store.SaveRules(ctx, "tenant-A", rulesA); err != nil {
		t.Fatalf("SaveRules(tenant-A) error = %v", err)
	}

	// GetRules for default returns only default's rules.
	gotDefault, err := store.GetRules(ctx, "default")
	if err != nil {
		t.Fatalf("GetRules(default) error = %v", err)
	}
	if len(gotDefault) != 1 {
		t.Fatalf("GetRules(default) = %d rules, want 1", len(gotDefault))
	}
	if gotDefault[0].Header != "X-Team" {
		t.Fatalf("GetRules(default) header = %q, want X-Team", gotDefault[0].Header)
	}

	// GetRules for tenant-A returns only A's rules.
	gotA, err := store.GetRules(ctx, "tenant-A")
	if err != nil {
		t.Fatalf("GetRules(tenant-A) error = %v", err)
	}
	if len(gotA) != 1 {
		t.Fatalf("GetRules(tenant-A) = %d rules, want 1", len(gotA))
	}
	if gotA[0].Header != "X-Cost-Center" {
		t.Fatalf("GetRules(tenant-A) header = %q, want X-Cost-Center", gotA[0].Header)
	}

	// GetRules for nonexistent returns empty.
	gotNone, err := store.GetRules(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetRules(nonexistent) error = %v", err)
	}
	if len(gotNone) != 0 {
		t.Fatalf("GetRules(nonexistent) = %d rules, want 0", len(gotNone))
	}
}

func TestSQLiteStore_ListEffectiveRules(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	ctx := context.Background()

	// Default: X-Team rule.
	if err := store.SaveRules(ctx, "default", []Rule{
		{Header: "X-Team", Prefix: "team-"},
	}); err != nil {
		t.Fatalf("SaveRules(default) error = %v", err)
	}

	// Tenant-A: X-Cost-Center rule (tenant-specific).
	if err := store.SaveRules(ctx, "tenant-A", []Rule{
		{Header: "X-Cost-Center", Prefix: "cc-"},
	}); err != nil {
		t.Fatalf("SaveRules(tenant-A) error = %v", err)
	}

	// ListEffectiveRules for tenant-A: gets both default's X-Team and A's X-Cost-Center.
	effectiveA, err := store.ListEffectiveRules(ctx, "tenant-A")
	if err != nil {
		t.Fatalf("ListEffectiveRules(tenant-A) error = %v", err)
	}
	byHeader := make(map[string]Rule, len(effectiveA))
	for _, r := range effectiveA {
		byHeader[r.Header] = r
	}
	if len(byHeader) != 2 {
		t.Fatalf("ListEffectiveRules(tenant-A) = %d rules, want 2", len(byHeader))
	}
	if _, ok := byHeader["X-Team"]; !ok {
		t.Fatal("ListEffectiveRules(tenant-A) missing X-Team from default")
	}
	if r, ok := byHeader["X-Cost-Center"]; !ok || r.Prefix != "cc-" {
		t.Fatalf("ListEffectiveRules(tenant-A) X-Cost-Center = %+v, want Prefix=cc-", r)
	}

	// ListEffectiveRules for tenant-B: only default's X-Team.
	effectiveB, err := store.ListEffectiveRules(ctx, "tenant-B")
	if err != nil {
		t.Fatalf("ListEffectiveRules(tenant-B) error = %v", err)
	}
	if len(effectiveB) != 1 {
		t.Fatalf("ListEffectiveRules(tenant-B) = %d rules, want 1", len(effectiveB))
	}
	if effectiveB[0].Header != "X-Team" {
		t.Fatalf("ListEffectiveRules(tenant-B) header = %q, want X-Team", effectiveB[0].Header)
	}

	// ListEffectiveRules for nonexistent: only default's X-Team.
	effectiveNone, err := store.ListEffectiveRules(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListEffectiveRules(nonexistent) error = %v", err)
	}
	if len(effectiveNone) != 1 {
		t.Fatalf("ListEffectiveRules(nonexistent) = %d rules, want 1", len(effectiveNone))
	}
}
