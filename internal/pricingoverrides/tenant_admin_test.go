package pricingoverrides

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_UpsertForTenant_Isolation(t *testing.T) {
	svc, err := NewService(newSQLiteStoreForTest(t), testCatalog{providerNames: []string{"openai"}}, nil)
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, svc.UpsertForTenant(ctx, "tenant-a", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(1.5)},
	}))

	gotA, err := svc.ListForTenant(ctx, "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, "openai/gpt-4o", gotA[0].Selector)
	require.Equal(t, "openai", gotA[0].ProviderName)
	require.Equal(t, "gpt-4o", gotA[0].Model)

	gotB, err := svc.ListForTenant(ctx, "tenant-b")
	require.NoError(t, err)
	require.Empty(t, gotB)
}

func TestService_UpsertForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	svc, err := NewService(newSQLiteStoreForTest(t), testCatalog{providerNames: []string{"openai"}}, nil)
	require.NoError(t, err)
	ctx := context.Background()
	before := svc.List()

	require.NoError(t, svc.UpsertForTenant(ctx, "tenant-a", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(1.5)},
	}))

	require.Equal(t, before, svc.List())
}

func TestService_DeleteForTenant(t *testing.T) {
	svc, err := NewService(newSQLiteStoreForTest(t), testCatalog{providerNames: []string{"openai"}}, nil)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, svc.UpsertForTenant(ctx, "tenant-a", Override{
		Selector: "openai/gpt-4o",
		Pricing:  Pricing{InputPerMtok: ptr(1.5)},
	}))

	require.NoError(t, svc.DeleteForTenant(ctx, "tenant-a", "openai/gpt-4o"))
	got, err := svc.ListForTenant(ctx, "tenant-a")
	require.NoError(t, err)
	require.Empty(t, got)
}
