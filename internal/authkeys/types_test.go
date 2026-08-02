package authkeys

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthKey_TenantFields(t *testing.T) {
	now := time.Now().UTC()
	k := AuthKey{
		ID:            "k-1",
		Name:          "n",
		RedactedValue: "sk_gom_...",
		SecretHash:    "h",
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
		TenantID:      "default",
		IsTenantAdmin: true,
	}
	require.Equal(t, "default", k.TenantID)
	require.True(t, k.IsTenantAdmin)
}

func TestCreateInput_TenantFields(t *testing.T) {
	in := CreateInput{Name: "n", TenantID: "default", IsTenantAdmin: true}
	normalized, err := normalizeCreateInput(in)
	require.NoError(t, err)
	require.Equal(t, "default", normalized.TenantID)
	require.True(t, normalized.IsTenantAdmin)
}

func TestNormalizeCreateInput_AdminKeyRequiresTenantID(t *testing.T) {
	_, err := normalizeCreateInput(CreateInput{Name: "n", IsTenantAdmin: true})
	require.Error(t, err)
	require.True(t, IsValidationError(err))
}

func TestNormalizeCreateInput_APIKeyWithoutTenantID(t *testing.T) {
	normalized, err := normalizeCreateInput(CreateInput{Name: "n"})
	require.NoError(t, err)
	require.Equal(t, "", normalized.TenantID)
	require.False(t, normalized.IsTenantAdmin)
}
