package usage

import (
	"context"

	"smartrouter/internal/core"
)

// PricingResolver resolves pricing metadata for a given model and provider type.
// Implementations should check the registry first and fall back to a reverse-index
// lookup when the model ID in the usage DB differs from the registry key.
// The ctx parameter carries the resolved tenant ID (core.GetTenantID(ctx)) so
// per-tenant pricing override caches can select the calling tenant's snapshot.
type PricingResolver interface {
	ResolvePricing(ctx context.Context, model, providerType string) *core.ModelPricing
}
