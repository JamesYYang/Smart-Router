package budget

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"smartrouter/internal/usage"
)

func TestSQLiteStoreReplaceConfigBudgetsRemovesStaleConfigRowsOnly(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}
	resetAt := time.Date(2026, time.April, 25, 9, 0, 0, 0, time.UTC)
	if err := store.UpsertBudgets(ctx, "", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 10, Source: SourceConfig},
		{UserPath: "/team", PeriodSeconds: PeriodWeeklySeconds, Amount: 50, Source: SourceConfig, LastResetAt: &resetAt},
		{UserPath: "/manual", PeriodSeconds: PeriodDailySeconds, Amount: 5, Source: SourceManual},
	}); err != nil {
		t.Fatalf("UpsertBudgets() failed: %v", err)
	}

	if err := store.ReplaceConfigBudgets(ctx, "", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodWeeklySeconds, Amount: 75},
	}); err != nil {
		t.Fatalf("ReplaceConfigBudgets() failed: %v", err)
	}

	got, err := store.ListBudgets(ctx, "")
	if err != nil {
		t.Fatalf("ListBudgets() failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 budgets after replacement, got %d: %+v", len(got), got)
	}
	byKey := make(map[string]Budget, len(got))
	for _, budget := range got {
		byKey[budgetKey(budget.UserPath, budget.PeriodSeconds)] = budget
	}
	if _, ok := byKey[budgetKey("/team", PeriodDailySeconds)]; ok {
		t.Fatal("stale config daily budget was not removed")
	}
	weekly := byKey[budgetKey("/team", PeriodWeeklySeconds)]
	if weekly.Amount != 75 {
		t.Fatalf("weekly amount = %v, want 75", weekly.Amount)
	}
	if weekly.Source != SourceConfig {
		t.Fatalf("weekly source = %q, want config", weekly.Source)
	}
	if weekly.LastResetAt == nil || !weekly.LastResetAt.Equal(resetAt) {
		t.Fatalf("weekly last_reset_at = %v, want %s", weekly.LastResetAt, resetAt)
	}
	if _, ok := byKey[budgetKey("/manual", PeriodDailySeconds)]; !ok {
		t.Fatal("manual budget was removed by config replacement")
	}
}

func TestSQLiteStoreReplaceConfigBudgetsPreservesManualCollision(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}
	if err := store.UpsertBudgets(ctx, "", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 10, Source: SourceManual},
	}); err != nil {
		t.Fatalf("UpsertBudgets() failed: %v", err)
	}

	if err := store.ReplaceConfigBudgets(ctx, "", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 99},
	}); err != nil {
		t.Fatalf("ReplaceConfigBudgets() failed: %v", err)
	}

	got, err := store.ListBudgets(ctx, "")
	if err != nil {
		t.Fatalf("ListBudgets() failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 budget, got %d: %+v", len(got), got)
	}
	if got[0].Source != SourceManual || got[0].Amount != 10 {
		t.Fatalf("manual budget = %+v, want manual amount preserved", got[0])
	}
}

func TestSQLiteStoreSumUsageCostHonorsUserPathBoundaryAndCacheType(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	usageStore, err := usage.NewSQLiteStore(db, 0)
	if err != nil {
		t.Fatalf("NewSQLiteStore() for usage failed: %v", err)
	}
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}

	now := time.Date(2026, time.April, 25, 12, 0, 0, 0, time.UTC)
	entries := []*usage.UsageEntry{
		usageEntryWithCost("team-root", "/team", "", now, 0.25),
		usageEntryWithCost("team-child", "/team/app", "", now, 0.75),
		usageEntryWithCost("sibling", "/team-alpha", "", now, 5),
		usageEntryWithCost("cached", "/team/cache", usage.CacheTypeExact, now, 10),
		usageEntryWithCost("outside-window", "/team/app", "", now.Add(-48*time.Hour), 7),
	}
	if err := usageStore.WriteBatch(ctx, "", entries); err != nil {
		t.Fatalf("WriteBatch() failed: %v", err)
	}

	got, hasUsage, err := store.SumUsageCost(ctx, "", "/team", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SumUsageCost() failed: %v", err)
	}
	if !hasUsage {
		t.Fatal("SumUsageCost() hasUsage = false, want true")
	}
	if got != 1.0 {
		t.Fatalf("SumUsageCost() = %v, want 1.0", got)
	}

	got, hasUsage, err = store.SumUsageCost(ctx, "", "/missing", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SumUsageCost() for missing path failed: %v", err)
	}
	if hasUsage || got != 0 {
		t.Fatalf("missing path sum = %v/%v, want 0/false", got, hasUsage)
	}
}

func TestSQLiteStoreSumUsageCostAggregatesAcrossUserPaths(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	usageStore, err := usage.NewSQLiteStore(db, 0)
	if err != nil {
		t.Fatalf("NewSQLiteStore() for usage failed: %v", err)
	}
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}

	now := time.Date(2026, time.April, 25, 12, 0, 0, 0, time.UTC)
	entries := []*usage.UsageEntry{
		usageEntryWithCost("team-root", "/team", "", now, 0.25),
		usageEntryWithCost("team-child", "/team/app", "", now, 0.75),
		usageEntryWithCost("sibling", "/team-alpha", "", now, 5),
		usageEntryWithCost("cached", "/team/cache", usage.CacheTypeExact, now, 10),
		usageEntryWithCost("outside-window", "/team/app", "", now.Add(-48*time.Hour), 7),
	}
	if err := usageStore.WriteBatch(ctx, "", entries); err != nil {
		t.Fatalf("WriteBatch() failed: %v", err)
	}

	got, hasUsage, err := store.SumUsageCost(ctx, "", "/*", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SumUsageCost() failed: %v", err)
	}
	if !hasUsage {
		t.Fatal("SumUsageCost() hasUsage = false, want true")
	}
	// 0.25 + 0.75 + 5 = 6.0; cached (10) and outside-window (7) are excluded.
	if got != 6.0 {
		t.Fatalf("SumUsageCost() = %v, want 6.0", got)
	}

	// The bare "*" form normalizes to "/*" and must behave identically.
	got, _, err = store.SumUsageCost(ctx, "", "*", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SumUsageCost() with bare * failed: %v", err)
	}
	if got != 6.0 {
		t.Fatalf("SumUsageCost() with bare * = %v, want 6.0", got)
	}
}

func usageEntryWithCost(id, userPath, cacheType string, ts time.Time, cost float64) *usage.UsageEntry {
	inputCost := cost / 2
	outputCost := cost / 2
	totalCost := cost
	return &usage.UsageEntry{
		ID:           id,
		RequestID:    id,
		ProviderID:   id,
		Timestamp:    ts,
		Model:        "gpt-4",
		Provider:     "test",
		ProviderName: "test",
		Endpoint:     "/v1/chat/completions",
		UserPath:     userPath,
		CacheType:    cacheType,
		InputTokens:  1,
		OutputTokens: 1,
		TotalTokens:  2,
		InputCost:    &inputCost,
		OutputCost:   &outputCost,
		TotalCost:    &totalCost,
	}
}

func TestSQLiteStoreTenantIsolationListBudgets(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}

	// Insert budget for tenant A
	if err := store.UpsertBudgets(ctx, "tenant-a", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 10, Source: SourceConfig},
	}); err != nil {
		t.Fatalf("UpsertBudgets(A) failed: %v", err)
	}

	// Insert budget for tenant B
	if err := store.UpsertBudgets(ctx, "tenant-b", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 20, Source: SourceConfig},
	}); err != nil {
		t.Fatalf("UpsertBudgets(B) failed: %v", err)
	}

	// List for tenant A should only return A's budget
	aBudgets, err := store.ListBudgets(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListBudgets(A) failed: %v", err)
	}
	if len(aBudgets) != 1 {
		t.Fatalf("expected 1 budget for tenant A, got %d: %+v", len(aBudgets), aBudgets)
	}
	if aBudgets[0].Amount != 10 {
		t.Fatalf("tenant A budget amount = %v, want 10", aBudgets[0].Amount)
	}

	// List for tenant B should only return B's budget
	bBudgets, err := store.ListBudgets(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("ListBudgets(B) failed: %v", err)
	}
	if len(bBudgets) != 1 {
		t.Fatalf("expected 1 budget for tenant B, got %d: %+v", len(bBudgets), bBudgets)
	}
	if bBudgets[0].Amount != 20 {
		t.Fatalf("tenant B budget amount = %v, want 20", bBudgets[0].Amount)
	}

	// List without tenantID should return all (unscoped)
	all, err := store.ListBudgets(ctx, "")
	if err != nil {
		t.Fatalf("ListBudgets(unscoped) failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 budgets unscoped, got %d: %+v", len(all), all)
	}
}

func TestSQLiteStoreTenantIsolationDeleteBudget(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}

	// Insert budget for both tenants
	if err := store.UpsertBudgets(ctx, "tenant-a", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 10, Source: SourceConfig},
	}); err != nil {
		t.Fatalf("UpsertBudgets(A) failed: %v", err)
	}
	if err := store.UpsertBudgets(ctx, "tenant-b", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 10, Source: SourceConfig},
	}); err != nil {
		t.Fatalf("UpsertBudgets(B) failed: %v", err)
	}

	// Delete budget for tenant A
	if err := store.DeleteBudget(ctx, "tenant-a", "/team", PeriodDailySeconds); err != nil {
		t.Fatalf("DeleteBudget(A) failed: %v", err)
	}

	// Tenant B's budget should still exist
	bBudgets, err := store.ListBudgets(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("ListBudgets(B) failed: %v", err)
	}
	if len(bBudgets) != 1 {
		t.Fatalf("expected 1 budget for tenant B after A delete, got %d: %+v", len(bBudgets), bBudgets)
	}
}

func TestSQLiteStoreTenantIsolationResetBudget(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}

	// Insert budgets for both tenants
	if err := store.UpsertBudgets(ctx, "tenant-a", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 10, Source: SourceConfig},
	}); err != nil {
		t.Fatalf("UpsertBudgets(A) failed: %v", err)
	}
	if err := store.UpsertBudgets(ctx, "tenant-b", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 10, Source: SourceConfig},
	}); err != nil {
		t.Fatalf("UpsertBudgets(B) failed: %v", err)
	}

	// Reset all budgets for tenant A
	now := time.Now().UTC()
	if err := store.ResetAllBudgets(ctx, "tenant-a", now); err != nil {
		t.Fatalf("ResetAllBudgets(A) failed: %v", err)
	}

	// Tenant B's budget should NOT be reset
	bBudgets, err := store.ListBudgets(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("ListBudgets(B) failed: %v", err)
	}
	if len(bBudgets) != 1 {
		t.Fatalf("expected 1 budget for tenant B after A reset, got %d: %+v", len(bBudgets), bBudgets)
	}
	if bBudgets[0].LastResetAt != nil {
		t.Fatal("tenant B's budget last_reset_at should not be set")
	}
}

func TestSQLiteStoreTenantIsolationReplaceConfigBudgets(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}

	// Insert config budgets for both tenants
	if err := store.ReplaceConfigBudgets(ctx, "tenant-a", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 10},
	}); err != nil {
		t.Fatalf("ReplaceConfigBudgets(A) failed: %v", err)
	}
	if err := store.ReplaceConfigBudgets(ctx, "tenant-b", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodDailySeconds, Amount: 20},
	}); err != nil {
		t.Fatalf("ReplaceConfigBudgets(B) failed: %v", err)
	}

	// Replace config for tenant A with new budgets
	if err := store.ReplaceConfigBudgets(ctx, "tenant-a", []Budget{
		{UserPath: "/team", PeriodSeconds: PeriodWeeklySeconds, Amount: 50},
	}); err != nil {
		t.Fatalf("ReplaceConfigBudgets(A) second call failed: %v", err)
	}

	// Tenant A should now only have the weekly budget
	aBudgets, err := store.ListBudgets(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListBudgets(A) failed: %v", err)
	}
	if len(aBudgets) != 1 {
		t.Fatalf("expected 1 budget for tenant A after replace, got %d: %+v", len(aBudgets), aBudgets)
	}
	if aBudgets[0].PeriodSeconds != PeriodWeeklySeconds || aBudgets[0].Amount != 50 {
		t.Fatalf("tenant A budget = %+v, want weekly/50", aBudgets[0])
	}

	// Tenant B should still have its original daily budget
	bBudgets, err := store.ListBudgets(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("ListBudgets(B) failed: %v", err)
	}
	if len(bBudgets) != 1 {
		t.Fatalf("expected 1 budget for tenant B after A replace, got %d: %+v", len(bBudgets), bBudgets)
	}
	if bBudgets[0].PeriodSeconds != PeriodDailySeconds || bBudgets[0].Amount != 20 {
		t.Fatalf("tenant B budget = %+v, want daily/20", bBudgets[0])
	}
}

func TestSQLiteStoreTenantIsolationSettings(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() failed: %v", err)
	}

	// Save settings for tenant A
	settingsA := DefaultSettings()
	settingsA.DailyResetHour = 7
	if _, err := store.SaveSettings(ctx, "tenant-a", settingsA); err != nil {
		t.Fatalf("SaveSettings(A) failed: %v", err)
	}

	// Save settings for tenant B
	settingsB := DefaultSettings()
	settingsB.DailyResetHour = 14
	if _, err := store.SaveSettings(ctx, "tenant-b", settingsB); err != nil {
		t.Fatalf("SaveSettings(B) failed: %v", err)
	}

	// Get settings for tenant A
	gotA, err := store.GetSettings(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("GetSettings(A) failed: %v", err)
	}
	if gotA.DailyResetHour != 7 {
		t.Fatalf("tenant A DailyResetHour = %d, want 7", gotA.DailyResetHour)
	}

	// Get settings for tenant B
	gotB, err := store.GetSettings(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("GetSettings(B) failed: %v", err)
	}
	if gotB.DailyResetHour != 14 {
		t.Fatalf("tenant B DailyResetHour = %d, want 14", gotB.DailyResetHour)
	}
}
