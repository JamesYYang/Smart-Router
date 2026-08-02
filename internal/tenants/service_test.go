package tenants

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeStore is a minimal Store stub that counts GetBySubdomain calls.
type fakeStore struct {
	gets    atomic.Int64
	tenant  Tenant
	getErr  error
	created []Tenant
}

func (f *fakeStore) Create(_ context.Context, t Tenant) error {
	f.created = append(f.created, t)
	f.tenant = t
	return nil
}
func (f *fakeStore) GetByID(_ context.Context, _ string) (Tenant, error) {
	return f.tenant, f.getErr
}
func (f *fakeStore) GetBySubdomain(_ context.Context, _ string) (Tenant, error) {
	f.gets.Add(1)
	if f.getErr != nil {
		return Tenant{}, f.getErr
	}
	return f.tenant, nil
}
func (f *fakeStore) List(_ context.Context) ([]Tenant, error) { return []Tenant{f.tenant}, nil }
func (f *fakeStore) UpdateStatus(_ context.Context, _ string, _ Status, _ time.Time) error {
	return nil
}
func (f *fakeStore) Close() error { return nil }

func TestService_ResolveBySubdomain_CacheHit(t *testing.T) {
	store := &fakeStore{tenant: Tenant{ID: "t-1", Subdomain: "xyz", Status: StatusActive}}
	svc := NewService(store, time.Minute)

	t1, err := svc.ResolveBySubdomain(context.Background(), "xyz")
	require.NoError(t, err)
	require.Equal(t, "t-1", t1.ID)

	t2, err := svc.ResolveBySubdomain(context.Background(), "xyz")
	require.NoError(t, err)
	require.Equal(t, "t-1", t2.ID)

	// 第二次应命中缓存,store 只被查询一次
	require.Equal(t, int64(1), store.gets.Load())
}

func TestService_ResolveBySubdomain_NotFound(t *testing.T) {
	store := &fakeStore{getErr: ErrNotFound}
	svc := NewService(store, time.Minute)

	_, err := svc.ResolveBySubdomain(context.Background(), "missing")
	require.True(t, IsNotFound(err))
}

func TestService_ResolveBySubdomain_DisabledTenant(t *testing.T) {
	store := &fakeStore{tenant: Tenant{ID: "t-2", Subdomain: "off", Status: StatusDisabled}}
	svc := NewService(store, time.Minute)

	_, err := svc.ResolveBySubdomain(context.Background(), "off")
	require.Error(t, err)
	var te *TenantDisabledError
	require.True(t, errors.As(err, &te))
}

func TestService_ResolveBySubdomain_TTLExpiry(t *testing.T) {
	store := &fakeStore{tenant: Tenant{ID: "t-3", Subdomain: "exp", Status: StatusActive}}
	svc := NewService(store, 10*time.Millisecond)

	_, _ = svc.ResolveBySubdomain(context.Background(), "exp")
	time.Sleep(20 * time.Millisecond)
	_, _ = svc.ResolveBySubdomain(context.Background(), "exp")

	// TTL 过期后应再次查库
	require.Equal(t, int64(2), store.gets.Load())
}
