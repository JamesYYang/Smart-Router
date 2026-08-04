package conversationstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"smartrouter/internal/core"
)

func storedConversation(id string, storedAt time.Time) *StoredConversation {
	return &StoredConversation{
		Conversation: &core.Conversation{
			ID:       id,
			Object:   core.ConversationObject,
			Metadata: map[string]string{},
		},
		StoredAt: storedAt,
	}
}

func TestMemoryStoreCreateGetUpdateDelete(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	if err := store.Create(ctx, "", storedConversation("conv_1", time.Time{})); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := store.Get(ctx, "", "conv_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Conversation.ID != "conv_1" {
		t.Fatalf("id = %q, want conv_1", got.Conversation.ID)
	}

	got.Conversation.Metadata = map[string]string{"k": "v"}
	if err := store.Update(ctx, "", got); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	updated, err := store.Get(ctx, "", "conv_1")
	if err != nil {
		t.Fatalf("Get() after update error = %v", err)
	}
	if updated.Conversation.Metadata["k"] != "v" {
		t.Fatalf("metadata[k] = %q, want v", updated.Conversation.Metadata["k"])
	}

	if err := store.Delete(ctx, "", "conv_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "", "conv_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() after delete error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreCreateRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	if err := store.Create(ctx, "", storedConversation("conv_dup", time.Time{})); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.Create(ctx, "", storedConversation("conv_dup", time.Time{})); err == nil {
		t.Fatal("Create() duplicate error = nil, want error")
	}
}

func TestMemoryStoreUpdateMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	if err := store.Update(ctx, "", storedConversation("conv_missing", time.Time{})); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreDeleteMissingReturnsNotFound(t *testing.T) {
	if err := NewMemoryStore().Delete(context.Background(), "", "conv_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreDeleteExpiredReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithTTL(time.Second))

	if err := store.Create(ctx, "", storedConversation("conv_expired", time.Now().UTC().Add(-2*time.Second))); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.Delete(ctx, "", "conv_expired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreExpiresConversations(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithTTL(time.Second))

	if err := store.Create(ctx, "", storedConversation("conv_old", time.Now().UTC().Add(-2*time.Second))); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := store.Get(ctx, "", "conv_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreMaxEntriesEvictsOldest(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithTTL(0), WithMaxEntries(2))
	now := time.Now().UTC()

	for _, conversation := range []*StoredConversation{
		storedConversation("conv_1", now.Add(-3*time.Second)),
		storedConversation("conv_2", now.Add(-2*time.Second)),
		storedConversation("conv_3", now.Add(-1*time.Second)),
	} {
		if err := store.Create(ctx, "", conversation); err != nil {
			t.Fatalf("Create(%s) error = %v", conversation.Conversation.ID, err)
		}
	}

	if _, err := store.Get(ctx, "", "conv_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(conv_1) error = %v, want ErrNotFound", err)
	}
	for _, id := range []string{"conv_2", "conv_3"} {
		if _, err := store.Get(ctx, "", id); err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
	}
}

func TestMemoryStoreDefaultRetentionIsBounded(t *testing.T) {
	store := NewMemoryStore()

	if store.ttl != DefaultMemoryStoreTTL {
		t.Fatalf("ttl = %s, want %s", store.ttl, DefaultMemoryStoreTTL)
	}
	if store.maxEntries != DefaultMemoryStoreMaxEntries {
		t.Fatalf("maxEntries = %d, want %d", store.maxEntries, DefaultMemoryStoreMaxEntries)
	}
}

func TestMemoryStoreGetReturnsIsolatedCopy(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Create(ctx, "", storedConversation("conv_iso", time.Time{})); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first, err := store.Get(ctx, "", "conv_iso")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	first.Conversation.Metadata["mutated"] = "true"

	second, err := store.Get(ctx, "", "conv_iso")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, mutated := second.Conversation.Metadata["mutated"]; mutated {
		t.Fatal("stored conversation mutated through returned copy")
	}
}

func TestMemoryStoreAppendItems(t *testing.T) {
	store := NewMemoryStore()
	conv := &StoredConversation{
		Conversation: &core.Conversation{ID: "conv_append", Object: "conversation"},
		Items:        []json.RawMessage{json.RawMessage(`{"n":0}`)},
	}
	if err := store.Create(context.Background(), "", conv); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := store.AppendItems(context.Background(), "", "conv_append", []json.RawMessage{json.RawMessage(`{"n":1}`)}); err != nil {
		t.Fatalf("AppendItems() error = %v", err)
	}
	if err := store.AppendItems(context.Background(), "", "conv_append", nil); err != nil {
		t.Fatalf("AppendItems(empty) error = %v", err)
	}
	if err := store.AppendItems(context.Background(), "", "missing", []json.RawMessage{json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AppendItems(missing) error = %v, want ErrNotFound", err)
	}

	got, err := store.Get(context.Background(), "", "conv_append")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(got.Items) != 2 || string(got.Items[1]) != `{"n":1}` {
		t.Fatalf("Items = %v, want initial item plus appended item", got.Items)
	}
}

func TestMemoryStoreAppendItems_ConcurrentAppendsAllSurvive(t *testing.T) {
	store := NewMemoryStore()
	conv := &StoredConversation{Conversation: &core.Conversation{ID: "conv_race", Object: "conversation"}}
	if err := store.Create(context.Background(), "", conv); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	const writers = 20
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			item := json.RawMessage(fmt.Sprintf(`{"writer":%d}`, n))
			if err := store.AppendItems(context.Background(), "", "conv_race", []json.RawMessage{item}); err != nil {
				t.Errorf("AppendItems() error = %v", err)
			}
		}(i)
	}
		wg.Wait()

	got, err := store.Get(context.Background(), "", "conv_race")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(got.Items) != writers {
		t.Fatalf("Items = %d, want %d (no lost appends)", len(got.Items), writers)
	}
	seen := make(map[int]int, writers)
	for _, raw := range got.Items {
		var item struct {
			Writer int `json:"writer"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("unmarshal appended item: %v", err)
		}
		seen[item.Writer]++
	}
	for i := range writers {
		if seen[i] != 1 {
			t.Fatalf("writer %d count = %d, want exactly once (no lost or duplicated appends)", i, seen[i])
		}
	}
}

func TestMemoryStoreTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	// Create conversation for tenant A.
	conv := storedConversation("conv_iso_1", time.Time{})
	conv.TenantID = "tenant-a"
	if err := store.Create(ctx, "tenant-a", conv); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Tenant B must not see tenant A's conversation.
	if _, err := store.Get(ctx, "tenant-b", "conv_iso_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(B, conv_iso_1) error = %v, want ErrNotFound", err)
	}

	// Tenant A sees its own conversation.
	got, err := store.Get(ctx, "tenant-a", "conv_iso_1")
	if err != nil {
		t.Fatalf("Get(A, conv_iso_1) error = %v", err)
	}
	if got.Conversation.ID != "conv_iso_1" {
		t.Fatalf("id = %q, want conv_iso_1", got.Conversation.ID)
	}
	if got.TenantID != "tenant-a" {
		t.Fatalf("TenantID = %q, want tenant-a", got.TenantID)
	}
}

func TestMemoryStoreAppendItemsTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	conv := storedConversation("conv_iso_2", time.Time{})
	if err := store.Create(ctx, "tenant-a", conv); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Appending from tenant-b must return ErrNotFound.
	err := store.AppendItems(ctx, "tenant-b", "conv_iso_2", []json.RawMessage{json.RawMessage(`{}`)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("AppendItems(B, conv_iso_2) error = %v, want ErrNotFound", err)
	}

	// Appending from tenant-a succeeds.
	if err := store.AppendItems(ctx, "tenant-a", "conv_iso_2", []json.RawMessage{json.RawMessage(`{"n":1}`)}); err != nil {
		t.Fatalf("AppendItems(A, conv_iso_2) error = %v", err)
	}

	got, err := store.Get(ctx, "tenant-a", "conv_iso_2")
	if err != nil {
		t.Fatalf("Get(A, conv_iso_2) error = %v", err)
	}
	if len(got.Items) != 1 || string(got.Items[0]) != `{"n":1}` {
		t.Fatalf("Items = %v, want [{\"n\":1}]", got.Items)
	}
}

func TestMemoryStoreUpdateTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	conv := storedConversation("conv_iso_3", time.Time{})
	if err := store.Create(ctx, "tenant-a", conv); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Update from tenant-b must return ErrNotFound.
	update := storedConversation("conv_iso_3", time.Time{})
	if err := store.Update(ctx, "tenant-b", update); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update(B, conv_iso_3) error = %v, want ErrNotFound", err)
	}

	// Update from tenant-a succeeds.
	update.Conversation.Metadata = map[string]string{"k": "v"}
	if err := store.Update(ctx, "tenant-a", update); err != nil {
		t.Fatalf("Update(A, conv_iso_3) error = %v", err)
	}

	got, err := store.Get(ctx, "tenant-a", "conv_iso_3")
	if err != nil {
		t.Fatalf("Get(A, conv_iso_3) error = %v", err)
	}
	if got.Conversation.Metadata["k"] != "v" {
		t.Fatalf("metadata[k] = %q, want v", got.Conversation.Metadata["k"])
	}
}

func TestMemoryStoreDeleteTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	conv := storedConversation("conv_iso_4", time.Time{})
	if err := store.Create(ctx, "tenant-a", conv); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Delete from tenant-b must return ErrNotFound.
	if err := store.Delete(ctx, "tenant-b", "conv_iso_4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(B, conv_iso_4) error = %v, want ErrNotFound", err)
	}

	// Delete from tenant-a succeeds.
	if err := store.Delete(ctx, "tenant-a", "conv_iso_4"); err != nil {
		t.Fatalf("Delete(A, conv_iso_4) error = %v", err)
	}

	// Verify deleted.
	if _, err := store.Get(ctx, "tenant-a", "conv_iso_4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(A, conv_iso_4) after delete error = %v, want ErrNotFound", err)
	}
}
