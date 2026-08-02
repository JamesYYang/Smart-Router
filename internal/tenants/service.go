package tenants

import (
	"context"
	"fmt"
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
	store Store
	ttl   time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry // keyed by subdomain
}

type cacheEntry struct {
	tenant    Tenant
	expiresAt time.Time
}

// NewService returns a Service caching resolved tenants for the given TTL.
// ttl <= 0 disables caching (every call hits the store).
func NewService(store Store, ttl time.Duration) *Service {
	if ttl < 0 {
		ttl = 0
	}
	return &Service{store: store, ttl: ttl, entries: make(map[string]cacheEntry)}
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
	return s.store.Create(ctx, t)
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

func (s *Service) Close() error { return s.store.Close() }

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
