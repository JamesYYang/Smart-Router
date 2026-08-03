package tenants

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewSQLiteStore(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSQLiteStore_CreateAndGet(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	tenant := Tenant{
		ID:        "t-1",
		Subdomain: "xyz",
		Name:      "XYZ Inc",
		Status:    StatusActive,
		Plan:      "pro",
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, store.Create(ctx, tenant))

	got, err := store.GetBySubdomain(ctx, "xyz")
	require.NoError(t, err)
	require.Equal(t, "t-1", got.ID)
	require.Equal(t, "XYZ Inc", got.Name)
	require.Equal(t, StatusActive, got.Status)
	require.Equal(t, "pro", got.Plan)
}

func TestSQLiteStore_GetBySubdomain_NotFound(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, err := store.GetBySubdomain(context.Background(), "missing")
	require.True(t, IsNotFound(err))
}

func TestSQLiteStore_GetByID(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-2", Subdomain: "abc", Name: "ABC", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	got, err := store.GetByID(ctx, "t-2")
	require.NoError(t, err)
	require.Equal(t, "abc", got.Subdomain)
}

func TestSQLiteStore_List(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-a", Subdomain: "a", Name: "A", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-b", Subdomain: "b", Name: "B", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	list, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestSQLiteStore_UpdateStatus(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-3", Subdomain: "c", Name: "C", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, store.UpdateStatus(ctx, "t-3", StatusDisabled, now.Add(time.Second)))
	got, err := store.GetByID(ctx, "t-3")
	require.NoError(t, err)
	require.True(t, got.IsDisabled())
}

func TestSQLiteStore_UniqueSubdomain(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-4", Subdomain: "dup", Name: "First", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))
	err := store.Create(ctx, Tenant{ID: "t-5", Subdomain: "dup", Name: "Second", Status: StatusActive, CreatedAt: now, UpdatedAt: now})
	require.Error(t, err) // UNIQUE 约束
}

func TestSQLiteStore_Update(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-upd", Subdomain: "upd", Name: "Old", Status: StatusActive, Plan: "free", CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, store.Update(ctx, "t-upd", "New Name", "pro", now.Add(time.Second)))
	got, err := store.GetByID(ctx, "t-upd")
	require.NoError(t, err)
	require.Equal(t, "New Name", got.Name)
	require.Equal(t, "pro", got.Plan)
}

func TestSQLiteStore_Update_NotFound(t *testing.T) {
	store := newTestSQLiteStore(t)
	err := store.Update(context.Background(), "missing", "n", "p", time.Now().UTC())
	require.True(t, IsNotFound(err))
}
