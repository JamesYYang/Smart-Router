// Package tenants provides tenant entity types and persistence for the
// multi-tenant SaaS layer. A tenant owns a unique subdomain and scopes
// auth keys, usage, budgets, and per-tenant configuration overrides.
package tenants

import (
	"errors"
	"time"
)

// Status is the lifecycle state of a tenant.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// Tenant is a SaaS customer organization identified by a unique subdomain.
type Tenant struct {
	ID        string
	Subdomain string
	Name      string
	Status    Status
	Plan      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsDisabled reports whether the tenant is in the disabled state.
func (t Tenant) IsDisabled() bool { return t.Status == StatusDisabled }

// ErrNotFound is returned by Store/Service when no tenant matches the query.
var ErrNotFound = errors.New("tenant not found")

// IsNotFound reports whether err is a tenant-not-found error.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
