package authkeys

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
// automatically when the test ends. Mirrors the inline pattern used in
// internal/tenants/store_postgresql_test.go.
func newTestPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(func() { pool.Close() })
	return pool
}

// newTestPostgreSQLStore constructs a PostgreSQLStore against the
// SMARTROUTER_TEST_POSTGRES_URL database. Skips when unset.
func newTestPostgreSQLStore(t *testing.T) *PostgreSQLStore {
	t.Helper()
	url := skipIfNoPostgres(t)
	pool := newTestPool(t, url)
	store, err := NewPostgreSQLStore(context.Background(), pool)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPostgreSQLStore_CreateWithTenantFields(t *testing.T) {
	store := newTestPostgreSQLStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, "", AuthKey{
		ID:            "k-pg-tenant",
		Name:          "pg admin",
		RedactedValue: "sk_gom_...",
		SecretHash:    "hash-pg-tenant",
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
		TenantID:      "default",
		IsTenantAdmin: true,
	}))

	list, err := store.List(ctx, "")
	require.NoError(t, err)
	var found *AuthKey
	for i := range list {
		if list[i].ID == "k-pg-tenant" {
			found = &list[i]
			break
		}
	}
	require.NotNil(t, found)
	require.Equal(t, "default", found.TenantID)
	require.True(t, found.IsTenantAdmin)
}
