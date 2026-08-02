package tenants

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTenantStatusConstants(t *testing.T) {
	require.Equal(t, Status("active"), StatusActive)
	require.Equal(t, Status("disabled"), StatusDisabled)
}

func TestTenantIsDisabled(t *testing.T) {
	now := time.Now().UTC()
	require.True(t, Tenant{Status: StatusDisabled, UpdatedAt: now}.IsDisabled())
	require.False(t, Tenant{Status: StatusActive, UpdatedAt: now}.IsDisabled())
}

func TestErrNotFound(t *testing.T) {
	require.True(t, IsNotFound(ErrNotFound))
	require.False(t, IsNotFound(nil))
}
