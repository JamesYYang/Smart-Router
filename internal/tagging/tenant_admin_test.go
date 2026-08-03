package tagging

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_SaveRulesForTenant_Isolation(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	svc := NewService(nil, store)

	_, err := svc.SaveRulesForTenant(context.Background(), "tenant-a", []Rule{{Header: "X-Tag-A", Prefix: "a"}})
	require.NoError(t, err)
	_, err = svc.SaveRulesForTenant(context.Background(), "tenant-b", []Rule{{Header: "X-Tag-B", Prefix: "b"}})
	require.NoError(t, err)

	gotA, err := svc.GetRulesForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, "X-Tag-A", gotA[0].Header)
	require.Equal(t, "a", gotA[0].Prefix)

	gotB, err := svc.GetRulesForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, "X-Tag-B", gotB[0].Header)
	require.Equal(t, "b", gotB[0].Prefix)
}

func TestService_SaveRulesForTenant_DoesNotAffectSharedCacheForNonDefaultTenant(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	svc := NewService(nil, store)
	before := svc.Rules()

	_, err := svc.SaveRulesForTenant(context.Background(), "tenant-a", []Rule{{Header: "X-Tag-A", Prefix: "a"}})
	require.NoError(t, err)

	require.Equal(t, before, svc.Rules())
}

func TestService_SaveRulesForTenant_DefaultTenantRefreshesSharedCache(t *testing.T) {
	store := newSQLiteStoreForTest(t)
	svc := NewService(nil, store)

	_, err := svc.SaveRulesForTenant(context.Background(), "default", []Rule{{Header: "X-Tag-Default", Prefix: "d"}})
	require.NoError(t, err)

	got := svc.Rules()
	require.Len(t, got, 1)
	require.Equal(t, "X-Tag-Default", got[0].Header)
}
