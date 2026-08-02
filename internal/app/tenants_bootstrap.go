package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"smartrouter/internal/tenants"
)

// bootstrapDefaultTenant ensures a "default" tenant exists in the store.
// It is a no-op when the tenant already exists (active or disabled); only
// creates a tenant when ResolveBySubdomain returns ErrNotFound.
//
// Service.ResolveBySubdomain is used because Service does not expose a
// passthrough GetBySubdomain. ResolveBySubdomain returns *TenantDisabledError
// for disabled tenants — for the default tenant this means "already exists,
// do not recreate", so we treat it the same as a successful resolution.
func bootstrapDefaultTenant(ctx context.Context, svc *tenants.Service) error {
	if svc == nil {
		return fmt.Errorf("tenant service is required")
	}
	_, err := svc.ResolveBySubdomain(ctx, "default")
	if err == nil {
		return nil // already exists and active
	}
	var disabledErr *tenants.TenantDisabledError
	if errors.As(err, &disabledErr) {
		return nil // exists but disabled; leave as-is
	}
	if !tenants.IsNotFound(err) {
		return fmt.Errorf("resolve default tenant: %w", err)
	}
	now := time.Now().UTC()
	return svc.Create(ctx, tenants.Tenant{
		ID:        "default",
		Subdomain: "default",
		Name:      "Default Tenant",
		Status:    tenants.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
}
