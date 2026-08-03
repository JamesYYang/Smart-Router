package tenants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// TenantDisabledError is returned when a tenant is resolved but its status
// is disabled.
type TenantDisabledError struct {
	TenantID  string
	Subdomain string
}

func (e *TenantDisabledError) Error() string {
	return fmt.Sprintf("tenant %q (%s) is disabled", e.Subdomain, e.TenantID)
}

// Service wraps a Store with an in-memory subdomain cache.
type Service struct {
	store        Store
	ttl          time.Duration
	platformHost string

	mu      sync.RWMutex
	entries map[string]cacheEntry // keyed by subdomain
}

type cacheEntry struct {
	tenant    Tenant
	expiresAt time.Time
}

// NewService returns a Service caching resolved tenants for the given TTL.
// ttl <= 0 disables caching (every call hits the store). platformHost, if
// non-empty, is additionally rejected as a subdomain on Create (in addition
// to the fixed reserved set); pass "" to skip that check.
func NewService(store Store, ttl time.Duration, platformHost string) *Service {
	if ttl < 0 {
		ttl = 0
	}
	return &Service{store: store, ttl: ttl, platformHost: strings.ToLower(strings.TrimSpace(platformHost)), entries: make(map[string]cacheEntry)}
}

// ResolveBySubdomain returns the active tenant for the subdomain.
// Returns ErrNotFound when no tenant matches, or *TenantDisabledError when
// the tenant exists but is disabled.
func (s *Service) ResolveBySubdomain(ctx context.Context, subdomain string) (Tenant, error) {
	if cached, ok := s.cacheGet(subdomain); ok {
		if cached.IsDisabled() {
			return cached, &TenantDisabledError{TenantID: cached.ID, Subdomain: cached.Subdomain}
		}
		return cached, nil
	}
	t, err := s.store.GetBySubdomain(ctx, subdomain)
	if err != nil {
		return Tenant{}, err
	}
	s.cacheSet(subdomain, t)
	if t.IsDisabled() {
		return t, &TenantDisabledError{TenantID: t.ID, Subdomain: t.Subdomain}
	}
	return t, nil
}

func (s *Service) GetByID(ctx context.Context, id string) (Tenant, error) {
	return s.store.GetByID(ctx, id)
}

func (s *Service) Create(ctx context.Context, t Tenant) error {
	if IsReservedSubdomainName(t.Subdomain) || strings.EqualFold(strings.TrimSpace(t.Subdomain), s.platformHost) {
		return fmt.Errorf("create tenant %q: %w", t.Subdomain, ErrReservedSubdomain)
	}
	return s.createInStore(ctx, t)
}

// CreateBootstrapTenant creates a tenant bypassing the reserved-subdomain
// guard applied by Create. It exists solely for internal/app's one-time
// startup bootstrap of the "default" sentinel tenant (P1) — the "default"
// subdomain is reserved specifically so that no *other* caller (in
// particular the admin tenant-management API added in a later task) can
// claim it, and the bootstrap path is the one legitimate exception.
//
// Do not call this from the admin API or any other tenant-creation path;
// use Create there so the reserved-subdomain/platform-host guard applies.
func (s *Service) CreateBootstrapTenant(ctx context.Context, t Tenant) error {
	return s.createInStore(ctx, t)
}

func (s *Service) createInStore(ctx context.Context, t Tenant) error {
	if err := s.store.Create(ctx, t); err != nil {
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("create tenant %q: %w", t.Subdomain, ErrSubdomainTaken)
		}
		return err
	}
	return nil
}

func (s *Service) List(ctx context.Context) ([]Tenant, error) {
	return s.store.List(ctx)
}

func (s *Service) UpdateStatus(ctx context.Context, id string, status Status, now time.Time) error {
	if err := s.store.UpdateStatus(ctx, id, status, now); err != nil {
		return err
	}
	s.invalidate(id)
	return nil
}

func (s *Service) Update(ctx context.Context, id, name, plan string) error {
	if err := s.store.Update(ctx, id, name, plan, time.Now().UTC()); err != nil {
		return err
	}
	s.invalidate(id)
	return nil
}

func (s *Service) Close() error { return s.store.Close() }

// isUniqueConstraintErr reports whether err indicates a duplicate-subdomain
// write. PostgreSQLStore/MongoDBStore already translate their backend's
// unique-violation error into ErrSubdomainTaken before returning, so those
// paths are covered by the errors.Is check; SQLiteStore does not translate,
// so this also recognizes its raw unique-constraint error text.
func isUniqueConstraintErr(err error) bool {
	return errors.Is(err, ErrSubdomainTaken) || isSQLiteUniqueConstraintError(err)
}

func (s *Service) cacheGet(subdomain string) (Tenant, bool) {
	if s.ttl == 0 {
		return Tenant{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[subdomain]
	if !ok {
		return Tenant{}, false
	}
	if time.Now().After(e.expiresAt) {
		return Tenant{}, false
	}
	return e.tenant, true
}

func (s *Service) cacheSet(subdomain string, t Tenant) {
	if s.ttl == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[subdomain] = cacheEntry{tenant: t, expiresAt: time.Now().Add(s.ttl)}
}

// invalidate drops cache entries that may reference the given tenant id.
func (s *Service) invalidate(tenantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub, e := range s.entries {
		if e.tenant.ID == tenantID {
			delete(s.entries, sub)
		}
	}
}
