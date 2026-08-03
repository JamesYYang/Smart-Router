package guardrails

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// newTestSQLiteStore returns a tenant-aware SQLite-backed store so tenant
// isolation can be asserted. The shared testStore fake ignores tenantID.
func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewSQLiteStore(context.Background(), db)
	require.NoError(t, err)
	return store
}

func TestService_UpsertForTenant_Isolation(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store)
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Definition{
		Name: "g1", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "tenant-a"}),
	}))
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-b", Definition{
		Name: "g1", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "tenant-b"}),
	}))

	gotA, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	var cfgA map[string]any
	require.NoError(t, json.Unmarshal(gotA[0].Config, &cfgA))
	require.Equal(t, "tenant-a", cfgA["content"])

	gotB, err := svc.ListForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	var cfgB map[string]any
	require.NoError(t, json.Unmarshal(gotB[0].Config, &cfgB))
	require.Equal(t, "tenant-b", cfgB["content"])
}

func TestService_UpsertForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store)
	require.NoError(t, err)
	before := svc.List()

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Definition{
		Name: "g1", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "tenant-a"}),
	}))

	require.Equal(t, before, svc.List())
	require.Len(t, svc.List(), 0)
}

func TestService_UpsertForTenant_DefaultTenantRefreshesSharedCache(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store)
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "default", Definition{
		Name: "g1", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "shared"}),
	}))

	got := svc.List()
	require.Len(t, got, 1)
	require.Equal(t, "g1", got[0].Name)
}

func TestService_DeleteForTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store)
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Definition{
		Name: "g1", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "tenant-a"}),
	}))

	require.NoError(t, svc.DeleteForTenant(context.Background(), "tenant-a", "g1"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestService_GetForTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store)
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Definition{
		Name: "g1", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "tenant-a"}),
	}))

	got, err := svc.GetForTenant(context.Background(), "tenant-a", "g1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "g1", got.Name)

	missing, err := svc.GetForTenant(context.Background(), "tenant-a", "nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	otherTenant, err := svc.GetForTenant(context.Background(), "tenant-b", "g1")
	require.NoError(t, err)
	require.Nil(t, otherTenant)
}
