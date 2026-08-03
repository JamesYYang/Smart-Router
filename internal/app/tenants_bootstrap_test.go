package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"smartrouter/internal/tenants"
)

// newTestTenantService returns a tenants.Service backed by a fresh in-memory
// SQLite store, mirroring the real construction path in app.New (a
// non-empty platformHost, matching config's "app" default, so the guard
// under test is actually exercised).
func newTestTenantService(t *testing.T) *tenants.Service {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := tenants.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return tenants.NewService(store, time.Minute, "app")
}

// TestBootstrapDefaultTenant_FreshStore_Succeeds is a regression test: the
// reserved-subdomain guard added to Service.Create (so the admin API can't
// let a real tenant claim "default"/"www"/the platform host) must not also
// block this startup bootstrap of the "default" sentinel tenant itself,
// which every fresh install performs via app.New whenever
// BootstrapDefaultTenant is enabled (the config default).
func TestBootstrapDefaultTenant_FreshStore_Succeeds(t *testing.T) {
	svc := newTestTenantService(t)

	if err := bootstrapDefaultTenant(context.Background(), svc); err != nil {
		t.Fatalf("bootstrapDefaultTenant on fresh store: %v", err)
	}

	got, err := svc.GetByID(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetByID(default): %v", err)
	}
	if got.Subdomain != "default" {
		t.Fatalf("got subdomain %q, want %q", got.Subdomain, "default")
	}
}

// TestBootstrapDefaultTenant_AlreadyExists_NoOp verifies the second call
// (e.g. on a subsequent restart against the same store) is a no-op rather
// than failing with a duplicate-subdomain error.
func TestBootstrapDefaultTenant_AlreadyExists_NoOp(t *testing.T) {
	svc := newTestTenantService(t)

	if err := bootstrapDefaultTenant(context.Background(), svc); err != nil {
		t.Fatalf("first bootstrapDefaultTenant call: %v", err)
	}
	if err := bootstrapDefaultTenant(context.Background(), svc); err != nil {
		t.Fatalf("second bootstrapDefaultTenant call (expected no-op): %v", err)
	}
}

// TestService_Create_StillRejectsDefaultSubdomain_NormalPath verifies the
// bootstrap fix did not weaken the guard for every other caller: a plain
// Service.Create for subdomain "default" (the shape the future admin
// tenant-creation API would use) must still be rejected.
func TestService_Create_StillRejectsDefaultSubdomain_NormalPath(t *testing.T) {
	svc := newTestTenantService(t)
	now := time.Now().UTC()

	err := svc.Create(context.Background(), tenants.Tenant{
		ID:        "not-the-bootstrap-tenant",
		Subdomain: "default",
		Name:      "Should Be Rejected",
		Status:    tenants.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err == nil {
		t.Fatal("expected Create to reject subdomain \"default\", got nil error")
	}
	if !tenants.IsReservedSubdomain(err) {
		t.Fatalf("expected IsReservedSubdomain(err) to be true, got err: %v", err)
	}
}
