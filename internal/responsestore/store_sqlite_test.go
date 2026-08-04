package responsestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"smartrouter/internal/core"
)

func newSQLiteResponseStore(t *testing.T) (*SQLiteStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store, err := NewSQLiteStore(db)
	require.NoError(t, err)
	return store, db
}

func sqliteStoredResponse(id string) *StoredResponse {
	return &StoredResponse{
		Response: &core.ResponsesResponse{
			ID:       id,
			Object:   "response",
			Status:   "completed",
			Model:    "gpt-4o-mini",
			Provider: "openai",
		},
		InputItems: []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello"}`)},
	}
}

func TestSQLiteStoreCreateGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteResponseStore(t)

	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	resp := sqliteStoredResponse("resp_roundtrip")
	resp.StoredAt = now
	resp.ExpiresAt = now.Add(DefaultMemoryStoreTTL)
	resp.Provider = "openai"
	resp.ProviderName = "OpenAI"
	resp.ProviderResponseID = "chatcmpl-123"
	resp.RequestID = "req_1"
	resp.UserPath = "/team"
	resp.WorkflowVersionID = "wf_1"

	require.NoError(t, store.Create(ctx, "tenant-a", resp))

	got, err := store.Get(ctx, "tenant-a", "resp_roundtrip")
	require.NoError(t, err)
	require.Equal(t, "resp_roundtrip", got.Response.ID)
	require.Equal(t, "completed", got.Response.Status)
	require.Equal(t, "openai", got.Provider)
	require.Equal(t, "OpenAI", got.ProviderName)
	require.Equal(t, "chatcmpl-123", got.ProviderResponseID)
	require.Equal(t, "req_1", got.RequestID)
	require.Equal(t, "/team", got.UserPath)
	require.Equal(t, "wf_1", got.WorkflowVersionID)
	require.Equal(t, "tenant-a", got.TenantID)
	require.True(t, got.StoredAt.Equal(now), "StoredAt = %v, want %v", got.StoredAt, now)
	require.True(t, got.ExpiresAt.Equal(now.Add(DefaultMemoryStoreTTL)), "ExpiresAt = %v, want %v", got.ExpiresAt, now.Add(DefaultMemoryStoreTTL))
	require.Len(t, got.InputItems, 1)
	require.JSONEq(t, `{"role":"user","content":"hello"}`, string(got.InputItems[0]))
}

func TestSQLiteStoreGetMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteResponseStore(t)

	_, err := store.Get(ctx, "tenant-a", "resp_missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSQLiteStoreCreateDuplicateFails(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteResponseStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredResponse("resp_dup")))
	err := store.Create(ctx, "tenant-a", sqliteStoredResponse("resp_dup"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

// TestSQLiteStoreUpdatePreservesZeroTimestamps exercises the Update path where
// a fresh struct with zero StoredAt/ExpiresAt must keep the persisted
// timestamps (matching MemoryStore semantics).
func TestSQLiteStoreUpdatePreservesZeroTimestamps(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteResponseStore(t)

	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	resp := sqliteStoredResponse("resp_update")
	resp.StoredAt = now
	resp.ExpiresAt = now.Add(DefaultMemoryStoreTTL)
	require.NoError(t, store.Create(ctx, "tenant-a", resp))

	update := sqliteStoredResponse("resp_update")
	update.Response.Status = "failed"
	require.NoError(t, store.Update(ctx, "tenant-a", update))

	got, err := store.Get(ctx, "tenant-a", "resp_update")
	require.NoError(t, err)
	require.Equal(t, "failed", got.Response.Status)
	require.True(t, got.StoredAt.Equal(now), "StoredAt = %v, want %v (preserved)", got.StoredAt, now)
	require.True(t, got.ExpiresAt.Equal(now.Add(DefaultMemoryStoreTTL)), "ExpiresAt = %v, want %v (preserved)", got.ExpiresAt, now.Add(DefaultMemoryStoreTTL))

	// Update on a missing response returns ErrNotFound.
	require.ErrorIs(t, store.Update(ctx, "tenant-a", sqliteStoredResponse("resp_missing")), ErrNotFound)
}

func TestSQLiteStoreDelete(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteResponseStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredResponse("resp_del")))
	require.NoError(t, store.Delete(ctx, "tenant-a", "resp_del"))

	_, err := store.Get(ctx, "tenant-a", "resp_del")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, store.Delete(ctx, "tenant-a", "resp_del"), ErrNotFound)
}

func TestSQLiteStoreTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteResponseStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredResponse("resp_iso")))

	_, err := store.Get(ctx, "tenant-b", "resp_iso")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, store.Update(ctx, "tenant-b", sqliteStoredResponse("resp_iso")), ErrNotFound)
	require.ErrorIs(t, store.Delete(ctx, "tenant-b", "resp_iso"), ErrNotFound)

	// tenant-a still sees its own response.
	got, err := store.Get(ctx, "tenant-a", "resp_iso")
	require.NoError(t, err)
	require.Equal(t, "resp_iso", got.Response.ID)
}

// TestSQLiteStoreCreateNormalizesZeroTimestamps verifies Create never persists
// the year-1 epoch for a zero StoredAt (and derives ExpiresAt from the TTL).
func TestSQLiteStoreCreateNormalizesZeroTimestamps(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteResponseStore(t)

	before := time.Now().UTC()
	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredResponse("resp_ts")))
	after := time.Now().UTC()

	got, err := store.Get(ctx, "tenant-a", "resp_ts")
	require.NoError(t, err)
	require.False(t, got.StoredAt.IsZero())
	require.False(t, got.ExpiresAt.IsZero())
	require.True(t, got.StoredAt.After(before.Add(-time.Minute)) && got.StoredAt.Before(after.Add(time.Minute)),
		"StoredAt = %v, want ~now", got.StoredAt)
	require.Equal(t, DefaultMemoryStoreTTL, got.ExpiresAt.Sub(got.StoredAt))
}

// TestSQLiteStoreCreateFailThenUpdateFallback mirrors the server's
// storeResponseSnapshot path: Create fails on a duplicate, then the same
// zero-timestamp struct is used for Update, which must preserve timestamps.
func TestSQLiteStoreCreateFailThenUpdateFallback(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteResponseStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredResponse("resp_fb")))
	require.Error(t, store.Create(ctx, "tenant-a", sqliteStoredResponse("resp_fb")))
	require.NoError(t, store.Update(ctx, "tenant-a", sqliteStoredResponse("resp_fb")))

	got, err := store.Get(ctx, "tenant-a", "resp_fb")
	require.NoError(t, err)
	require.False(t, got.StoredAt.IsZero())
	require.False(t, got.ExpiresAt.IsZero())
}
