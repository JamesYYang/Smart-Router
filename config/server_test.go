package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestServerConfig_TenantFields_FromEnv verifies env var overrides for the
// multi-tenant server config fields.
func TestServerConfig_TenantFields_FromEnv(t *testing.T) {
	clearAllConfigEnvVars(t)

	withTempDir(t, func(string string) {
		t.Setenv("SERVER_BASE_DOMAIN", "smart-router.com")
		t.Setenv("SERVER_PLATFORM_HOST", "admin")
		t.Setenv("BOOTSTRAP_DEFAULT_TENANT", "false")

		result, err := Load()
		require.NoError(t, err)
		require.Equal(t, "smart-router.com", result.Config.Server.BaseDomain)
		require.Equal(t, "admin", result.Config.Server.PlatformHost)
		require.False(t, result.Config.Server.BootstrapDefaultTenant)
	})
}

// TestServerConfig_TenantFields_Defaults verifies the default values when no
// env vars or config file are present.
func TestServerConfig_TenantFields_Defaults(t *testing.T) {
	clearAllConfigEnvVars(t)

	withTempDir(t, func(string string) {
		result, err := Load()
		require.NoError(t, err)
		require.Equal(t, "", result.Config.Server.BaseDomain)
		require.Equal(t, "app", result.Config.Server.PlatformHost)
		require.True(t, result.Config.Server.BootstrapDefaultTenant)
	})
}
