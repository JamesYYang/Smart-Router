package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTenantIDContext(t *testing.T) {
	ctx := context.Background()
	require.Equal(t, "", GetTenantID(ctx))

	ctx = WithTenantID(ctx, "tenant-xyz")
	require.Equal(t, "tenant-xyz", GetTenantID(ctx))
}

func TestPlatformHostContext(t *testing.T) {
	ctx := context.Background()
	require.False(t, GetPlatformHost(ctx))

	ctx = WithPlatformHost(ctx, true)
	require.True(t, GetPlatformHost(ctx))

	ctx = WithPlatformHost(ctx, false)
	require.False(t, GetPlatformHost(ctx))
}
