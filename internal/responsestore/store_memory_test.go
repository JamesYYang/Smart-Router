package responsestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"smartrouter/internal/core"
)

func storedResponse(id string, storedAt time.Time) *StoredResponse {
	return &StoredResponse{
		Response: &core.ResponsesResponse{ID: id, Object: "response"},
		StoredAt: storedAt,
	}
}

func TestMemoryStoreExpiresResponses(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithTTL(time.Second))

	err := store.Create(ctx, "", &StoredResponse{
		Response: &core.ResponsesResponse{ID: "resp_old", Object: "response"},
		StoredAt: time.Now().UTC().Add(-2 * time.Second),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := store.Get(ctx, "", "resp_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreMaxEntriesEvictsOldest(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithTTL(0), WithMaxEntries(2))
	now := time.Now().UTC()

	for _, response := range []*StoredResponse{
		{Response: &core.ResponsesResponse{ID: "resp_1", Object: "response"}, StoredAt: now.Add(-3 * time.Second)},
		{Response: &core.ResponsesResponse{ID: "resp_2", Object: "response"}, StoredAt: now.Add(-2 * time.Second)},
		{Response: &core.ResponsesResponse{ID: "resp_3", Object: "response"}, StoredAt: now.Add(-1 * time.Second)},
	} {
		if err := store.Create(ctx, "", response); err != nil {
			t.Fatalf("Create(%s) error = %v", response.Response.ID, err)
		}
	}

	if _, err := store.Get(ctx, "", "resp_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(resp_1) error = %v, want ErrNotFound", err)
	}
	for _, id := range []string{"resp_2", "resp_3"} {
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

func TestMemoryStoreCleanupExpiredRunsPeriodically(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore(WithTTL(time.Second))
	store.items["resp_expired"] = &StoredResponse{
		Response:  &core.ResponsesResponse{ID: "resp_expired", Object: "response"},
		StoredAt:  now.Add(-2 * time.Second),
		ExpiresAt: now.Add(-time.Second),
	}
	store.lastCleanup = now

	store.cleanupExpiredLocked(now.Add(time.Second / 2))
	if _, ok := store.items["resp_expired"]; !ok {
		t.Fatal("expired response removed before cleanup interval elapsed")
	}

	store.cleanupExpiredLocked(now.Add(DefaultMemoryStoreCleanupInterval + time.Second))
	if _, ok := store.items["resp_expired"]; ok {
		t.Fatal("expired response retained after cleanup interval elapsed")
	}
}

func TestMemoryStoreAllowsExplicitUnboundedRetention(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(WithUnboundedRetention())

	err := store.Create(ctx, "", &StoredResponse{
		Response: &core.ResponsesResponse{ID: "resp_old", Object: "response"},
		StoredAt: time.Now().UTC().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := store.Get(ctx, "", "resp_old"); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
}

func TestMemoryStoreTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	// Create a response for tenant A.
	resp := storedResponse("resp_iso_1", time.Time{})
	resp.TenantID = "tenant-a"
	if err := store.Create(ctx, "tenant-a", resp); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Tenant B must not see tenant A's response.
	if _, err := store.Get(ctx, "tenant-b", "resp_iso_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(B, resp_iso_1) error = %v, want ErrNotFound", err)
	}

	// Tenant A sees its own response.
	got, err := store.Get(ctx, "tenant-a", "resp_iso_1")
	if err != nil {
		t.Fatalf("Get(A, resp_iso_1) error = %v", err)
	}
	if got.Response.ID != "resp_iso_1" {
		t.Fatalf("id = %q, want resp_iso_1", got.Response.ID)
	}
	if got.TenantID != "tenant-a" {
		t.Fatalf("TenantID = %q, want tenant-a", got.TenantID)
	}
}

func TestMemoryStoreUpdateTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	resp := storedResponse("resp_iso_2", time.Time{})
	if err := store.Create(ctx, "tenant-a", resp); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Update from tenant-b must return ErrNotFound.
	update := storedResponse("resp_iso_2", time.Time{})
	update.Response.Provider = "provider-b"
	if err := store.Update(ctx, "tenant-b", update); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update(B, resp_iso_2) error = %v, want ErrNotFound", err)
	}

	// Update from tenant-a succeeds.
	update.Response.Provider = "provider-a"
	if err := store.Update(ctx, "tenant-a", update); err != nil {
		t.Fatalf("Update(A, resp_iso_2) error = %v", err)
	}

	got, err := store.Get(ctx, "tenant-a", "resp_iso_2")
	if err != nil {
		t.Fatalf("Get(A, resp_iso_2) error = %v", err)
	}
	if got.Response.Provider != "provider-a" {
		t.Fatalf("provider = %q, want provider-a", got.Response.Provider)
	}
}

func TestMemoryStoreDeleteTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	resp := storedResponse("resp_iso_3", time.Time{})
	if err := store.Create(ctx, "tenant-a", resp); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Delete from tenant-b must return ErrNotFound.
	if err := store.Delete(ctx, "tenant-b", "resp_iso_3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(B, resp_iso_3) error = %v, want ErrNotFound", err)
	}

	// Delete from tenant-a succeeds.
	if err := store.Delete(ctx, "tenant-a", "resp_iso_3"); err != nil {
		t.Fatalf("Delete(A, resp_iso_3) error = %v", err)
	}

	// Verify deleted.
	if _, err := store.Get(ctx, "tenant-a", "resp_iso_3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(A, resp_iso_3) after delete error = %v, want ErrNotFound", err)
	}
}
