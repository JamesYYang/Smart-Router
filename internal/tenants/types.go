// Package tenants provides tenant entity types and persistence for the
// multi-tenant SaaS layer. A tenant owns a unique subdomain and scopes
// auth keys, usage, budgets, and per-tenant configuration overrides.
package tenants

import (
	"errors"
	"strings"
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

// reservedSubdomains lists subdomains no real tenant may claim. The
// configured platform host is checked separately (see Service.Create)
// because it is configurable; this set covers the fixed reservations.
var reservedSubdomains = map[string]struct{}{
	"default": {},
	"www":     {},
}

// ErrReservedSubdomain is returned when a Create call targets a reserved
// subdomain (default, www, or the configured platform host).
var ErrReservedSubdomain = errors.New("subdomain is reserved")

// IsReservedSubdomain reports whether err wraps ErrReservedSubdomain.
func IsReservedSubdomain(err error) bool { return errors.Is(err, ErrReservedSubdomain) }

// IsReservedSubdomainName reports whether subdomain is unconditionally
// reserved (independent of the configured platform host).
func IsReservedSubdomainName(subdomain string) bool {
	_, ok := reservedSubdomains[strings.ToLower(strings.TrimSpace(subdomain))]
	return ok
}

// ErrSubdomainTaken is returned when Create targets a subdomain that
// already belongs to another tenant (translated from the backend's unique
// constraint violation).
var ErrSubdomainTaken = errors.New("subdomain already in use")

// IsSubdomainTaken reports whether err wraps ErrSubdomainTaken.
func IsSubdomainTaken(err error) bool { return errors.Is(err, ErrSubdomainTaken) }
