package tenants

import (
	"context"
	"time"
)

// Store persists tenants across storage backends.
type Store interface {
	Create(ctx context.Context, tenant Tenant) error
	GetByID(ctx context.Context, id string) (Tenant, error)
	GetBySubdomain(ctx context.Context, subdomain string) (Tenant, error)
	List(ctx context.Context) ([]Tenant, error)
	UpdateStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error
	Close() error
}
