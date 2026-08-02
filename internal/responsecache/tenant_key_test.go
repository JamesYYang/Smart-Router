package responsecache

import (
	"context"
	"testing"

	"smartrouter/internal/core"
)

func TestHashRequest_DifferentTenantsProduceDifferentKeys(t *testing.T) {
	path := "/v1/chat/completions"
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	var plan *core.Workflow

	ctx1 := core.WithTenantID(context.Background(), "tenant-a")
	ctx2 := core.WithTenantID(context.Background(), "tenant-b")

	h1 := hashRequest(ctx1, path, body, plan)
	h2 := hashRequest(ctx2, path, body, plan)

	if h1 == h2 {
		t.Fatal("different tenant IDs should produce different cache keys")
	}

	// Verify tenant "default" behavior: empty tenantID uses "default"
	ctxDefault := context.Background()
	hDefault := hashRequest(ctxDefault, path, body, plan)
	if hDefault == h1 || hDefault == h2 {
		t.Fatal("default tenant (empty) should use \"default\" string, producing a different key than named tenants")
	}
}

func TestHashRequest_EmptyTenantIDBackCompat(t *testing.T) {
	path := "/v1/chat/completions"
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	var plan *core.Workflow

	// Two contexts both with no tenant ID set should produce the same hash
	ctx1 := context.Background()
	ctx2 := context.Background()

	h1 := hashRequest(ctx1, path, body, plan)
	h2 := hashRequest(ctx2, path, body, plan)

	if h1 != h2 {
		t.Fatal("two default (empty tenantID) contexts should produce the same cache key (back-compat)")
	}
}

func TestComputeParamsHash_DifferentTenantsProduceDifferentHashes(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"same prompt"}]}`)
	plan := &core.Workflow{
		Mode:         core.ExecutionModeTranslated,
		ProviderType: "openai",
		Resolution: &core.RequestModelResolution{
			ResolvedSelector: core.ModelSelector{Provider: "openai", Model: "gpt-4"},
		},
	}

	ctx1 := core.WithTenantID(context.Background(), "tenant-a")
	ctx2 := core.WithTenantID(context.Background(), "tenant-b")

	h1 := computeParamsHash(body, "/v1/chat/completions", plan, "", "", ctx1)
	h2 := computeParamsHash(body, "/v1/chat/completions", plan, "", "", ctx2)

	if h1 == h2 {
		t.Fatal("different tenant IDs should produce different params hashes")
	}
}
