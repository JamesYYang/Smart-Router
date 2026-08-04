package conversationstore

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

func newSQLiteConversationStore(t *testing.T) (*SQLiteStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store, err := NewSQLiteStore(db)
	require.NoError(t, err)
	return store, db
}

func sqliteStoredConversation(id string) *StoredConversation {
	return &StoredConversation{
		Conversation: &core.Conversation{
			ID:       id,
			Object:   core.ConversationObject,
			Metadata: map[string]string{"k": "v"},
		},
		Items: []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello"}`)},
	}
}

func TestSQLiteStoreCreateGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteConversationStore(t)

	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	conv := sqliteStoredConversation("conv_roundtrip")
	conv.StoredAt = now
	conv.ExpiresAt = now.Add(DefaultMemoryStoreTTL)
	conv.UserPath = "/team"
	conv.RequestID = "req_1"

	require.NoError(t, store.Create(ctx, "tenant-a", conv))

	got, err := store.Get(ctx, "tenant-a", "conv_roundtrip")
	require.NoError(t, err)
	require.Equal(t, "conv_roundtrip", got.Conversation.ID)
	require.Equal(t, core.ConversationObject, got.Conversation.Object)
	require.Equal(t, "/team", got.UserPath)
	require.Equal(t, "req_1", got.RequestID)
	require.Equal(t, "tenant-a", got.TenantID)
	require.True(t, got.StoredAt.Equal(now), "StoredAt = %v, want %v", got.StoredAt, now)
	require.True(t, got.ExpiresAt.Equal(now.Add(DefaultMemoryStoreTTL)), "ExpiresAt = %v, want %v", got.ExpiresAt, now.Add(DefaultMemoryStoreTTL))
	require.Len(t, got.Items, 1)
	require.JSONEq(t, `{"role":"user","content":"hello"}`, string(got.Items[0]))
}

func TestSQLiteStoreGetMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteConversationStore(t)

	_, err := store.Get(ctx, "tenant-a", "conv_missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSQLiteStoreCreateDuplicateFails(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteConversationStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredConversation("conv_dup")))
	err := store.Create(ctx, "tenant-a", sqliteStoredConversation("conv_dup"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

// TestSQLiteStoreUpdatePreservesZeroTimestamps exercises the Update path where
// a fresh struct with zero StoredAt/ExpiresAt must keep the persisted
// timestamps (matching MemoryStore semantics).
func TestSQLiteStoreUpdatePreservesZeroTimestamps(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteConversationStore(t)

	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	conv := sqliteStoredConversation("conv_update")
	conv.StoredAt = now
	conv.ExpiresAt = now.Add(DefaultMemoryStoreTTL)
	require.NoError(t, store.Create(ctx, "tenant-a", conv))

	update := sqliteStoredConversation("conv_update")
	update.Conversation.Metadata = map[string]string{"updated": "yes"}
	require.NoError(t, store.Update(ctx, "tenant-a", update))

	got, err := store.Get(ctx, "tenant-a", "conv_update")
	require.NoError(t, err)
	require.Equal(t, "yes", got.Conversation.Metadata["updated"])
	require.True(t, got.StoredAt.Equal(now), "StoredAt = %v, want %v (preserved)", got.StoredAt, now)
	require.True(t, got.ExpiresAt.Equal(now.Add(DefaultMemoryStoreTTL)), "ExpiresAt = %v, want %v (preserved)", got.ExpiresAt, now.Add(DefaultMemoryStoreTTL))

	// Update on a missing conversation returns ErrNotFound.
	require.ErrorIs(t, store.Update(ctx, "tenant-a", sqliteStoredConversation("conv_missing")), ErrNotFound)
}

func TestSQLiteStoreAppendItems(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteConversationStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredConversation("conv_append")))

	require.NoError(t, store.AppendItems(ctx, "tenant-a", "conv_append", []json.RawMessage{
		json.RawMessage(`{"role":"assistant","content":"hi"}`),
		json.RawMessage(`{"role":"user","content":"bye"}`),
	}))
	// An empty append is a no-op.
	require.NoError(t, store.AppendItems(ctx, "tenant-a", "conv_append", nil))

	got, err := store.Get(ctx, "tenant-a", "conv_append")
	require.NoError(t, err)
	require.Len(t, got.Items, 3)
	require.JSONEq(t, `{"role":"user","content":"hello"}`, string(got.Items[0]))
	require.JSONEq(t, `{"role":"assistant","content":"hi"}`, string(got.Items[1]))
	require.JSONEq(t, `{"role":"user","content":"bye"}`, string(got.Items[2]))

	// Append to a missing conversation returns ErrNotFound.
	err = store.AppendItems(ctx, "tenant-a", "conv_missing", []json.RawMessage{json.RawMessage(`{}`)})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSQLiteStoreDelete(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteConversationStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredConversation("conv_del")))
	require.NoError(t, store.Delete(ctx, "tenant-a", "conv_del"))

	_, err := store.Get(ctx, "tenant-a", "conv_del")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, store.Delete(ctx, "tenant-a", "conv_del"), ErrNotFound)
}

func TestSQLiteStoreTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteConversationStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredConversation("conv_iso")))

	_, err := store.Get(ctx, "tenant-b", "conv_iso")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, store.Update(ctx, "tenant-b", sqliteStoredConversation("conv_iso")), ErrNotFound)
	require.ErrorIs(t, store.AppendItems(ctx, "tenant-b", "conv_iso", []json.RawMessage{json.RawMessage(`{}`)}), ErrNotFound)
	require.ErrorIs(t, store.Delete(ctx, "tenant-b", "conv_iso"), ErrNotFound)

	// tenant-a still sees its own conversation.
	got, err := store.Get(ctx, "tenant-a", "conv_iso")
	require.NoError(t, err)
	require.Equal(t, "conv_iso", got.Conversation.ID)
}

// TestSQLiteStoreCreateNormalizesZeroTimestamps verifies Create never persists
// the year-1 epoch for a zero StoredAt (and derives ExpiresAt from the TTL).
func TestSQLiteStoreCreateNormalizesZeroTimestamps(t *testing.T) {
	ctx := context.Background()
	store, _ := newSQLiteConversationStore(t)

	before := time.Now().UTC()
	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredConversation("conv_ts")))
	after := time.Now().UTC()

	got, err := store.Get(ctx, "tenant-a", "conv_ts")
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
	store, _ := newSQLiteConversationStore(t)

	require.NoError(t, store.Create(ctx, "tenant-a", sqliteStoredConversation("conv_fb")))
	require.Error(t, store.Create(ctx, "tenant-a", sqliteStoredConversation("conv_fb")))
	require.NoError(t, store.Update(ctx, "tenant-a", sqliteStoredConversation("conv_fb")))

	got, err := store.Get(ctx, "tenant-a", "conv_fb")
	require.NoError(t, err)
	require.False(t, got.StoredAt.IsZero())
	require.False(t, got.ExpiresAt.IsZero())
}
