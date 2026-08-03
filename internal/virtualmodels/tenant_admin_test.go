package virtualmodels

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_UpsertForTenant_Isolation(t *testing.T) {
	svc, err := NewService(newSQLiteVMStore(t), fakeCatalog{}, true)
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", VirtualModel{Source: "s1", Targets: []Target{{Model: "m1", Provider: "openai"}}}))
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-b", VirtualModel{Source: "s1", Targets: []Target{{Model: "m2", Provider: "openai"}}}))

	gotA, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, "m1", gotA[0].Targets[0].Model)

	gotB, err := svc.ListForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, "m2", gotB[0].Targets[0].Model)
}

func TestService_UpsertForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	svc, err := NewService(newSQLiteVMStore(t), fakeCatalog{}, true)
	require.NoError(t, err)
	before := svc.List() // 共享缓存(始终代表 "default" 租户)初始为空

	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", VirtualModel{Source: "s1", Targets: []Target{{Model: "m1", Provider: "openai"}}}))

	require.Equal(t, before, svc.List(), "non-default tenant write must not touch the shared cache")
}

func TestService_UpsertForTenant_DefaultTenant_RefreshesSharedCache(t *testing.T) {
	svc, err := NewService(newSQLiteVMStore(t), fakeCatalog{}, true)
	require.NoError(t, err)

	require.NoError(t, svc.UpsertForTenant(context.Background(), "default", VirtualModel{Source: "s1", Targets: []Target{{Model: "m1", Provider: "openai"}}}))

	require.Len(t, svc.List(), 1, "default-tenant write must refresh the shared cache like Upsert does")
}

func TestService_DeleteForTenant(t *testing.T) {
	svc, err := NewService(newSQLiteVMStore(t), fakeCatalog{}, true)
	require.NoError(t, err)
	require.NoError(t, svc.UpsertForTenant(context.Background(), "tenant-a", VirtualModel{Source: "s1", Targets: []Target{{Model: "m1", Provider: "openai"}}}))

	require.NoError(t, svc.DeleteForTenant(context.Background(), "tenant-a", "s1"))
	got, err := svc.ListForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}
