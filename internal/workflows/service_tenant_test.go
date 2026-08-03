package workflows

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"smartrouter/internal/core"
)

// mapStore is a Store whose rows vary per tenant ID, used to exercise the
// per-tenant compiled-workflow snapshot cache.
type mapStore struct {
	versions map[string][]Version
}

func (s *mapStore) ListActive(_ context.Context, tenantID string) ([]Version, error) {
	return s.versions[tenantID], nil
}

func (s *mapStore) ListEffective(_ context.Context, tenantID string) ([]Version, error) {
	return s.versions[tenantID], nil
}

func (s *mapStore) Get(_ context.Context, _, id string) (*Version, error) {
	for _, versions := range s.versions {
		for _, version := range versions {
			if version.ID == id {
				versionCopy := version
				return &versionCopy, nil
			}
		}
	}
	return nil, ErrNotFound
}

func (s *mapStore) Create(_ context.Context, _ string, _ CreateInput) (*Version, error) {
	return nil, nil
}

func (s *mapStore) EnsureManagedDefaultGlobal(_ context.Context, _ string, _ CreateInput, _ string) (*Version, error) {
	return nil, nil
}

func (s *mapStore) Deactivate(_ context.Context, _, _ string) error {
	return ErrNotFound
}

func (s *mapStore) Close() error { return nil }

func globalVersion(id, name string) Version {
	return Version{
		ID:       id,
		Scope:    Scope{},
		ScopeKey: "global",
		Version:  1,
		Active:   true,
		Name:     name,
		Payload: Payload{
			SchemaVersion: 1,
			Features:      FeatureFlags{Cache: true, Audit: true, Usage: true, Guardrails: false},
		},
	}
}

func providerModelVersion(id, provider, model, name string) Version {
	return Version{
		ID:       id,
		Scope:    Scope{Provider: provider, Model: model},
		ScopeKey: "provider_model:" + provider + ":" + model,
		Version:  1,
		Active:   true,
		Name:     name,
		Payload: Payload{
			SchemaVersion: 1,
			Features:      FeatureFlags{Cache: true, Audit: true, Usage: true, Guardrails: false},
		},
	}
}

func TestServicePerTenantSnapshots_MatchIsolation(t *testing.T) {
	store := &mapStore{
		versions: map[string][]Version{
			"default": {globalVersion("default-global", "default-global")},
			"tenant-a": {
				globalVersion("tenant-a-global", "tenant-a-global"),
				providerModelVersion("tenant-a-openai-gpt5", "openai", "gpt-5", "tenant-a-openai-gpt5"),
			},
			"tenant-b": {
				globalVersion("tenant-b-global", "tenant-b-global"),
			},
		},
	}
	service, err := NewService(store, NewCompiler(nil))
	require.NoError(t, err)

	err = service.RefreshAll(context.Background(), []string{"default", "tenant-a", "tenant-b"})
	require.NoError(t, err)

	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	ctxB := core.WithTenantID(context.Background(), "tenant-b")

	// tenant-a sees its own provider-model workflow.
	policyA, err := service.Match(ctxA, core.NewWorkflowSelector("openai", "gpt-5"))
	require.NoError(t, err)
	require.NotNil(t, policyA)
	require.Equal(t, "tenant-a-openai-gpt5", policyA.VersionID)

	// tenant-b must NOT see tenant-a's compiled workflow; it falls back to its
	// own global workflow.
	policyB, err := service.Match(ctxB, core.NewWorkflowSelector("openai", "gpt-5"))
	require.NoError(t, err)
	require.NotNil(t, policyB)
	require.Equal(t, "tenant-b-global", policyB.VersionID)

	// The default tenant is unaffected by both.
	policyDefault, err := service.Match(context.Background(), core.NewWorkflowSelector("openai", "gpt-5"))
	require.NoError(t, err)
	require.NotNil(t, policyDefault)
	require.Equal(t, "default-global", policyDefault.VersionID)
}

func TestServicePerTenantSnapshots_FallbackToDefaultTenant(t *testing.T) {
	store := &mapStore{
		versions: map[string][]Version{
			"default": {globalVersion("default-global", "default-global")},
			"tenant-a": {
				globalVersion("tenant-a-global", "tenant-a-global"),
			},
		},
	}
	service, err := NewService(store, NewCompiler(nil))
	require.NoError(t, err)

	err = service.RefreshAll(context.Background(), []string{"default", "tenant-a"})
	require.NoError(t, err)

	// Empty tenant ID (dev mode / platform host) falls back to the default
	// tenant's snapshot.
	policy, err := service.Match(context.Background(), core.NewWorkflowSelector("anthropic", "claude-sonnet-4"))
	require.NoError(t, err)
	require.NotNil(t, policy)
	require.Equal(t, "default-global", policy.VersionID)

	// A tenant absent from the map gets the empty snapshot, whose missing
	// global surfaces as the same "missing active global workflow" error the
	// single-tenant service produced before the default global was seeded.
	_, err = service.Match(core.WithTenantID(context.Background(), "tenant-unknown"), core.WorkflowSelector{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing active global workflow")
}

func TestServicePerTenantSnapshots_RefreshAllAndSingleTenantRefresh(t *testing.T) {
	store := &mapStore{
		versions: map[string][]Version{
			"default": {globalVersion("default-global", "default-global")},
			"tenant-a": {
				globalVersion("tenant-a-global", "tenant-a-global"),
			},
			"tenant-b": {
				globalVersion("tenant-b-global", "tenant-b-global"),
			},
		},
	}
	service, err := NewService(store, NewCompiler(nil))
	require.NoError(t, err)

	// RefreshAll seeds every tenant's snapshot in one atomic swap.
	err = service.RefreshAll(context.Background(), []string{"default", "tenant-a", "tenant-b"})
	require.NoError(t, err)

	ctxA := core.WithTenantID(context.Background(), "tenant-a")
	ctxB := core.WithTenantID(context.Background(), "tenant-b")

	// Now add a tenant-a provider-model workflow and single-tenant refresh.
	store.versions["tenant-a"] = append(store.versions["tenant-a"],
		providerModelVersion("tenant-a-openai-gpt5", "openai", "gpt-5", "tenant-a-openai-gpt5"))

	err = service.Refresh(ctxA)
	require.NoError(t, err)

	// tenant-a now matches the new workflow...
	policyA, err := service.Match(ctxA, core.NewWorkflowSelector("openai", "gpt-5"))
	require.NoError(t, err)
	require.Equal(t, "tenant-a-openai-gpt5", policyA.VersionID)

	// ...while tenant-b is untouched by the single-tenant refresh.
	policyB, err := service.Match(ctxB, core.NewWorkflowSelector("openai", "gpt-5"))
	require.NoError(t, err)
	require.Equal(t, "tenant-b-global", policyB.VersionID)

	// RefreshAll with an empty tenant list rebuilds only the platform-default
	// tenant via a full map swap (per the shared cache infrastructure).
	store.versions["default"] = []Version{globalVersion("default-global-v2", "default-global-v2")}
	err = service.RefreshAll(context.Background(), nil)
	require.NoError(t, err)

	policyDefault, err := service.Match(context.Background(), core.WorkflowSelector{})
	require.NoError(t, err)
	require.Equal(t, "default-global-v2", policyDefault.VersionID)

	// The full swap dropped tenant-a from the map; its snapshot is gone, so
	// Match falls back to the empty snapshot (missing global error).
	_, err = service.Match(ctxA, core.NewWorkflowSelector("openai", "gpt-5"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing active global workflow")
}
