package tenants

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
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
// this mirrors the inline pattern used in internal/filestore/store_test.go.
func newTestPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(func() { pool.Close() })
	return pool
}

func TestPostgreSQLStore_CRUD(t *testing.T) {
	url := skipIfNoPostgres(t)
	pool := newTestPool(t, url)
	store, err := NewPostgreSQLStore(pool)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-pg-1", Subdomain: "pgxyz", Name: "PG", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	got, err := store.GetBySubdomain(ctx, "pgxyz")
	require.NoError(t, err)
	require.Equal(t, "t-pg-1", got.ID)
}
