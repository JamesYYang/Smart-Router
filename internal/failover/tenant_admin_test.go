package failover

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"smartrouter/config"
)

func TestService_UpsertForTenant_Isolation(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	svc, err := NewService(store, config.FailoverConfig{})
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Rule{Source: "s1", Targets: []string{"f1"}}))
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-b", Rule{Source: "s1", Targets: []string{"f2"}}))

	gotA, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, []string{"f1"}, gotA[0].Targets)

	gotB, err := svc.ListForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, []string{"f2"}, gotB[0].Targets)
}

func TestService_UpsertForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	svc, err := NewService(store, config.FailoverConfig{})
	require.NoError(t, err)
	before := svc.List()

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Rule{Source: "s1", Targets: []string{"f1"}}))

	require.Equal(t, before, svc.List())
}

func TestService_DeleteForTenant(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	svc, err := NewService(store, config.FailoverConfig{})
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Rule{Source: "s1", Targets: []string{"f1"}}))

	require.NoError(t, svc.DeleteForTenant(context.Background(), "tenant-a", "s1"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestService_DeleteAllForTenant(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	svc, err := NewService(store, config.FailoverConfig{})
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", Rule{Source: "s1", Targets: []string{"f1"}}))

	require.NoError(t, svc.DeleteAllForTenant(context.Background(), "tenant-a"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}
