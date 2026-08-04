package budget

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/usage"
)

func skipIfNoPostgres(t *testing.T) string {
	t.Helper()
	url := os.Getenv("SMARTROUTER_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set SMARTROUTER_TEST_POSTGRES_URL to run")
	}
	return url
}

// newTestPool creates a pgxpool backed by the given DSN. The pool is closed
// automatically when the test ends. No project-wide shared helper exists, so
// this mirrors the inline pattern used in internal/tenants/store_postgresql_test.go.
func newTestPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(func() { pool.Close() })
	return pool
}

func TestPostgreSQLStoreSumUsageCostAggregatesAcrossUserPaths(t *testing.T) {
	url := skipIfNoPostgres(t)
	pool := newTestPool(t, url)
	ctx := context.Background()

	// The usage store only creates the "usage" table and seeds rows; the budget
	// PostgreSQLStore is the code under test.
	usageStore, err := usage.NewPostgreSQLStore(pool, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = usageStore.Close() })
	store, err := NewPostgreSQLStore(ctx, pool)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	tenantID := "tenant-pg-aggregate"
	// Drop any rows a previous run left behind so the test is repeatable
	// against a shared test database. Runs before the pool closes (LIFO).
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM usage WHERE tenant_id = $1`, tenantID)
	})
	_, err = pool.Exec(ctx, `DELETE FROM usage WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)

	now := time.Date(2026, time.April, 25, 12, 0, 0, 0, time.UTC)
	entries := []*usage.UsageEntry{
		// PostgreSQL requires a real UUID for the usage id column, unlike
		// SQLite; the shared usageEntryWithCost helper supplies non-UUID ids,
		// so generate fresh UUIDs here.
		usageEntryWithCost(uuid.New().String(), "/team", "", now, 0.25),
		usageEntryWithCost(uuid.New().String(), "/team/app", "", now, 0.75),
		usageEntryWithCost(uuid.New().String(), "/team-alpha", "", now, 5),
		usageEntryWithCost(uuid.New().String(), "/team/cache", usage.CacheTypeExact, now, 10),
		usageEntryWithCost(uuid.New().String(), "/team/app", "", now.Add(-48*time.Hour), 7),
	}
	require.NoError(t, usageStore.WriteBatch(ctx, tenantID, entries))

	start, end := now.Add(-time.Hour), now.Add(time.Hour)

	// Both the bare "*" and the normalized "/*" forms aggregate across ALL user
	// paths for the tenant: 0.25 + 0.75 + 5 = 6.0 (cached and outside-window
	// rows are excluded).
	for _, path := range []string{"*", "/*"} {
		got, hasUsage, err := store.SumUsageCost(ctx, tenantID, path, start, end)
		require.NoError(t, err)
		require.True(t, hasUsage, "hasUsage false for %q", path)
		require.Equal(t, 6.0, got, "aggregate sum for %q", path)
	}

	// A concrete user path is still restricted to that path's subtree
	// (0.25 + 0.75 = 1.0; /team-alpha must not leak in).
	got, hasUsage, err := store.SumUsageCost(ctx, tenantID, "/team", start, end)
	require.NoError(t, err)
	require.True(t, hasUsage)
	require.Equal(t, 1.0, got)

	// A path with no usage in the window returns hasUsage=false, not an error.
	got, hasUsage, err = store.SumUsageCost(ctx, tenantID, "/missing", start, end)
	require.NoError(t, err)
	require.False(t, hasUsage)
	require.Equal(t, 0.0, got)
}
